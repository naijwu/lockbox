// lockboxctl is a small CLI client for lockboxd. The AI program's tool call
// can shell out to it instead of speaking HTTP directly.
//
//	lockboxctl status
//	lockboxctl unlock [-d 3s] [-reason "..."]
//	lockboxctl lock
//	lockboxctl led <idle|thinking|denied|unlock|party|off>
//	lockboxctl events [-n 50]
//
// Exit codes: 0 ok, 1 error, 2 refused (cooldown), 3 board not connected, 4 unauthorised.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	var (
		addr  string
		token string
	)
	root := flag.NewFlagSet("lockboxctl", flag.ExitOnError)
	root.StringVar(&addr, "addr", envOr("LOCKBOX_ADDR", "http://127.0.0.1:7777"), "daemon base URL")
	root.StringVar(&token, "token", os.Getenv("LOCKBOX_TOKEN"), "bearer token")
	root.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: lockboxctl [-addr URL] [-token T] <status|unlock|lock|led|events> [args]")
		root.PrintDefaults()
	}
	_ = root.Parse(os.Args[1:])
	args := root.Args()
	if len(args) == 0 {
		root.Usage()
		os.Exit(1)
	}
	c := &client{addr: strings.TrimRight(addr, "/"), token: token, http: &http.Client{Timeout: 10 * time.Second}}

	switch args[0] {
	case "status":
		c.do("GET", "/status", nil)
	case "events":
		fs := flag.NewFlagSet("events", flag.ExitOnError)
		n := fs.Int("n", 50, "number of events")
		_ = fs.Parse(args[1:])
		c.do("GET", fmt.Sprintf("/events?n=%d", *n), nil)
	case "unlock":
		fs := flag.NewFlagSet("unlock", flag.ExitOnError)
		d := fs.Duration("d", 0, "pulse duration (default: daemon default)")
		reason := fs.String("reason", "", "why (recorded in audit log)")
		source := fs.String("source", "cli", "who is asking")
		_ = fs.Parse(args[1:])
		body := map[string]any{"reason": *reason, "source": *source}
		if *d > 0 {
			body["duration_ms"] = d.Milliseconds()
		}
		c.do("POST", "/unlock", body)
	case "lock":
		c.do("POST", "/lock", nil)
	case "led":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: lockboxctl led <idle|thinking|denied|unlock|party|off>")
			os.Exit(1)
		}
		c.do("POST", "/led", map[string]any{"mode": args[1], "source": "cli"})
	default:
		root.Usage()
		os.Exit(1)
	}
}

type client struct {
	addr, token string
	http        *http.Client
}

func (c *client) do(method, path string, body any) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.addr+path, rd)
	if err != nil {
		fail(1, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		fail(1, fmt.Errorf("daemon unreachable: %w", err))
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	os.Stdout.Write(bytes.TrimRight(out, "\n"))
	fmt.Println()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted:
		return
	case http.StatusTooManyRequests:
		os.Exit(2)
	case http.StatusServiceUnavailable:
		os.Exit(3)
	case http.StatusUnauthorized:
		os.Exit(4)
	default:
		os.Exit(1)
	}
}

func fail(code int, err error) {
	fmt.Fprintln(os.Stderr, "lockboxctl:", err)
	os.Exit(code)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
