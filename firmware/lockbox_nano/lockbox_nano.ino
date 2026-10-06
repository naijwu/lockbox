/*
 * Lockbox carrier board firmware - Arduino Nano (ATmega328P)
 *
 * Drives an Adafruit 1512 lock-style solenoid through a low-side MOSFET,
 * a WS2812B LED ring for theatrics, a piezo buzzer, and an organiser
 * override button. Talks to lockboxd over USB serial at 115200 baud.
 *
 * Protocol (one command per line, "\n" terminated):
 *   PING            -> PONG
 *   UNLOCK [ms]     -> OK UNLOCK <ms>   then "EVT UNLOCKED <ms>", later "EVT LOCKED"
 *   LOCK            -> OK LOCK          (abort pulse)
 *   STATUS          -> STATUS locked=<0|1> uptime=<ms> pulses=<n> fw=<ver>
 *   LED <mode>      -> OK LED <mode>    idle|thinking|denied|unlock|party|off
 *   anything else   -> ERR <reason>
 * On boot the board prints "HELLO lockbox fw=<ver>".
 *
 * Safety rules the board enforces on its own, regardless of the host:
 *   - the solenoid pin is driven LOW first thing in setup()
 *   - a pulse is clamped to [MIN_PULSE_MS, MAX_PULSE_MS] and always ends by itself
 *   - a second UNLOCK during a pulse is rejected (ERR BUSY)
 *   - the coil gets MIN_OFF_MS rest between pulses (ERR COOLDOWN)
 *   - a 1 s watchdog resets the chip if the loop ever stalls; on reset all pins
 *     go hi-Z and the 10k gate pulldown on the carrier board keeps the MOSFET off
 *
 * Libraries: Adafruit NeoPixel.
 */

#include <Adafruit_NeoPixel.h>
#include <avr/wdt.h>

#define FW_VERSION "1.0"

// ---------------------------------------------------------------- pins ----
// Keep the solenoid OFF D13: the bootloader blinks D13 for a second on every
// reset/port-open, which would pulse the lock.
const uint8_t PIN_SOLENOID = 4;   // MOSFET gate, via 150R; 10k pulldown on board
const uint8_t PIN_LED_DATA = 6;   // WS2812B DIN, via 330R
const uint8_t PIN_BUTTON   = 2;   // override button to GND (uses INPUT_PULLUP)
const uint8_t PIN_BUZZER   = 9;   // passive piezo, via 100R
const uint8_t PIN_STATUS   = 13;  // onboard LED mirrors the solenoid

const uint16_t NUM_LEDS = 16;
const uint8_t  LED_BRIGHTNESS = 128;   // keep the 5V budget sane

// Set to 0 if your Nano has the OLD bootloader (ATmegaBOOT): it does not clear
// the watchdog flag and the chip will boot-loop after a WDT reset. The "new
// bootloader" (Optiboot) Nanos are fine. The pulse timer below is still a
// hard upper bound even without the watchdog.
#define USE_WATCHDOG 1

// -------------------------------------------------------------- timings ----
const uint32_t MIN_PULSE_MS     = 100;
const uint32_t MAX_PULSE_MS     = 10000;  // Adafruit 1512 is intermittent duty only
const uint32_t DEFAULT_PULSE_MS = 3000;
const uint32_t MIN_OFF_MS       = 500;    // coil rest between pulses
const uint32_t OVERRIDE_HOLD_MS = 2000;   // hold the button this long to unlock
const uint32_t DENIED_MS        = 1500;   // length of the "denied" strobe

// ---------------------------------------------------------------- state ----
Adafruit_NeoPixel strip(NUM_LEDS, PIN_LED_DATA, NEO_GRB + NEO_KHZ800);

enum LedMode : uint8_t { LED_IDLE, LED_THINKING, LED_DENIED, LED_UNLOCK, LED_PARTY, LED_OFF };
const char* const LED_NAMES[] = { "idle", "thinking", "denied", "unlock", "party", "off" };

LedMode  ledMode  = LED_IDLE;
uint32_t ledSince = 0;

bool     unlocked     = false;
uint32_t unlockUntil  = 0;
uint32_t lastLockedAt = 0;
uint32_t pulses       = 0;

char    line[48];
uint8_t lineLen = 0;

bool     btnWasPressed = false;
bool     btnFired      = false;
uint32_t btnPressedAt  = 0;

// --------------------------------------------------------------- buzzer ----
struct Note { uint16_t freq; uint16_t ms; };
const Note SEQ_BOOT[]   = { {880, 70}, {0, 40}, {1320, 90} };
const Note SEQ_UNLOCK[] = { {523, 110}, {659, 110}, {784, 110}, {1047, 320} };
const Note SEQ_DENIED[] = { {196, 160}, {0, 60}, {147, 320} };

