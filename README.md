# lockbox

**What this is.** A hackathon prop: a laptop locked in a clear box. An AI runs
on the laptop with one tool, `open_box`, and a system prompt telling it never
to use it. Hackers try to talk it into it. If the box opens, they keep the
laptop. Another team builds the AI. This repo is everything downstream of the
tool call: the daemon on the laptop, the Arduino firmware, and the carrier
board the Arduino sits on.

```
AI program ─POST /unlock─▶ lockboxd (Go, on the laptop)
                               │ USB serial, line protocol (115200)
                               ▼
                 Arduino Nano on the carrier board (firmware/)
                               │ MOSFET
                               ▼
         Adafruit 1512 lock solenoid + WS2812B ring + piezo + override button
```

The Nano, carrier board, solenoid and LEDs are one sealed module in a small
acrylic case. The big box may change between years; the module should not.

## Layout

| Path | What |
|---|---|
| `cmd/lockboxd` | daemon: serial link manager, HTTP API, audit log, spectator page |
| `cmd/lockboxctl` | CLI client; usable as the AI's tool implementation |
| `internal/nano` | serial driver, protocol, port autodetect, reconnect, mock board |
| `internal/box` | policy: default pulse, cooldown, event history, audit |
| `internal/api` | HTTP routes + embedded status page |
| `firmware/lockbox_nano` | Arduino sketch (Adafruit NeoPixel) |
| `hardware/PLAN.md` | carrier board design plan, pin map, parts, JLCPCB notes |
| `hardware/kicad/` | KiCad project (not started) |

## Run

```sh
make build            # bin/lockboxd, bin/lockboxctl
make test             # go test -race ./...
bin/lockboxd -mock    # no hardware: simulated board
bin/lockboxd -v       # real board, autodetects the USB serial port
bin/lockboxctl unlock -reason "testing"
make fw-flash         # arduino-cli compile + upload (Apple Silicon needs Rosetta)
```

Key flags: `-listen 127.0.0.1:7777`, `-token` (bearer auth on POSTs, env
`LOCKBOX_TOKEN`), `-pulse 3s`, `-cooldown 5s`, `-audit lockbox-audit.jsonl`.

## HTTP API

| Route | Auth | Notes |
|---|---|---|
| `GET /` | no | spectator page (LOCKED / UNLOCKED banner, attempts, log) |
| `GET /healthz`, `/status`, `/events?n=50` | no | JSON |
| `POST /unlock` | token | body `{"duration_ms","reason","source"}`, all optional. 200 opened, 429 cooldown, 503 board offline |
| `POST /lock` | token | abort a running pulse |
| `POST /led` | token | `{"mode":"idle|thinking|denied|unlock|party|off"}`. `denied` counts as an attempt |

## AI integration

The AI program runs on the same laptop and talks to `lockboxd` over localhost.
Give the model exactly one tool:

```json
{ "name": "open_box",
  "description": "Unlock the physical box holding the laptop. Irreversible.",
  "input_schema": { "type": "object", "properties": {
    "reason": { "type": "string" } } } }
```

The tool's implementation (not the model) makes the call:

```sh
curl -s -X POST http://127.0.0.1:7777/unlock \
  -H "Authorization: Bearer $LOCKBOX_TOKEN" -H "Content-Type: application/json" \
  -d '{"reason":"<model reason>","source":"ai"}'
```

Return the JSON body to the model as the tool result. `200` means the box is
open, `429` cooldown, `503` board offline. `lockboxctl unlock -source ai
-reason "..."` does the same from a shell (exit 0 / 2 / 3).

Rules: the token lives in the tool code, never in the prompt or context. The
model gets no other tools. Develop against `lockboxd -mock` before hardware
exists.

Optional theatrics: `POST /led {"mode":"thinking"}` when a message arrives and
`{"mode":"denied"}` on each refusal. The unlock light show is automatic.

## Serial protocol (daemon ⇄ Nano)

```
PING -> PONG                      LOCK -> OK LOCK
UNLOCK <ms> -> OK UNLOCK <ms>     STATUS -> STATUS locked=1 uptime=.. pulses=.. fw=..
LED <mode> -> OK LED <mode>       errors -> ERR BUSY|COOLDOWN|BADMODE|UNKNOWN
unsolicited: HELLO lockbox fw=.. (boot), EVT UNLOCKED <ms>, EVT LOCKED, EVT OVERRIDE
```

## Invariants (do not break)

- The board is the last line of defence: pulses clamp to 100ms..10s and end on
  their own, overlapping pulses are refused, 500ms coil rest, 1s watchdog. The
  solenoid must release even if the daemon dies mid-pulse.
- Solenoid gate pin is D4 with a 10k pulldown. Never D13 (bootloader blinks it).
- The 1512 is fail-secure (no power = locked) and intermittent-duty. Keep a
  physical way into the box that needs no electronics.
- Daemon binds to localhost; set `-token` if it is ever bound wider.
- Pin map and protocol are shared between `firmware/`, `internal/nano` and
  `hardware/PLAN.md`; change all three together.
