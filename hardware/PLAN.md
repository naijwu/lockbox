# Carrier board plan

One sealed module: Nano + carrier board + Adafruit 1512 solenoid + LED ring,
bolted inside a small clear acrylic case. The big box only needs a hole for
the latch and two cables in: 12 V and USB. Boxes come and go; the module stays.

## 1. Electrical architecture

```
12V 2A wall adapter ─ barrel jack ─ fuse ─ reverse-polarity ─┬─ bulk cap 1000µF
                                                             ├─ Nano VIN (onboard LDO -> 5V logic)
                                                             ├─ 5V buck module (3A) ─ WS2812B ring + strip header
                                                             ├─ solenoid ─ low-side MOSFET ─ GND   (D4)
                                                             └─ AUX 12V ─ low-side MOSFET ─ GND    (D5, spare channel)
Laptop USB ─ Nano USB (data + 5V backup so the board enumerates even with 12V unplugged)
```

Why this shape:

- The 1512 wants 12 V / 650 mA. Running it and the Nano from one 12 V rail is
  fine as long as a fat cap absorbs the inrush.
- LEDs do **not** come off the Nano's 5 V pin. With 12 V on VIN the Nano's
  LDO drops 7 V, so even 200 mA cooks it. A $1 buck module solves it.
- A second identical MOSFET channel ("AUX") costs nothing and gives you a 12 V
  output for a rotating beacon, 12 V LED strip, fan, or a second lock later.

## 2. Schematic blocks and parts

Verify every LCSC number against JLC's live library before ordering; stock
moves. Prefer **Basic** parts (no per-part loading fee).

| Block | Part | Notes |
|---|---|---|
| Power in | DC barrel jack 5.5/2.1 mm, THT | hand-solder |
| Fuse | 1.5 A polyfuse 1812, or 2 A mini blade holder | 650 mA + LEDs |
| Reverse polarity | SS54 Schottky in series (simple, drops 0.4 V) or P-FET ideal diode | SS54 is fine |
| Bulk | 1000 µF / 25 V electrolytic (THT) + 100 nF | right next to the MOSFET |
| Input TVS | SMBJ15A | optional belt-and-braces |
| Solenoid MOSFET | AO3400A SOT-23 (5.7 A, logic level, ~C20917) | or AOD4184 TO-252 for margin |
| Gate | 150 Ω series from D4, **10 kΩ pulldown gate→GND** | pulldown is mandatory: keeps the lock shut during reset/bootloader |
| Flyback | SS34 SMA Schottky across solenoid, cathode to +12 V (~C8678) | mandatory |
| Solenoid connector | 2-pos 3.5 mm screw terminal or JST-XH 2.54 2-pin | 1512 ships with bare leads |
| Drain indicator | green LED + 1 kΩ from +12 V to drain | lights when energised |
| AUX channel | duplicate of the above on D5 | +1 terminal |
| Nano socket | 2×15 female 0.1" headers, rows 15.24 mm (0.6") apart | KiCad: `Module:Arduino_Nano_WithMountingHoles`, symbol `MCU_Module:Arduino_Nano_v3.x` |
| 5 V buck | MP1584EN / Mini-360 module footprint (4 pins) set to 5.0 V | THT module, no SMPS layout risk on rev A |
| 5 V decoupling | 100 µF + 100 nF at the LED ring | |
| LED ring | 16× WS2812B-5050 (or 24× WS2812B-2020) in a ring around the latch cutout | 330 Ω series on DIN from D6; Nano is 5 V logic so no level shifter |
| LED strip header | JST-SM or 3-pin 0.1" (5 V, DIN, GND) | to edge-light the acrylic |
| Buzzer | 12 mm passive piezo THT, 100 Ω series from D9 | passive so the firmware can play notes |
| Override | 6×6 tactile on board + 2-pin header for a remote keyed switch, both to D2 | 100 nF to GND for debounce |
| Lid sensor | 2-pin header to D3 (reed switch + magnet on the lid) | firmware hook later: celebrate when the lid actually opens |
| Expansion | 2×5 header: D7, D8, D10–D12, A0–A3, 5 V, GND | future 7-seg counter, servo, etc. |
| Mounting | 4× M3 holes, plus holes matching the 1512's bracket if you bolt the solenoid to the board | measure the bracket |

Pin map (matches `firmware/lockbox_nano/lockbox_nano.ino`):

