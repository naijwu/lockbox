package nano

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Mock is an in-memory Driver for developing the daemon and the AI side
// without a board attached. It behaves like the firmware: a pulse relocks
// itself and emits the same events.
type Mock struct {
	onEvent func(Event)

	mu       sync.Mutex
	locked   bool
	pulses   uint64
	start    time.Time
	timer    *time.Timer
	mode     string
	closed   bool
	connTime time.Time
}

// NewMock returns a connected mock. onEvent may be nil.
func NewMock(onEvent func(Event)) *Mock {
	if onEvent == nil {
		onEvent = func(Event) {}
	}
	m := &Mock{onEvent: onEvent, locked: true, start: time.Now(), mode: "idle"}
	m.onEvent(Event{Kind: EventBoot, Detail: "HELLO lockbox fw=mock", At: time.Now()})
	m.onEvent(Event{Kind: EventConnected, Detail: "mock", At: time.Now()})
	return m
}

// Connected implements Driver.
func (m *Mock) Connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closed
}

// Port implements Driver.
func (m *Mock) Port() string { return "mock" }

// Unlock implements Driver.
func (m *Mock) Unlock(_ context.Context, d time.Duration) error {
	d = ClampPulse(d)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrNotConnected
	}
	if !m.locked {
		return &ProtocolError{Msg: "BUSY"}
	}
	m.locked = false
	m.pulses++
	m.mode = "unlock"
	m.timer = time.AfterFunc(d, m.relock)
	m.onEvent(Event{Kind: EventUnlocked, Detail: fmt.Sprint(d.Milliseconds()), At: time.Now()})
	return nil
}

func (m *Mock) relock() {
	m.mu.Lock()
	if m.locked || m.closed {
		m.mu.Unlock()
		return
	}
	m.locked = true
	m.mode = "idle"
	m.timer = nil
	m.mu.Unlock()
	m.onEvent(Event{Kind: EventLocked, At: time.Now()})
}

// Lock implements Driver.
func (m *Mock) Lock(context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrNotConnected
	}
	if m.timer != nil {
		m.timer.Stop()
	}
	m.mu.Unlock()
	m.relock()
	return nil
}

// LED implements Driver.
func (m *Mock) LED(_ context.Context, mode string) error {
	if !ValidLEDMode(mode) {
		return &ProtocolError{Msg: "BADMODE"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrNotConnected
	}
	m.mode = mode
	return nil
}

// Status implements Driver.
func (m *Mock) Status(context.Context) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Status{}, ErrNotConnected
	}
	return Status{
		Locked:   m.locked,
		UptimeMs: uint64(time.Since(m.start).Milliseconds()),
		Pulses:   m.pulses,
		Firmware: "mock",
	}, nil
}

// Close implements Driver.
func (m *Mock) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	if m.timer != nil {
		m.timer.Stop()
	}
	m.mu.Unlock()
	m.onEvent(Event{Kind: EventDisconnected, Detail: "mock", At: time.Now()})
	return nil
}