const Note* seq    = nullptr;
uint8_t     seqLen = 0, seqIdx = 0;
uint32_t    noteEnd = 0;

void play(const Note* s, uint8_t n) { seq = s; seqLen = n; seqIdx = 0; noteEnd = 0; }

void tickBuzzer() {
  if (!seq) return;
  uint32_t now = millis();
  if ((int32_t)(now - noteEnd) < 0) return;
  if (seqIdx >= seqLen) { noTone(PIN_BUZZER); seq = nullptr; return; }
  const Note& n = seq[seqIdx++];
  if (n.freq) tone(PIN_BUZZER, n.freq, n.ms); else noTone(PIN_BUZZER);
  noteEnd = now + n.ms;
}

// ----------------------------------------------------------------- LEDs ----
void setLedMode(LedMode m) { ledMode = m; ledSince = millis(); }

int8_t ledModeFromName(const char* s) {
  for (uint8_t i = 0; i < sizeof(LED_NAMES) / sizeof(LED_NAMES[0]); i++)
    if (strcmp(s, LED_NAMES[i]) == 0) return i;
  return -1;
}

void renderLeds() {
  static uint32_t lastFrame = 0;
  uint32_t now = millis();
  if (now - lastFrame < 20) return;  // ~50 fps
  lastFrame = now;
  uint32_t t = now - ledSince;

  switch (ledMode) {
    case LED_OFF:
      strip.clear();
      break;

    case LED_IDLE: {  // slow red breathing: armed and waiting
      float ph = (now % 3000UL) * (TWO_PI / 3000.0f);
      uint8_t v = 10 + (uint8_t)((sin(ph) + 1.0f) * 0.5f * 100);
      strip.fill(strip.Color(v, 0, 0));
      break;
    }

    case LED_THINKING: {  // cyan comet chasing around the ring
      strip.clear();
      uint16_t head = (now / 45) % NUM_LEDS;
      for (uint8_t k = 0; k < 6 && k < NUM_LEDS; k++) {
        uint16_t i = (head + NUM_LEDS - k) % NUM_LEDS;
        uint8_t v = 220 >> k;
        strip.setPixelColor(i, strip.Color(0, v, v));
      }
      break;
    }

    case LED_DENIED: {  // hard red strobe, then back to idle
      bool on = ((t / 80) % 2) == 0;
      strip.fill(on ? strip.Color(255, 0, 0) : 0);
      if (t > DENIED_MS) setLedMode(LED_IDLE);
      break;
    }

    case LED_UNLOCK: {  // fast green spin with a white spark
      strip.fill(strip.Color(0, 40, 8));
      uint16_t head = (now / 25) % NUM_LEDS;
      strip.setPixelColor(head, strip.Color(255, 255, 255));
      strip.setPixelColor((head + NUM_LEDS - 1) % NUM_LEDS, strip.Color(0, 255, 60));
      strip.setPixelColor((head + NUM_LEDS - 2) % NUM_LEDS, strip.Color(0, 120, 30));
      break;
    }

    case LED_PARTY: {  // rainbow wheel
      for (uint16_t i = 0; i < NUM_LEDS; i++) {
        uint16_t hue = (uint16_t)(now * 24) + (uint16_t)(i * (65536UL / NUM_LEDS));
        strip.setPixelColor(i, strip.gamma32(strip.ColorHSV(hue)));
      }
      break;
    }
  }
  strip.show();
}

// ------------------------------------------------------------- solenoid ----
void startUnlock(uint32_t ms) {
  if (ms < MIN_PULSE_MS) ms = MIN_PULSE_MS;
  if (ms > MAX_PULSE_MS) ms = MAX_PULSE_MS;
  unlocked    = true;
  unlockUntil = millis() + ms;
  pulses++;
  digitalWrite(PIN_SOLENOID, HIGH);
  digitalWrite(PIN_STATUS, HIGH);
  setLedMode(LED_UNLOCK);
  play(SEQ_UNLOCK, sizeof(SEQ_UNLOCK) / sizeof(Note));
  Serial.print(F("EVT UNLOCKED "));
  Serial.println(ms);
}

void endUnlock() {
  digitalWrite(PIN_SOLENOID, LOW);
  digitalWrite(PIN_STATUS, LOW);
  unlocked     = false;
  lastLockedAt = millis();
  if (lastLockedAt == 0) lastLockedAt = 1;
  setLedMode(LED_IDLE);
  Serial.println(F("EVT LOCKED"));
}