| Nano pin | Function |
|---|---|
| D2 | override button (INPUT_PULLUP) |
| D3 | lid reed switch (reserved) |
| D4 | solenoid MOSFET gate |
| D5 | AUX 12 V MOSFET gate (reserved) |
| D6 | WS2812B data |
| D9 | piezo |
| D13 | onboard LED, mirrors the solenoid. **Never the solenoid itself** |
| VIN, GND | 12 V in |

## 3. Layout rules

1. **Power loop first.** Jack → fuse → bulk cap → terminal → MOSFET drain →
   source → GND → jack, as short and tight as possible, 1.5–2 mm traces or a
   pour (1 oz copper is fine for ~1 A).
2. MOSFET, flyback diode and bulk cap within ~15 mm of the solenoid terminal.
3. One continuous ground pour on the bottom; power stuff at one end, Nano in
   the middle, LEDs on the perimeter.
4. Keep the WS2812B data trace short and away from the solenoid trace.
5. Nano USB connector hangs over a board edge. The Nano sits on headers so it
   can be swapped.
6. Leave a cutout or clear zone for the solenoid latch and its wires.
7. DRC with JLC 2-layer rules: use ≥0.2 mm traces, 0.3/0.6 mm vias, 0.2 mm
   clearance even though JLC allows less.

## 4. Theatrics

Cheap, high-impact:

- **Black matte soldermask + ENIG** (gold). Or purple. Both look like props.
- **Copper art.** Open the soldermask over copper shapes: a sunburst behind the
  latch, warning chevrons along the edges, "LOCKBOX" in exposed copper.
  KiCad's Image Converter turns a PNG into a footprint on F.Cu + F.Mask.
- **Silkscreen voice.** "DO NOT OPEN", "ARMED", a fake serial number, a
  "WARNING: AUTONOMOUS SYSTEM" block, an attempts tally box.
- **Ring under a frosted acrylic diffuser** facing the hackers; a second strip
  on the header edge-lights the acrylic case so the whole module glows.
- **Big 10 mm red "ARMED" LED** at eye height on the board (constant via 1 kΩ
  from 5 V, or PWM-breathed from a spare pin later).
- **Sound.** The piezo already does a boot chirp, a buzz on refusal and an
  arpeggio on unlock. The 1512's own clunk is the real payoff.
- **AUX channel ideas.** 12 V rotating beacon for the unlock, or a short
  12 V LED strip under the laptop.
- Later: a TM1637 4-digit "ATTEMPTS" display or an analog panel meter labelled
  "RESOLVE" that creeps up as people fail, driven from the expansion header.

## 5. JLCPCB order

- 2-layer, 1.6 mm, black (or purple) soldermask, ENIG if you went for copper
  art, 5 pcs minimum.
- Economic PCBA, top side: MOSFETs, diodes, passives, WS2812B, indicator
  LEDs. Hand-solder the THT: Nano headers, barrel jack, terminals, buck
  module, electrolytic, buzzer, button.
- Install the **Fabrication Toolkit** KiCad plugin: one click gives Gerbers,
  BOM and CPL in JLC's format. Assign LCSC numbers in a `LCSC` symbol field so
  the BOM comes out populated.
- Budget: roughly $2 boards + ~$30 assembly + $8 shipping for 5. One week
  door to door is typical.

## 6. Build order

1. **Breadboard tonight.** Nano + any logic-level MOSFET + Schottky + 10 kΩ +
   the 1512 + a 12 V supply. Flash the firmware, run `lockboxd -v`, run
   `lockboxctl unlock`. Hear the clunk. This validates the whole chain.
2. **Rev A in KiCad.** Schematic → footprint assignment → layout → DRC →
   Fabrication Toolkit → order. Expect rev B; keep the first order small.
3. **Acrylic module.** 3 mm clear/frosted acrylic, M3 standoffs, two cable
   glands. Mount the solenoid so the latch clears the box's striker.
4. **Integrate.** Hand the AI team `README.md` and a token. They call
   `POST /unlock`; everything else is yours.

## 7. Don't forget

- **Fail-secure means fail-locked.** Power dies, lock stays shut. Keep a
  physical way in that does not need electronics (security screws on a panel,
  or a keyed switch wired to the override header).
- The 1512 is intermittent duty. Pulse ≤10 s, rest between pulses. The
  firmware enforces this; the hardware should not make it possible to bypass
  (no "hold to open" path that skips the Nano).
- Gate pulldown. Fuse. Flyback diode. Three parts, all mandatory.
