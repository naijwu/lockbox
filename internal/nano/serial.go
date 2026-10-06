package nano

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

var (
	// ErrNotConnected is returned when no handshaken link is available.
	ErrNotConnected = errors.New("nano: not connected")
	// ErrTimeout is returned when the board does not answer a command in time.
	ErrTimeout = errors.New("nano: command timed out")

	errClosed = errors.New("nano: driver closed")
)

// ProtocolError is returned when the board answers a command with ERR or
// with something the host did not expect.
type ProtocolError struct{ Msg string }

func (e *ProtocolError) Error() string { return "nano: board said: " + e.Msg }

// port is the slice of serial.Port we actually use; tests inject fakes.
type port interface {
	io.ReadWriteCloser
	SetReadTimeout(time.Duration) error
}

// Config configures the Serial driver. Zero values pick sensible defaults.
type Config struct {
	// Port is the serial device path. Empty means autodetect a USB serial
	// adapter (CH340/FTDI/Arduino VIDs first, then anything that looks right).
	Port string
	// Baud defaults to 115200 and must match the firmware.
	Baud int
	// CommandTimeout bounds how long a single command waits for its reply.
	CommandTimeout time.Duration
	// HandshakeTimeout bounds how long to wait for HELLO after opening the
	// port (the Nano resets on DTR and takes ~2 s to come back).
	HandshakeTimeout time.Duration
	// PingInterval sets the keepalive cadence. A failed ping triggers reconnect.
	PingInterval time.Duration
	// OnEvent receives link and board events. Called from the driver's own
	// goroutines; it must not block for long.
	OnEvent func(Event)
	// Logf receives debug output. nil disables it.
	Logf func(format string, args ...any)

	open func(name string, baud int) (port, error) // test hook
}