// --------------------------------------------------------------- serial ----
void handleLine(char* s) {
  if (strcmp(s, "PING") == 0) { Serial.println(F("PONG")); return; }

  if (strncmp(s, "UNLOCK", 6) == 0 && (s[6] == 0 || s[6] == ' ')) {
    uint32_t ms = DEFAULT_PULSE_MS;
    if (s[6]) { ms = strtoul(s + 7, NULL, 10); if (ms == 0) ms = DEFAULT_PULSE_MS; }
    if (ms < MIN_PULSE_MS) ms = MIN_PULSE_MS;
    if (ms > MAX_PULSE_MS) ms = MAX_PULSE_MS;
    if (unlocked) { Serial.println(F("ERR BUSY")); return; }
    if (lastLockedAt && millis() - lastLockedAt < MIN_OFF_MS) { Serial.println(F("ERR COOLDOWN")); return; }
    Serial.print(F("OK UNLOCK ")); Serial.println(ms);
    startUnlock(ms);
    return;
  }

  if (strcmp(s, "LOCK") == 0) {
    if (unlocked) endUnlock();
    Serial.println(F("OK LOCK"));
    return;
  }

  if (strcmp(s, "STATUS") == 0) {
    Serial.print(F("STATUS locked=")); Serial.print(unlocked ? 0 : 1);
    Serial.print(F(" uptime="));       Serial.print(millis());
    Serial.print(F(" pulses="));       Serial.print(pulses);
    Serial.print(F(" fw="));           Serial.print(F(FW_VERSION));
    Serial.print(F(" led="));          Serial.println(LED_NAMES[ledMode]);
    return;
  }

  if (strncmp(s, "LED ", 4) == 0) {
    int8_t m = ledModeFromName(s + 4);
    if (m < 0) { Serial.println(F("ERR BADMODE")); return; }
    if (unlocked && m != LED_OFF) { Serial.print(F("OK LED ")); Serial.println(s + 4); return; } // unlock show wins
    setLedMode((LedMode)m);
    if (m == LED_DENIED) play(SEQ_DENIED, sizeof(SEQ_DENIED) / sizeof(Note));
    Serial.print(F("OK LED ")); Serial.println(s + 4);
    return;
  }

  if (strcmp(s, "HELLO") == 0) { Serial.print(F("HELLO lockbox fw=")); Serial.println(F(FW_VERSION)); return; }

  Serial.print(F("ERR UNKNOWN ")); Serial.println(s);
}

void pollSerial() {
  while (Serial.available()) {
    char ch = (char)Serial.read();
    if (ch == '\n' || ch == '\r') {
      if (lineLen) { line[lineLen] = 0; handleLine(line); lineLen = 0; }
    } else if (lineLen < sizeof(line) - 1) {
      line[lineLen++] = ch;
    } else {
      lineLen = 0;  // oversized line: discard
    }
  }
}

// --------------------------------------------------------------- button ----
void pollButton() {
  bool pressed = digitalRead(PIN_BUTTON) == LOW;
  uint32_t now = millis();
  if (pressed && !btnWasPressed) { btnWasPressed = true; btnPressedAt = now; btnFired = false; }
  if (!pressed) { btnWasPressed = false; return; }
  if (!btnFired && now - btnPressedAt >= OVERRIDE_HOLD_MS) {
    btnFired = true;
    if (!unlocked) {
      Serial.println(F("EVT OVERRIDE"));
      startUnlock(DEFAULT_PULSE_MS);
    }
  }
}

// ---------------------------------------------------------------- setup ----
void setup() {
  MCUSR = 0;
  wdt_disable();

  pinMode(PIN_SOLENOID, OUTPUT); digitalWrite(PIN_SOLENOID, LOW);
  pinMode(PIN_STATUS,   OUTPUT); digitalWrite(PIN_STATUS, LOW);
  pinMode(PIN_BUTTON,   INPUT_PULLUP);
  pinMode(PIN_BUZZER,   OUTPUT);

  strip.begin();
  strip.setBrightness(LED_BRIGHTNESS);
  strip.clear();
  strip.show();

  Serial.begin(115200);
  Serial.print(F("HELLO lockbox fw=")); Serial.println(F(FW_VERSION));

  setLedMode(LED_IDLE);
  play(SEQ_BOOT, sizeof(SEQ_BOOT) / sizeof(Note));

#if USE_WATCHDOG
  wdt_enable(WDTO_1S);
#endif
}

void loop() {
#if USE_WATCHDOG
  wdt_reset();
#endif
  // Solenoid timeout is checked first, before anything that could be slow.
  if (unlocked && (int32_t)(millis() - unlockUntil) >= 0) endUnlock();
  pollSerial();
  pollButton();
  tickBuzzer();
  renderLeds();
}
