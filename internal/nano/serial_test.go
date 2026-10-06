package nano

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBoard emulates the firmware on the far end of an in-memory pipe.
type fakeBoard struct {
	hostR *io.PipeReader // host reads board output
	hostW *io.PipeWriter // host writes commands
	brdR  *io.PipeReader
	brdW  *io.PipeWriter

	noHello bool
	once    sync.Once
	mu      sync.Mutex
	locked  bool
	pulses  int
	seen    []string
}

func newFakeBoard(noHello bool) *fakeBoard {
	f := &fakeBoard{noHello: noHello, locked: true}
	f.hostR, f.brdW = io.Pipe()
	f.brdR, f.hostW = io.Pipe()
	go f.run()
	return f
}

func (f *fakeBoard) Read(p []byte) (int, error)         { return f.hostR.Read(p) }
func (f *fakeBoard) Write(p []byte) (int, error)        { return f.hostW.Write(p) }
func (f *fakeBoard) SetReadTimeout(time.Duration) error { return nil }
func (f *fakeBoard) Close() error {
	f.once.Do(func() {
		f.hostR.Close()
		f.hostW.Close()
		f.brdR.Close()
		f.brdW.Close()
	})
	return nil
}

func (f *fakeBoard) say(s string) { _, _ = io.WriteString(f.brdW, s+"\r\n") }

func (f *fakeBoard) run() {
	if !f.noHello {
		time.Sleep(30 * time.Millisecond) // "bootloader"
		f.say("HELLO lockbox fw=test")
	}
	sc := bufio.NewScanner(f.brdR)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		f.mu.Lock()
		f.seen = append(f.seen, line)
		f.mu.Unlock()
		switch {
		case line == "PING":
			f.say("PONG")
		case strings.HasPrefix(line, "UNLOCK"):
			ms, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "UNLOCK")))
			f.mu.Lock()
			if !f.locked {
				f.mu.Unlock()
				f.say("ERR BUSY")
				continue
			}
			f.locked = false
			f.pulses++
			f.mu.Unlock()
			f.say(fmt.Sprintf("OK UNLOCK %d", ms))
			f.say(fmt.Sprintf("EVT UNLOCKED %d", ms))
			time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
				f.mu.Lock()
				f.locked = true
				f.mu.Unlock()
				f.say("EVT LOCKED")
			})
		case line == "LOCK":
			f.say("OK LOCK")
		case line == "STATUS":
			f.mu.Lock()
			l := 0
			if f.locked {
				l = 1
			}
			f.say(fmt.Sprintf("STATUS locked=%d uptime=4242 pulses=%d fw=test", l, f.pulses))
			f.mu.Unlock()
		case strings.HasPrefix(line, "LED "):
			f.say("OK " + line)
		default:
			f.say("ERR UNKNOWN " + line)
		}
	}
}

func waitEvent(t *testing.T, ch <-chan Event, kind EventKind) Event {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case e := <-ch:
			if e.Kind == kind {
				return e
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s event", kind)
		}
	}
}

func newTestSerial(t *testing.T, open func(string, int) (port, error)) (*Serial, chan Event) {
	t.Helper()
	events := make(chan Event, 64)
	s := NewSerial(Config{
		Port:             "fake",
		HandshakeTimeout: 300 * time.Millisecond,
		CommandTimeout:   time.Second,
		PingInterval:     200 * time.Millisecond,
		OnEvent:          func(e Event) { events <- e },
		Logf:             t.Logf,
		open:             open,
	})
	t.Cleanup(func() { _ = s.Close() })
	return s, events
}