func (c *Config) defaults() {
	if c.Baud == 0 {
		c.Baud = 115200
	}
	if c.CommandTimeout == 0 {
		c.CommandTimeout = 2 * time.Second
	}
	if c.HandshakeTimeout == 0 {
		c.HandshakeTimeout = 3500 * time.Millisecond
	}
	if c.PingInterval == 0 {
		c.PingInterval = 3 * time.Second
	}
	if c.OnEvent == nil {
		c.OnEvent = func(Event) {}
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.open == nil {
		c.open = openSerialPort
	}
}

func openSerialPort(name string, baud int) (port, error) {
	p, err := serial.Open(name, &serial.Mode{BaudRate: baud})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Serial is the real Driver. It owns a background goroutine that finds the
// board, handshakes, keeps the link alive and reconnects after any failure.
type Serial struct {
	cfg Config

	cmdMu  sync.Mutex // serialises request/response pairs on the wire
	connMu sync.RWMutex
	conn   *conn

	closed    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

type conn struct {
	name string
	p    port

	resp  chan string   // replies to commands (never EVT/HELLO lines)
	hello chan struct{} // closed once HELLO is seen
	done  chan struct{} // closed when the reader goroutine exits

	helloOnce, doneOnce sync.Once
}

// NewSerial starts the driver. It returns immediately; the link comes up in
// the background and is reported through cfg.OnEvent.
func NewSerial(cfg Config) *Serial {
	cfg.defaults()
	s := &Serial{cfg: cfg, closed: make(chan struct{})}
	s.wg.Add(1)
	go s.run()
	return s
}

// Connected implements Driver.
func (s *Serial) Connected() bool { return s.getConn() != nil }

// Port implements Driver.
func (s *Serial) Port() string {
	if c := s.getConn(); c != nil {
		return c.name
	}
	return ""
}

// Unlock implements Driver.
func (s *Serial) Unlock(ctx context.Context, d time.Duration) error {
	d = ClampPulse(d)
	return s.expectOK(ctx, fmt.Sprintf("UNLOCK %d", d.Milliseconds()))
}

// Lock implements Driver.
func (s *Serial) Lock(ctx context.Context) error { return s.expectOK(ctx, "LOCK") }

// LED implements Driver.
func (s *Serial) LED(ctx context.Context, mode string) error {
	if !ValidLEDMode(mode) {
		return fmt.Errorf("nano: unknown LED mode %q", mode)
	}
	return s.expectOK(ctx, "LED "+mode)
}

// Status implements Driver.
func (s *Serial) Status(ctx context.Context) (Status, error) {
	line, err := s.command(ctx, "STATUS")
	if err != nil {
		return Status{}, err
	}
	return parseStatus(line)
}

// Close implements Driver.
func (s *Serial) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	if c := s.getConn(); c != nil {
		_ = c.p.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *Serial) expectOK(ctx context.Context, cmd string) error {
	line, err := s.command(ctx, cmd)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "OK") {
		return &ProtocolError{Msg: line}
	}
	return nil
}

// command sends one line and waits for the board's reply.
func (s *Serial) command(ctx context.Context, cmd string) (string, error) {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	c := s.getConn()
	if c == nil {
		return "", ErrNotConnected
	}
	return s.exchange(ctx, c, cmd, s.cfg.CommandTimeout)
}

// exchange performs a request/response on a specific conn. Callers must hold
// cmdMu unless the conn is not yet published (handshake).
func (s *Serial) exchange(ctx context.Context, c *conn, cmd string, timeout time.Duration) (string, error) {
	// Discard any stale reply from a command that timed out earlier.
	for {
		select {
		case <-c.resp:
			continue
		default:
		}
		break
	}
	s.cfg.Logf("-> %s", cmd)
	if _, err := c.p.Write([]byte(cmd + "\n")); err != nil {
		c.markDone()
		return "", fmt.Errorf("nano: write: %w", err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case line := <-c.resp:
		if strings.HasPrefix(line, "ERR") {
			return line, &ProtocolError{Msg: strings.TrimSpace(strings.TrimPrefix(line, "ERR"))}
		}
		return line, nil
	case <-timer.C:
		return "", ErrTimeout
	case <-c.done:
		return "", ErrNotConnected
	case <-s.closed:
		return "", errClosed
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *Serial) getConn() *conn {
	s.connMu.RLock()
	defer s.connMu.RUnlock()
	return s.conn
}

func (s *Serial) setConn(c *conn) {
	s.connMu.Lock()
	s.conn = c
	s.connMu.Unlock()
}

func (c *conn) markDone() { c.doneOnce.Do(func() { close(c.done) }) }

// run is the connection manager loop.
func (s *Serial) run() {
	defer s.wg.Done()
	backoff := time.Second
	for {
		if s.isClosed() {
			return
		}
		c, err := s.connect()
		if err != nil {
			if errors.Is(err, errClosed) {
				return
			}
			s.cfg.Logf("connect: %v (retry in %s)", err, backoff)
			if !s.sleep(backoff) {
				return
			}
			if backoff < 5*time.Second {
				backoff += time.Second
			}
			continue
		}
		backoff = time.Second
		s.setConn(c)
		s.cfg.OnEvent(Event{Kind: EventConnected, Detail: c.name, At: time.Now()})
		s.keepalive(c)
		s.setConn(nil)
		_ = c.p.Close()
		<-c.done
		s.cfg.OnEvent(Event{Kind: EventDisconnected, Detail: c.name, At: time.Now()})
	}
}

func (s *Serial) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// sleep waits d or until Close. It reports false if the driver was closed.
func (s *Serial) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.closed:
		return false
	}
}

// connect tries each candidate port in turn and returns the first one that
// completes the handshake.
func (s *Serial) connect() (*conn, error) {
	cands := s.candidates()
	if len(cands) == 0 {
		return nil, errors.New("no serial ports found")
	}
	var lastErr error
	for _, name := range cands {
		if s.isClosed() {
			return nil, errClosed
		}
		c, err := s.openAndHandshake(name)
		if err == nil {
			return c, nil
		}
		lastErr = fmt.Errorf("%s: %w", name, err)
		s.cfg.Logf("%v", lastErr)
	}
	return nil, lastErr
}

func (s *Serial) openAndHandshake(name string) (*conn, error) {
	p, err := s.cfg.open(name, s.cfg.Baud)
	if err != nil {
		return nil, err
	}
	_ = p.SetReadTimeout(500 * time.Millisecond)
	c := &conn{
		name:  name,
		p:     p,
		resp:  make(chan string, 4),
		hello: make(chan struct{}),
		done:  make(chan struct{}),
	}
	go s.reader(c)
	if err := s.handshake(c); err != nil {
		_ = p.Close()
		<-c.done
		return nil, err
	}
	return c, nil
}

// handshake waits for the board's HELLO (it resets when the port opens) and
// then confirms the link with PING/PONG. If HELLO never arrives (some
// adapters do not toggle DTR) it probes with PING anyway.
func (s *Serial) handshake(c *conn) error {
	helloTimer := time.NewTimer(s.cfg.HandshakeTimeout)
	defer helloTimer.Stop()
	select {
	case <-c.hello:
	case <-helloTimer.C:
		s.cfg.Logf("%s: no HELLO, probing", c.name)
	case <-c.done:
		return errors.New("port closed during handshake")
	case <-s.closed:
		return errClosed
	}
	var lastErr error
	for i := 0; i < 3; i++ {
		line, err := s.exchange(context.Background(), c, "PING", time.Second)
		if err == nil && line == "PONG" {
			return nil
		}
		if errors.Is(err, errClosed) || errors.Is(err, ErrNotConnected) {
			return err
		}
		lastErr = err
		if lastErr == nil {
			lastErr = &ProtocolError{Msg: line}
		}
	}
	return fmt.Errorf("handshake failed: %w", lastErr)
}

// keepalive pings the board until the link fails or the driver closes.
func (s *Serial) keepalive(c *conn) {
	t := time.NewTicker(s.cfg.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-c.done:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.CommandTimeout)
			line, err := s.command(ctx, "PING")
			cancel()
			if err != nil || line != "PONG" {
				s.cfg.Logf("%s: keepalive failed: %v %q", c.name, err, line)
				return
			}
		}
	}
}

