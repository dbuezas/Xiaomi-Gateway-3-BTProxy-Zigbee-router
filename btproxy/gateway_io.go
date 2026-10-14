package main

// The gateway's LED and button, for the Zigbee router's endpoints.
//
// LED: one RGB LED on three GPIOs (red 57, green 56, blue 54), as /bin/set_led.sh drives them: each colour only on
// or off, so 7 colours. Xiaomi's basic_gw also sets them (blue: running) but leaves a value written here alone.
// Button: /dev/input/event0 ("lumi_key", key code 102). basic_gw reads it too (without a grab), so every reader gets
// every event. The driver reports value 0 when the button goes down and 1 when it comes up (recorded on the device).

import (
	"encoding/binary"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

var ledPins = [3]string{"/sys/class/gpio/gpio57/value", "/sys/class/gpio/gpio56/value", "/sys/class/gpio/gpio54/value"}

type gatewayLED struct {
	mu       sync.Mutex
	identify chan struct{} // closed to end the running blink
	until    time.Time     // end of the running blink
	saved    [3]bool       // the LED before the blink
	colour   [3]bool       // what "on" lights when the LED is dark
}

func newGatewayLED() *gatewayLED { return &gatewayLED{colour: [3]bool{true, true, true}} }

// shown is what the LED shows (during a blink: what it showed before it).
func (l *gatewayLED) shown() [3]bool {
	if l.identify != nil {
		return l.saved
	}
	return readPins()
}

func readPins() (v [3]bool) {
	for i, p := range ledPins {
		b, err := os.ReadFile(p)
		v[i] = err == nil && strings.TrimSpace(string(b)) == "1"
	}
	return
}

func writePins(v [3]bool) {
	for i, p := range ledPins {
		s := "0"
		if v[i] {
			s = "1"
		}
		if err := os.WriteFile(p, []byte(s), 0); err != nil {
			debugf("zigbee: LED: %v", err)
		}
	}
}

// On is true when any colour is lit.
func (l *gatewayLED) On() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.shown() != [3]bool{}
}

// Colour is the lit colour, or when dark the one "on" lights.
func (l *gatewayLED) Colour() [3]bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if v := l.shown(); v != [3]bool{} {
		return v
	}
	return l.colour
}

// Set turns the LED on (keeping a lit colour) or off, ending a blink.
func (l *gatewayLED) Set(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v := l.shown()
	l.stopLocked(false)
	switch {
	case !on:
		v = [3]bool{}
	case v == [3]bool{}:
		v = l.colour
	}
	writePins(v)
}

// SetColour sets the colour "on" lights, and shows it now when the LED is lit (after a blink, when it ends).
func (l *gatewayLED) SetColour(c [3]bool) {
	if c == [3]bool{} {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.colour = c
	switch {
	case l.identify != nil:
		if l.saved != [3]bool{} {
			l.saved = c
		}
	case readPins() != [3]bool{}:
		writePins(c)
	}
}

func (l *gatewayLED) stopLocked(restore bool) {
	if l.identify == nil {
		return
	}
	close(l.identify)
	l.identify = nil
	if restore {
		writePins(l.saved)
	}
}

// IdentifyLeft is the time the running blink still takes.
func (l *gatewayLED) IdentifyLeft() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.identify == nil {
		return 0
	}
	return max(0, time.Until(l.until))
}

// Identify blinks the LED for d, then puts back what it showed; d = 0 stops a running blink.
func (l *gatewayLED) Identify(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if d <= 0 {
		l.stopLocked(true)
		return
	}
	if l.identify == nil {
		l.saved = readPins()
	} else {
		close(l.identify)
	}
	stop := make(chan struct{})
	l.identify, l.until = stop, time.Now().Add(d)
	go func() {
		t := time.NewTicker(400 * time.Millisecond)
		defer t.Stop()
		end := time.After(d)
		lit := false
		for {
			select {
			case <-stop:
				return
			case <-end:
				l.mu.Lock()
				if l.identify == stop {
					l.stopLocked(true)
				}
				l.mu.Unlock()
				return
			case <-t.C:
				lit = !lit
				l.mu.Lock()
				if l.identify == stop {
					writePins([3]bool{lit, lit, lit})
				}
				l.mu.Unlock()
			}
		}
	}()
}

// watchButton sends to presses once per short press (released within 2 s; longer ones are Xiaomi's pairing).
func watchButton(path string, presses chan<- struct{}) {
	for {
		f, err := os.Open(path)
		if err != nil {
			debugf("zigbee: button: %v", err)
			return
		}
		readButton(f, presses)
		f.Close()
		time.Sleep(5 * time.Second)
	}
}

// readButton reads 32-bit struct input_event records (time 8 bytes, type, code, value) until r fails.
func readButton(r io.Reader, presses chan<- struct{}) {
	var ev [16]byte
	var down time.Time
	for {
		if _, err := io.ReadFull(r, ev[:]); err != nil {
			debugf("zigbee: button: %v", err)
			return
		}
		typ, value := binary.LittleEndian.Uint16(ev[8:]), int32(binary.LittleEndian.Uint32(ev[12:]))
		if typ != 1 { // EV_KEY
			continue
		}
		switch value {
		case 0: // down
			down = time.Now()
		case 1: // up
			if !down.IsZero() && time.Since(down) < 2*time.Second {
				select {
				case presses <- struct{}{}:
				default:
				}
			}
			down = time.Time{}
		}
	}
}
