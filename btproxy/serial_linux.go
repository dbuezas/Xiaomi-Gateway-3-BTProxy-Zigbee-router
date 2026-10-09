package main

import (
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// openSerial opens the chip UART raw at 115200 8N1, blocking reads. With rtscts it also turns on hardware flow
// control (RTS/CTS). This kernel's UART driver stops sending for good when it gets data to send while CTS is low
// (the chip holds CTS low for ~130 ms after every reset, a little longer than its boot event), and never notices
// CTS coming back. So flow control goes on only once the chip raises CTS, and every write first waits for CTS.
// If CTS stays low, the port runs without flow control.
func openSerial(path string, rtscts bool) (io.ReadWriter, error) {
	f, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		f.Close()
		return nil, err
	}
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB | unix.CRTSCTS | unix.CBAUD
	t.Cflag |= unix.CS8 | unix.CLOCAL | unix.CREAD | unix.B115200
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		f.Close()
		return nil, err
	}
	if !rtscts {
		return f, nil
	}
	p := &ctsPort{File: f, fd: fd}
	if waitCTS(fd, 3*time.Second) {
		p.enable()
	} else {
		logf("CTS low at start: no hardware flow control until the chip raises it (next boot event)")
	}
	return p, nil
}

func waitCTS(fd int, timeout time.Duration) bool {
	end := time.Now().Add(timeout)
	for {
		if m, err := unix.IoctlGetInt(fd, unix.TIOCMGET); err == nil && m&unix.TIOCM_CTS != 0 {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ctsPort: the UART with hardware flow control (see openSerial). While flow control is on, every write waits for
// CTS. Rearm and writes take the same lock, so termios never changes in the middle of a write.
type ctsPort struct {
	*os.File
	fd       int
	mu       sync.Mutex
	on       bool
	lowSince time.Time // last "CTS low before a write" log, to keep the log quiet when the chip is gone
}

func (p *ctsPort) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.on && !waitCTS(p.fd, time.Second) && time.Since(p.lowSince) > time.Minute {
		logf("CTS low for 1 s before a write")
		p.lowSince = time.Now()
	}
	return p.File.Write(b)
}

// enable turns flow control on (CTS must be up). Called with p.mu held, or before p is shared.
// It logs only when flow control was off before (not on every re-arm).
func (p *ctsPort) enable() { p.enableQuiet(false) }

func (p *ctsPort) enableQuiet(wasOn bool) {
	t, err := unix.IoctlGetTermios(p.fd, unix.TCGETS)
	if err == nil {
		t.Cflag |= unix.CRTSCTS
		err = unix.IoctlSetTermios(p.fd, unix.TCSETS, t)
	}
	if err != nil {
		logf("hardware flow control: %v; running without it", err)
		return
	}
	if !wasOn {
		logf("hardware flow control on")
	}
	p.on = true
}

// Rearm is called on every chip boot event. A reset drops CTS, and the driver then pauses sending for good (it
// does not notice CTS coming back). Turning flow control off clears that pause; turning it on again, once CTS is
// up, makes the driver read the line afresh.
func (p *ctsPort) Rearm() { p.rearm(2 * time.Second) }

// Unstick is called after a command timed out: sending may be paused although the chip did not reset. Only while
// flow control is on, and without a long wait (a dead chip would otherwise make every retry 2 s slower).
func (p *ctsPort) Unstick() {
	p.mu.Lock()
	on := p.on
	p.mu.Unlock()
	if on {
		p.rearm(100 * time.Millisecond)
	}
}

func (p *ctsPort) rearm(wait time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	wasOn := p.on
	if !waitCTS(p.fd, wait) {
		if wasOn {
			logf("CTS stays low: hardware flow control off")
		}
		p.setOff()
		return
	}
	p.setOff()
	p.enableQuiet(wasOn)
}

func (p *ctsPort) setOff() {
	t, err := unix.IoctlGetTermios(p.fd, unix.TCGETS)
	if err == nil {
		t.Cflag &^= unix.CRTSCTS
		err = unix.IoctlSetTermios(p.fd, unix.TCSETS, t)
	}
	if err != nil {
		logf("hardware flow control off: %v", err)
		return
	}
	p.on = false
}