// reader splits the byte stream into lines and routes them.
func (s *Serial) reader(c *conn) {
	defer c.markDone()
	buf := make([]byte, 256)
	line := make([]byte, 0, 128)
	for {
		n, err := c.p.Read(buf)
		for _, b := range buf[:n] {
			switch b {
			case '\n':
				s.handleLine(c, strings.TrimSpace(string(line)))
				line = line[:0]
			case '\r':
			default:
				if len(line) < 512 {
					line = append(line, b)
				}
			}
		}
		if err != nil {
			return
		}
		if s.isClosed() {
			return
		}
	}
}

func (s *Serial) handleLine(c *conn, line string) {
	if line == "" {
		return
	}
	s.cfg.Logf("<- %s", line)
	switch {
	case strings.HasPrefix(line, "HELLO"):
		c.helloOnce.Do(func() { close(c.hello) })
		s.cfg.OnEvent(Event{Kind: EventBoot, Detail: line, At: time.Now()})
	case strings.HasPrefix(line, "EVT "):
		s.cfg.OnEvent(parseEvent(line))
	default:
		select {
		case c.resp <- line:
		default:
			s.cfg.Logf("dropping unsolicited line: %s", line)
		}
	}
}

func parseStatus(line string) (Status, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "STATUS" {
		return Status{}, &ProtocolError{Msg: line}
	}
	var st Status
	for _, f := range fields[1:] {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case "locked":
			st.Locked = v != "0"
		case "uptime":
			st.UptimeMs, _ = strconv.ParseUint(v, 10, 64)
		case "pulses":
			st.Pulses, _ = strconv.ParseUint(v, 10, 64)
		case "fw":
			st.Firmware = v
		}
	}
	return st, nil
}

// USB vendor IDs of the serial bridges found on Nanos and clones.
var knownVIDs = map[string]bool{
	"1A86": true, // WCH CH340 (most Nano clones)
	"0403": true, // FTDI FT232 (original Nano)
	"2341": true, // Arduino
	"2A03": true, // Arduino.org
	"10C4": true, // Silicon Labs CP210x
	"1B4F": true, // SparkFun
}

// candidates returns serial device paths to try, best first.
func (s *Serial) candidates() []string {
	if s.cfg.Port != "" {
		return []string{s.cfg.Port}
	}
	var preferred, other []string
	if ports, err := enumerator.GetDetailedPortsList(); err == nil {
		for _, p := range ports {
			if !p.IsUSB {
				continue
			}
			if runtime.GOOS == "darwin" && strings.HasPrefix(p.Name, "/dev/tty.") {
				continue // use the callout (cu.*) device on macOS
			}
			if knownVIDs[strings.ToUpper(p.VID)] {
				preferred = append(preferred, p.Name)
			} else {
				other = append(other, p.Name)
			}
		}
	}
	if len(preferred)+len(other) == 0 {
		if names, err := serial.GetPortsList(); err == nil {
			for _, n := range names {
				if looksLikeUSBSerial(n) {
					other = append(other, n)
				}
			}
		}
	}
	sort.Strings(preferred)
	sort.Strings(other)
	return append(preferred, other...)
}

func looksLikeUSBSerial(name string) bool {
	if runtime.GOOS == "darwin" && strings.HasPrefix(name, "/dev/tty.") {
		return false
	}
	for _, pat := range []string{"usbserial", "usbmodem", "wchusbserial", "ttyUSB", "ttyACM", "COM"} {
		if strings.Contains(name, pat) {
			return true
		}
	}
	return false
}
