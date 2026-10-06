package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lockbox/internal/box"
	"lockbox/internal/nano"
)

func newTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	ctrl := box.New(box.Config{Pulse: 150 * time.Millisecond, Cooldown: 2 * time.Second})
	m := nano.NewMock(ctrl.HandleEvent)
	ctrl.Attach(m)
	ts := httptest.NewServer(New(ctrl, token).Handler())
	t.Cleanup(func() { ts.Close(); _ = m.Close() })
	return ts
}

func call(t *testing.T, ts *httptest.Server, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestUnlockFlowWithToken(t *testing.T) {
	ts := newTestServer(t, "s3cret")

	if code, _ := call(t, ts, "POST", "/unlock", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := call(t, ts, "POST", "/unlock", "wrong", ""); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}
	code, out := call(t, ts, "POST", "/unlock", "s3cret", `{"reason":"please","source":"ai"}`)
	if code != http.StatusOK || out["ok"] != true || out["duration_ms"].(float64) != 150 {
		t.Fatalf("unlock: %d %v", code, out)
	}
	code, out = call(t, ts, "POST", "/unlock", "s3cret", "")
	if code != http.StatusTooManyRequests || out["retry_after_ms"] == nil {
		t.Fatalf("cooldown: %d %v", code, out)
	}
	// GETs are open.
	code, st := call(t, ts, "GET", "/status", "", "")
	if code != http.StatusOK || st["connected"] != true || st["unlocks"].(float64) != 1 {
		t.Fatalf("status: %d %v", code, st)
	}
	if code, _ := call(t, ts, "GET", "/healthz", "", ""); code != http.StatusOK {
		t.Fatalf("healthz: %d", code)
	}
}

func TestLedAndLock(t *testing.T) {
	ts := newTestServer(t, "")
	if code, _ := call(t, ts, "POST", "/led", "", `{"mode":"thinking"}`); code != http.StatusOK {
		t.Fatalf("led thinking: %d", code)
	}
	if code, _ := call(t, ts, "POST", "/led", "", `{"mode":"disco"}`); code != http.StatusBadRequest {
		t.Fatalf("led disco: %d", code)
	}
	if code, _ := call(t, ts, "POST", "/unlock", "", `{"duration_ms":5000}`); code != http.StatusOK {
		t.Fatalf("unlock: %d", code)
	}
	if code, _ := call(t, ts, "POST", "/lock", "", ""); code != http.StatusOK {
		t.Fatalf("lock: %d", code)
	}
	_, st := call(t, ts, "GET", "/status", "", "")
	if st["locked"] != true {
		t.Fatalf("expected locked after /lock, status %v", st)
	}
	if code, _ := call(t, ts, "GET", "/", "", ""); code != http.StatusOK {
		t.Fatalf("index: %d", code)
	}
}

func TestBadJSON(t *testing.T) {
	ts := newTestServer(t, "")
	if code, _ := call(t, ts, "POST", "/unlock", "", `{not json`); code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", code)
	}
}
