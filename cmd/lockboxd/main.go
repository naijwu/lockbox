// lockboxd is the lockbox daemon. It keeps a serial link to the carrier
// board and exposes the box over a small HTTP API on localhost.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lockbox/internal/api"
	"lockbox/internal/box"
	"lockbox/internal/nano"
)

func main() {
	var (
		listen   = flag.String("listen", envOr("LOCKBOX_LISTEN", "127.0.0.1:7777"), "HTTP listen address")
		portName = flag.String("port", os.Getenv("LOCKBOX_PORT"), "serial device (default: autodetect)")
		baud     = flag.Int("baud", 115200, "serial baud rate (must match firmware)")
		token    = flag.String("token", os.Getenv("LOCKBOX_TOKEN"), "bearer token required on POST routes (empty = none)")
		pulse    = flag.Duration("pulse", 3*time.Second, "default solenoid on-time per unlock (100ms..10s)")
		cooldown = flag.Duration("cooldown", 5*time.Second, "minimum gap between unlock pulses")
		audit    = flag.String("audit", envOr("LOCKBOX_AUDIT", "lockbox-audit.jsonl"), "append-only JSONL audit log (empty = none)")
		mock     = flag.Bool("mock", false, "run without hardware (simulated board)")
		verbose  = flag.Bool("v", false, "log serial traffic")
	)
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("lockboxd ")

	var auditW *os.File
	if *audit != "" {
		f, err := os.OpenFile(*audit, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("open audit log: %v", err)
		}
		auditW = f
		defer f.Close()
	}

	ctrl := box.New(box.Config{
		Pulse:    nano.ClampPulse(*pulse),
		Cooldown: *cooldown,
		Audit:    auditW,
		Logf:     log.Printf,
	})

	var drv nano.Driver
	if *mock {
		log.Printf("MOCK MODE: no hardware, unlocks are simulated")
		drv = nano.NewMock(ctrl.HandleEvent)
	} else {
		cfg := nano.Config{Port: *portName, Baud: *baud, OnEvent: ctrl.HandleEvent}
		if *verbose {
			cfg.Logf = log.Printf
		}
		drv = nano.NewSerial(cfg)
	}
	ctrl.Attach(drv)
	defer drv.Close()

	srv := &http.Server{
		Addr:              *listen,
		Handler:           api.New(ctrl, *token).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("listening on http://%s (pulse=%s cooldown=%s auth=%v)", *listen, *pulse, *cooldown, *token != "")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