func TestHandshakeAndCommands(t *testing.T) {
	var fb *fakeBoard
	s, events := newTestSerial(t, func(string, int) (port, error) {
		fb = newFakeBoard(false)
		return fb, nil
	})
	waitEvent(t, events, EventBoot)
	waitEvent(t, events, EventConnected)
	if !s.Connected() || s.Port() != "fake" {
		t.Fatalf("expected connected on fake, got %v %q", s.Connected(), s.Port())
	}
	ctx := context.Background()

	st, err := s.Status(ctx)
	if err != nil || !st.Locked || st.Firmware != "test" || st.UptimeMs != 4242 {
		t.Fatalf("status = %+v, %v", st, err)
	}

	if err := s.Unlock(ctx, 150*time.Millisecond); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	e := waitEvent(t, events, EventUnlocked)
	if e.Detail != "150" {
		t.Fatalf("unlocked detail = %q", e.Detail)
	}
	// Second unlock while the pulse runs is rejected by the board.
	err = s.Unlock(ctx, time.Second)
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Msg != "BUSY" {
		t.Fatalf("expected BUSY protocol error, got %v", err)
	}
	waitEvent(t, events, EventLocked)

	if err := s.LED(ctx, "bogus"); err == nil {
		t.Fatal("expected error for unknown LED mode")
	}
	if err := s.LED(ctx, "thinking"); err != nil {
		t.Fatalf("led: %v", err)
	}
	if _, err := s.command(ctx, "WHAT"); !errors.As(err, &pe) {
		t.Fatalf("expected protocol error for unknown command, got %v", err)
	}

	// Pulse requests are clamped before hitting the wire.
	if err := s.Unlock(ctx, time.Minute); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	fb.mu.Lock()
	last := fb.seen[len(fb.seen)-1]
	fb.mu.Unlock()
	if last != "UNLOCK 10000" {
		t.Fatalf("expected clamped UNLOCK 10000, board saw %q", last)
	}
}

func TestHandshakeWithoutHello(t *testing.T) {
	s, events := newTestSerial(t, func(string, int) (port, error) {
		return newFakeBoard(true), nil
	})
	waitEvent(t, events, EventConnected)
	if _, err := s.Status(context.Background()); err != nil {
		t.Fatalf("status after probe handshake: %v", err)
	}
}

func TestReconnectAfterLinkLoss(t *testing.T) {
	var (
		mu     sync.Mutex
		boards []*fakeBoard
	)
	s, events := newTestSerial(t, func(string, int) (port, error) {
		fb := newFakeBoard(false)
		mu.Lock()
		boards = append(boards, fb)
		mu.Unlock()
		return fb, nil
	})
	waitEvent(t, events, EventConnected)

	// Yank the cable.
	mu.Lock()
	boards[0].Close()
	mu.Unlock()
	waitEvent(t, events, EventDisconnected)
	if s.Connected() {
		t.Fatal("still reports connected after link loss")
	}
	_, err := s.Status(context.Background())
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("expected ErrNotConnected while down, got %v", err)
	}

	waitEvent(t, events, EventConnected)
	mu.Lock()
	n := len(boards)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("expected a second open, got %d", n)
	}
	if _, err := s.Status(context.Background()); err != nil {
		t.Fatalf("status after reconnect: %v", err)
	}
}

func TestOpenFailureRetries(t *testing.T) {
	var calls int
	var mu sync.Mutex
	_, events := newTestSerial(t, func(string, int) (port, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls < 2 {
			return nil, errors.New("device busy")
		}
		return newFakeBoard(false), nil
	})
	waitEvent(t, events, EventConnected)
}

func TestParseStatusAndEvent(t *testing.T) {
	st, err := parseStatus("STATUS locked=0 uptime=99 pulses=7 fw=1.0")
	if err != nil || st.Locked || st.UptimeMs != 99 || st.Pulses != 7 || st.Firmware != "1.0" {
		t.Fatalf("parseStatus = %+v, %v", st, err)
	}
	if _, err := parseStatus("ERR nope"); err == nil {
		t.Fatal("expected error for non-STATUS line")
	}
	e := parseEvent("EVT UNLOCKED 3000")
	if e.Kind != EventUnlocked || e.Detail != "3000" {
		t.Fatalf("parseEvent = %+v", e)
	}
	if parseEvent("EVT OVERRIDE").Kind != EventOverride {
		t.Fatal("override event not parsed")
	}
}

func TestClampPulse(t *testing.T) {
	if ClampPulse(0) != MinPulse || ClampPulse(time.Hour) != MaxPulse || ClampPulse(2*time.Second) != 2*time.Second {
		t.Fatal("ClampPulse bounds wrong")
	}
}
