GO      ?= go
FQBN    ?= arduino:avr:nano
# Use arduino:avr:nano:cpu=atmega328old for clones with the old bootloader.
PORT    ?= $(firstword $(wildcard /dev/cu.usbserial* /dev/cu.wchusbserial* /dev/cu.usbmodem* /dev/ttyUSB* /dev/ttyACM*))

.PHONY: all build test run-mock fw fw-flash fw-monitor clean

all: build

build:
	$(GO) build -o bin/ ./cmd/...

test:
	$(GO) test -race ./...

run-mock: build
	./bin/lockboxd -mock -v

fw:
	arduino-cli compile --fqbn $(FQBN) --warnings default firmware/lockbox_nano

fw-flash: fw
	@test -n "$(PORT)" || (echo "no serial port found; set PORT=/dev/..." && exit 1)
	arduino-cli upload --fqbn $(FQBN) -p $(PORT) firmware/lockbox_nano

fw-monitor:
	arduino-cli monitor -p $(PORT) -c baudrate=115200

clean:
	rm -rf bin
