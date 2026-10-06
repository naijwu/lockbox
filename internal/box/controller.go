// Package box holds the lockbox policy: default pulse length, cooldown
// between pulses, event history and the audit log. It sits between the HTTP
// API and the board driver.
package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"lockbox/internal/nano"
)

// Config configures a Controller.
type Config struct {
	// Pulse is the solenoid on-time used when a request does not specify one.
	Pulse time.Duration
	// Cooldown is the minimum gap between the start of two pulses. It
	// protects the solenoid coil from being hammered by a looping tool call.
	Cooldown time.Duration
	// History is how many events to keep in memory for GET /events.
	History int
	// Audit, if non-nil, receives one JSON line per event (append-only log).
	Audit io.Writer
	// Logf receives log output. nil disables it.
	Logf func(format string, args ...any)
}

// Event is one entry in the history / audit log.
type Event struct {
	At     time.Time `json:"at"`
	Type   string    `json:"type"`
	Detail string    `json:"detail,omitempty"`
	Reason string    `json:"reason,omitempty"`
	Source string    `json:"source,omitempty"`
}

// UnlockRequest describes who wants the box open and for how long.
type UnlockRequest struct {
	// Duration of the pulse. Zero means the configured default.
	Duration time.Duration
	// Reason is free text recorded in the audit log (e.g. the model's
	// justification). It has no effect on behaviour.
	Reason string
	// Source identifies the caller (e.g. "ai", "cli", "admin").
	Source string
}

// UnlockResult reports a successful unlock.
type UnlockResult struct {
	Duration time.Duration
	At       time.Time
	Count    int
}

// CooldownError is returned when an unlock is refused because the previous
// pulse started too recently.
type CooldownError struct{ Remaining time.Duration }

func (e *CooldownError) Error() string {
	return fmt.Sprintf("box: cooling down, retry in %s", e.Remaining.Round(time.Millisecond))
}

// Status is the daemon's view of the world, served by GET /status.
type Status struct {
	Connected         bool           `json:"connected"`
	Port              string         `json:"port,omitempty"`
	Locked            bool           `json:"locked"`
	Unlocks           int            `json:"unlocks"`
	LastUnlock        *time.Time     `json:"last_unlock,omitempty"`
	CooldownRemaining int64          `json:"cooldown_remaining_ms"`
	DefaultPulseMs    int64          `json:"default_pulse_ms"`
	Signals           map[string]int `json:"signals"`
	DaemonUptimeS     int64          `json:"daemon_uptime_s"`
	Board             *nano.Status   `json:"board,omitempty"`
}

// Controller implements the lockbox policy on top of a nano.Driver.
type Controller struct {
	cfg   Config
	start time.Time

	opMu sync.Mutex // one unlock at a time, including the cooldown check

	mu         sync.Mutex
	drv        nano.Driver
	locked     bool
	unlocks    int
	lastUnlock time.Time
	signals    map[string]int
	events     []Event

	auditMu sync.Mutex
}

