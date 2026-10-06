// Package nano talks to the lockbox carrier board (an Arduino Nano) over a
// USB serial link using a small line-oriented text protocol.
//
// Host -> board (one command per line, "\n" terminated):
//
//	PING              -> PONG
//	UNLOCK <ms>       -> OK UNLOCK <ms>      (board energises the solenoid for <ms>, then relocks itself)
//	LOCK              -> OK LOCK            (abort a running pulse)
//	STATUS            -> STATUS locked=1 uptime=<ms> pulses=<n> fw=<ver>
//	LED <mode>        -> OK LED <mode>      (idle|thinking|denied|unlock|party|off)
//
// Board -> host, unsolicited:
//
//	HELLO lockbox fw=<ver>   on boot
//	EVT UNLOCKED <ms>        pulse started
//	EVT LOCKED               pulse finished / aborted
//	EVT OVERRIDE             physical override button was used
//
// Any command the board rejects is answered with "ERR <reason>".
package nano

import (
	"context"
	"strings"
	"time"
)

// Pulse limits. The Adafruit 1512 lock solenoid is rated for intermittent
// duty only (roughly 1-10 s on), so the firmware clamps to the same range
// and the daemon never asks for more.
const (
	MinPulse = 100 * time.Millisecond
	MaxPulse = 10 * time.Second
)

// LEDModes lists the light-show modes the firmware understands.
var LEDModes = []string{"idle", "thinking", "denied", "unlock", "party", "off"}

// ValidLEDMode reports whether mode is a known LED mode.
func ValidLEDMode(mode string) bool {
	for _, m := range LEDModes {
		if m == mode {
			return true
		}
	}
	return false
}

// ClampPulse bounds a requested solenoid pulse to [MinPulse, MaxPulse].
func ClampPulse(d time.Duration) time.Duration {
	if d < MinPulse {
		return MinPulse
	}
	if d > MaxPulse {
		return MaxPulse
	}
	return d
}

// EventKind identifies an unsolicited board event.
type EventKind string

const (
	EventConnected    EventKind = "connected"    // serial link established and handshake done
	EventDisconnected EventKind = "disconnected" // serial link lost
	EventBoot         EventKind = "boot"         // board sent HELLO
	EventUnlocked     EventKind = "unlocked"     // solenoid energised
	EventLocked       EventKind = "locked"       // solenoid released
	EventOverride     EventKind = "override"     // physical override button
)

// Event is something the board (or the link to it) reported on its own.
type Event struct {
	Kind   EventKind
	Detail string
	At     time.Time
}

// Status is the board's answer to STATUS.
type Status struct {
	Locked   bool   `json:"locked"`
	UptimeMs uint64 `json:"uptime_ms"`
	Pulses   uint64 `json:"pulses"`
	Firmware string `json:"firmware"`
}

// Driver is what the rest of the daemon needs from the board. Serial is the
// real implementation; Mock is for running without hardware.
type Driver interface {
	// Connected reports whether a handshaken serial link is up right now.
	Connected() bool
	// Port is the device path of the current link, or "" when disconnected.
	Port() string
	// Unlock energises the solenoid for d (clamped to the protocol limits).
	Unlock(ctx context.Context, d time.Duration) error
	// Lock aborts a running pulse. It is a no-op when already locked.
	Lock(ctx context.Context) error
	// LED switches the light-show mode.
	LED(ctx context.Context, mode string) error
	// Status queries the board.
	Status(ctx context.Context) (Status, error)
	// Close tears the link down and stops reconnecting.
	Close() error
}

// parseEvent turns an "EVT ..." line into an Event.
func parseEvent(line string) Event {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "EVT"))
	word, detail, _ := strings.Cut(rest, " ")
	return Event{
		Kind:   EventKind(strings.ToLower(word)),
		Detail: strings.TrimSpace(detail),
		At:     time.Now(),
	}
}
