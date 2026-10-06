// Package api exposes the lockbox over HTTP. This is the surface the AI
// program's tool call talks to.
//
//	GET  /            spectator status page (HTML)
//	GET  /healthz     200 always; body says whether the board is connected
//	GET  /status      daemon + board status (JSON)
//	GET  /events?n=50 recent events (JSON)
//	POST /unlock      {"duration_ms":3000,"reason":"...","source":"ai"}  (all optional)
//	POST /lock        abort a running pulse
//	POST /led         {"mode":"thinking"}  idle|thinking|denied|unlock|party|off
//
// POST routes require "Authorization: Bearer <token>" when a token is set.
package api

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lockbox/internal/box"
	"lockbox/internal/nano"
)

//go:embed index.html
var indexHTML []byte

// Server serves the lockbox API.
type Server struct {
	ctrl  *box.Controller
	token string
	mux   *http.ServeMux
}

// New builds a Server. An empty token disables authentication (fine when
// bound to 127.0.0.1 only).
func New(ctrl *box.Controller, token string) *Server {
	s := &Server{ctrl: ctrl, token: token, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /{$}", s.index)
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /status", s.status)
	s.mux.HandleFunc("GET /events", s.events)
	s.mux.HandleFunc("POST /unlock", s.auth(s.unlock))
	s.mux.HandleFunc("POST /lock", s.auth(s.lock))
	s.mux.HandleFunc("POST /led", s.auth(s.led))
	return s
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	if s.token == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Lockbox-Token")
		if h := r.Header.Get("Authorization"); got == "" && strings.HasPrefix(h, "Bearer ") {
			got = strings.TrimPrefix(h, "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "missing or bad token")
			return
		}
		next(w, r)
	}
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	st := s.ctrl.Status(nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "connected": st.Connected})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.ctrl.Status(r.Context()))
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 {
		n = 50
	}
	writeJSON(w, http.StatusOK, s.ctrl.Events(n))
}

type unlockBody struct {
	DurationMs int64  `json:"duration_ms"`
	Reason     string `json:"reason"`
	Source     string `json:"source"`
}

func (s *Server) unlock(w http.ResponseWriter, r *http.Request) {
	var body unlockBody
	if err := decodeOptional(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if body.Source == "" {
		body.Source = "api"
	}
	// Detached from the request: once asked, the unlock should complete even
	// if the caller hangs up.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := s.ctrl.Unlock(ctx, box.UnlockRequest{
		Duration: time.Duration(body.DurationMs) * time.Millisecond,
		Reason:   body.Reason,
		Source:   body.Source,
	})
	if err != nil {
		var ce *box.CooldownError
		switch {
		case errors.As(err, &ce):
			w.Header().Set("Retry-After", strconv.Itoa(int(ce.Remaining.Seconds())+1))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"ok": false, "error": err.Error(), "retry_after_ms": ce.Remaining.Milliseconds(),
			})
		case errors.Is(err, nano.ErrNotConnected):
			writeErr(w, http.StatusServiceUnavailable, err.Error())
		case errors.Is(err, nano.ErrTimeout):
			writeErr(w, http.StatusGatewayTimeout, err.Error())
		default:
			writeErr(w, http.StatusBadGateway, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"unlocked":     true,
		"duration_ms":  res.Duration.Milliseconds(),
		"unlock_count": res.Count,
		"at":           res.At,
	})
}

func (s *Server) lock(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.ctrl.Lock(ctx, "api"); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, nano.ErrNotConnected) {
			code = http.StatusServiceUnavailable
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "locked": true})
}

func (s *Server) led(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode   string `json:"mode"`
		Source string `json:"source"`
	}
	if err := decodeOptional(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if body.Mode == "" {
		body.Mode = r.URL.Query().Get("mode")
	}
	if body.Source == "" {
		body.Source = "api"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := s.ctrl.Signal(ctx, body.Mode, body.Source)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": body.Mode})
	case errors.Is(err, nano.ErrNotConnected):
		// The signal was counted; the light show just has nowhere to go.
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "mode": body.Mode, "warning": err.Error()})
	case strings.Contains(err.Error(), "unknown signal"):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusBadGateway, err.Error())
	}
}

func decodeOptional(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}