// New creates a Controller. Attach a driver with Attach; its OnEvent callback
// should be the controller's HandleEvent.
func New(cfg Config) *Controller {
	if cfg.Pulse == 0 {
		cfg.Pulse = 3 * time.Second
	}
	if cfg.History == 0 {
		cfg.History = 200
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Controller{
		cfg:     cfg,
		start:   time.Now(),
		locked:  true,
		signals: map[string]int{},
	}
}

// Attach sets the board driver.
func (c *Controller) Attach(drv nano.Driver) {
	c.mu.Lock()
	c.drv = drv
	c.mu.Unlock()
}

// HandleEvent consumes driver events. Pass it as nano.Config.OnEvent.
func (c *Controller) HandleEvent(e nano.Event) {
	switch e.Kind {
	case nano.EventLocked:
		c.mu.Lock()
		c.locked = true
		c.mu.Unlock()
	case nano.EventUnlocked:
		c.mu.Lock()
		c.locked = false
		c.mu.Unlock()
	case nano.EventOverride:
		c.mu.Lock()
		c.unlocks++
		c.lastUnlock = time.Now()
		c.mu.Unlock()
	case nano.EventConnected, nano.EventBoot:
		// A (re)booted board always comes up locked.
		c.mu.Lock()
		c.locked = true
		c.mu.Unlock()
	}
	c.record(Event{At: e.At, Type: string(e.Kind), Detail: e.Detail, Source: "board"})
}

// Unlock opens the box, subject to the cooldown.
func (c *Controller) Unlock(ctx context.Context, req UnlockRequest) (UnlockResult, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()

	d := req.Duration
	if d == 0 {
		d = c.cfg.Pulse
	}
	d = nano.ClampPulse(d)
	src := req.Source
	if src == "" {
		src = "api"
	}
	detail := fmt.Sprintf("%dms", d.Milliseconds())

	c.mu.Lock()
	drv := c.drv
	var remaining time.Duration
	if !c.lastUnlock.IsZero() {
		remaining = c.cfg.Cooldown - time.Since(c.lastUnlock)
	}
	c.mu.Unlock()

	if remaining > 0 {
		c.record(Event{At: time.Now(), Type: "unlock_refused", Detail: "cooldown " + detail, Reason: req.Reason, Source: src})
		return UnlockResult{}, &CooldownError{Remaining: remaining}
	}
	if drv == nil || !drv.Connected() {
		c.record(Event{At: time.Now(), Type: "unlock_failed", Detail: "board not connected " + detail, Reason: req.Reason, Source: src})
		return UnlockResult{}, nano.ErrNotConnected
	}
	if err := drv.Unlock(ctx, d); err != nil {
		c.record(Event{At: time.Now(), Type: "unlock_failed", Detail: err.Error() + " " + detail, Reason: req.Reason, Source: src})
		return UnlockResult{}, err
	}

	now := time.Now()
	c.mu.Lock()
	c.unlocks++
	c.lastUnlock = now
	c.locked = false
	n := c.unlocks
	c.mu.Unlock()
	c.record(Event{At: now, Type: "unlock", Detail: detail, Reason: req.Reason, Source: src})
	c.cfg.Logf("UNLOCK #%d for %s by %s", n, d, src)
	return UnlockResult{Duration: d, At: now, Count: n}, nil
}

// Lock aborts a running pulse.
func (c *Controller) Lock(ctx context.Context, source string) error {
	drv := c.driver()
	if drv == nil || !drv.Connected() {
		return nano.ErrNotConnected
	}
	if err := drv.Lock(ctx); err != nil {
		return err
	}
	c.record(Event{At: time.Now(), Type: "lock", Source: source})
	return nil
}

// Signal switches the board's LED mode and counts it. The AI side uses this
// for theatrics: "thinking" while generating, "denied" on a refusal.
func (c *Controller) Signal(ctx context.Context, mode, source string) error {
	if !nano.ValidLEDMode(mode) {
		return fmt.Errorf("box: unknown signal %q (want one of %v)", mode, nano.LEDModes)
	}
	c.mu.Lock()
	c.signals[mode]++
	c.mu.Unlock()
	c.record(Event{At: time.Now(), Type: "signal", Detail: mode, Source: source})
	drv := c.driver()
	if drv == nil || !drv.Connected() {
		return nano.ErrNotConnected
	}
	return drv.LED(ctx, mode)
}

// Status returns the current state. Board status is best-effort.
func (c *Controller) Status(ctx context.Context) Status {
	c.mu.Lock()
	st := Status{
		Locked:         c.locked,
		Unlocks:        c.unlocks,
		DefaultPulseMs: c.cfg.Pulse.Milliseconds(),
		Signals:        make(map[string]int, len(c.signals)),
		DaemonUptimeS:  int64(time.Since(c.start).Seconds()),
	}
	for k, v := range c.signals {
		st.Signals[k] = v
	}
	if !c.lastUnlock.IsZero() {
		t := c.lastUnlock
		st.LastUnlock = &t
		if rem := c.cfg.Cooldown - time.Since(t); rem > 0 {
			st.CooldownRemaining = rem.Milliseconds()
		}
	}
	drv := c.drv
	c.mu.Unlock()

	if drv != nil && drv.Connected() {
		st.Connected = true
		st.Port = drv.Port()
		if ctx != nil {
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			if bs, err := drv.Status(ctx); err == nil {
				st.Board = &bs
				st.Locked = bs.Locked
			}
		}
	}
	return st
}

// Events returns up to n most recent events, oldest first.
func (c *Controller) Events(n int) []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n <= 0 || n > len(c.events) {
		n = len(c.events)
	}
	out := make([]Event, n)
	copy(out, c.events[len(c.events)-n:])
	return out
}

func (c *Controller) driver() nano.Driver {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.drv
}

func (c *Controller) record(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	c.mu.Lock()
	c.events = append(c.events, e)
	if len(c.events) > c.cfg.History {
		c.events = c.events[len(c.events)-c.cfg.History:]
	}
	c.mu.Unlock()
	c.cfg.Logf("event %s %s %s", e.Type, e.Detail, e.Source)
	if c.cfg.Audit != nil {
		b, err := json.Marshal(e)
		if err != nil {
			return
		}
		c.auditMu.Lock()
		_, _ = c.cfg.Audit.Write(append(b, '\n'))
		c.auditMu.Unlock()
	}
}

// IsCooldown reports whether err is a CooldownError.
func IsCooldown(err error) bool {
	var ce *CooldownError
	return errors.As(err, &ce)
}
