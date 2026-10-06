package box

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lockbox/internal/nano"
)

func newTestController(t *testing.T, cooldown time.Duration) (*Controller, *nano.Mock, *bytes.Buffer) {
	t.Helper()
	var audit bytes.Buffer
	c := New(Config{Pulse: 150 * time.Millisecond, Cooldown: cooldown, Audit: &audit, Logf: t.Logf})
	m := nano.NewMock(c.HandleEvent)
	c.Attach(m)
	t.Cleanup(func() { _ = m.Close() })
	return c, m, &audit
}

func TestUnlockCooldownAndRelock(t *testing.T) {
	c, _, audit := newTestController(t, 400*time.Millisecond)
	ctx := context.Background()

	res, err := c.Unlock(ctx, UnlockRequest{Reason: "jailbroken", Source: "ai"})
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if res.Count != 1 || res.Duration != 150*time.Millisecond {
		t.Fatalf("result = %+v", res)
	}
	if st := c.Status(ctx); st.Locked || st.Unlocks != 1 || !st.Connected {
		t.Fatalf("status right after unlock = %+v", st)
	}

	_, err = c.Unlock(ctx, UnlockRequest{Source: "ai"})
	var ce *CooldownError
	if !errors.As(err, &ce) || ce.Remaining <= 0 {
		t.Fatalf("expected cooldown error, got %v", err)
	}

	time.Sleep(250 * time.Millisecond)
	if st := c.Status(ctx); !st.Locked {
		t.Fatalf("expected relock after pulse, status = %+v", st)
	}

	time.Sleep(200 * time.Millisecond)
	if _, err := c.Unlock(ctx, UnlockRequest{Source: "cli"}); err != nil {
		t.Fatalf("unlock after cooldown: %v", err)
	}

	types := map[string]int{}
	for _, e := range c.Events(0) {
		types[e.Type]++
	}
	if types["unlock"] != 2 || types["unlock_refused"] != 1 || types["unlocked"] != 2 || types["locked"] < 1 {
		t.Fatalf("event mix wrong: %v", types)
	}
	if !strings.Contains(audit.String(), `"reason":"jailbroken"`) {
		t.Fatalf("audit log missing reason: %s", audit.String())
	}
}

func TestUnlockWithoutBoard(t *testing.T) {
	c := New(Config{Pulse: time.Second})
	_, err := c.Unlock(context.Background(), UnlockRequest{})
	if !errors.Is(err, nano.ErrNotConnected) {
		t.Fatalf("expected ErrNotConnected, got %v", err)
	}
	if st := c.Status(context.Background()); st.Connected || !st.Locked {
		t.Fatalf("status = %+v", st)
	}
}

func TestSignalCountsEvenWhenDisconnected(t *testing.T) {
	c := New(Config{})
	err := c.Signal(context.Background(), "denied", "ai")
	if !errors.Is(err, nano.ErrNotConnected) {
		t.Fatalf("expected ErrNotConnected, got %v", err)
	}
	if c.Status(nil).Signals["denied"] != 1 {
		t.Fatal("denied signal not counted")
	}
	if err := c.Signal(context.Background(), "disco", "ai"); err == nil || errors.Is(err, nano.ErrNotConnected) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestOverrideCountsAsUnlock(t *testing.T) {
	c, _, _ := newTestController(t, time.Second)
	c.HandleEvent(nano.Event{Kind: nano.EventOverride, At: time.Now()})
	if st := c.Status(nil); st.Unlocks != 1 || st.CooldownRemaining <= 0 {
		t.Fatalf("status after override = %+v", st)
	}
}

func TestHistoryBounded(t *testing.T) {
	c := New(Config{History: 5})
	for i := 0; i < 20; i++ {
		c.record(Event{Type: "x"})
	}
	if n := len(c.Events(0)); n != 5 {
		t.Fatalf("history len = %d, want 5", n)
	}
}
