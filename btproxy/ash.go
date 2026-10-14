package main

// ASH (UG101), the link layer under EZSP on the gateway's Zigbee chip: byte stuffing, CRC, a pseudo-random XOR on
// DATA frames, sequence numbers and acknowledgements. One DATA frame in flight in each direction (window 1), as
// bellows does.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	ashFlag   = 0x7E
	ashEscape = 0x7D
	ashXON    = 0x11
	ashXOFF   = 0x13
	ashSub    = 0x18
	ashCancel = 0x1A

	ashRST     = 0xC0
	ashRSTACK  = 0xC1
	ashERROR   = 0xC2
	ashAckTry  = 5 // DATA frame tries before the link counts as failed
	ashMaxData = 128
)

var errASHClosed = errors.New("ash: link closed")

func ashReserved(b byte) bool {
	switch b {
	case ashFlag, ashEscape, ashXON, ashXOFF, ashSub, ashCancel:
		return true
	}
	return false
}

// ashCRC is CRC-CCITT (polynomial 0x1021, initial value 0xFFFF), as binascii.crc_hqx(data, 0xFFFF).
func ashCRC(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// ashRandomize XORs DATA frame contents with the ASH pseudo-random sequence (its own inverse).
func ashRandomize(data []byte) []byte {
	out := make([]byte, len(data))
	r := byte(0x42)
	for i, b := range data {
		out[i] = b ^ r
		if r&1 == 0 {
			r >>= 1
		} else {
			r = r>>1 ^ 0xB8
		}
	}
	return out
}

// ashEncode adds the CRC to a frame (control byte + data) and stuffs it, without the closing flag.
func ashEncode(frame []byte) []byte {
	crc := ashCRC(frame)
	raw := append(append([]byte{}, frame...), byte(crc>>8), byte(crc))
	out := make([]byte, 0, len(raw)+4)
	for _, b := range raw {
		if ashReserved(b) {
			out = append(out, ashEscape, b^0x20)
		} else {
			out = append(out, b)
		}
	}
	return out
}

// ashDecode unstuffs a frame (bytes between flags) and checks its CRC; it returns control byte + data.
func ashDecode(stuffed []byte) ([]byte, error) {
	out := make([]byte, 0, len(stuffed))
	esc := false
	for _, b := range stuffed {
		switch {
		case esc:
			out = append(out, b^0x20)
			esc = false
		case b == ashEscape:
			esc = true
		default:
			out = append(out, b)
		}
	}
	if esc || len(out) < 3 {
		return nil, fmt.Errorf("ash: short frame % x", stuffed)
	}
	n := len(out) - 2
	if crc := ashCRC(out[:n]); byte(crc>>8) != out[n] || byte(crc) != out[n+1] {
		return nil, fmt.Errorf("ash: bad CRC in % x", stuffed)
	}
	return out[:n], nil
}

// ASH is one link to the chip. Run reads frames until the port fails; Reset and Send are called from other
// goroutines. Received EZSP frames go to the recv callback, which must not block for long.
type ASH struct {
	w    io.Writer
	r    *bufio.Reader
	recv func([]byte)

	wmu sync.Mutex // one frame written at a time

	mu      sync.Mutex
	txSeq   byte          // frame number of the next DATA frame sent
	rxSeq   byte          // frame number of the next DATA frame expected
	acked   chan byte     // ackNum values from the chip, for the waiting Send
	rstack  chan byte     // reset codes from RSTACK frames
	reject  bool          // a NAK was sent and no good DATA frame came since
	failed  error         // the link is gone (port error, ERROR frame, a reset nobody asked for)
	Lost    chan struct{} // closed when failed is set
	reset   bool          // Reset is waiting for an RSTACK
	done    chan struct{} // closed when Run returns
	sendMu  sync.Mutex    // one DATA frame in flight
	timeout time.Duration // current ACK timeout
}

func NewASH(rw io.ReadWriter, recv func([]byte)) *ASH {
	return &ASH{
		w: rw, r: bufio.NewReaderSize(rw, 512), recv: recv,
		acked: make(chan byte, 8), rstack: make(chan byte, 1), done: make(chan struct{}), Lost: make(chan struct{}),
		timeout: 1600 * time.Millisecond,
	}
}

func (a *ASH) write(frame []byte) error {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	_, err := a.w.Write(append(ashEncode(frame), ashFlag))
	return err
}

func (a *ASH) fail(err error) {
	a.mu.Lock()
	if a.failed == nil {
		a.failed = err
		close(a.Lost)
	}
	a.mu.Unlock()
}

func (a *ASH) err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failed
}

// Run reads from the port until it fails; it returns that error.
func (a *ASH) Run() error {
	defer close(a.done)
	var buf []byte
	discard := false // a SUBSTITUTE byte came: drop everything up to the next flag
	for {
		b, err := a.r.ReadByte()
		if err != nil {
			a.fail(err)
			return err
		}
		switch b {
		case ashXON, ashXOFF:
			continue
		case ashCancel:
			buf, discard = buf[:0], false
			continue
		case ashSub:
			buf, discard = buf[:0], true
			continue
		case ashFlag:
			if !discard && len(buf) > 0 {
				a.frame(buf)
			}
			buf, discard = buf[:0], false
			continue
		}
		if !discard && len(buf) < 512 {
			buf = append(buf, b)
		}
	}
}

func (a *ASH) frame(stuffed []byte) {
	f, err := ashDecode(stuffed)
	if err != nil {
		debugf("zigbee: %v", err)
		if len(stuffed) > 0 && stuffed[0]&0x80 == 0 {
			a.nak()
		}
		return
	}
	c := f[0]
	switch {
	case c&0x80 == 0: // DATA
		a.ackReceived(c & 7)
		num, retx := c>>4&7, c&0x08 != 0
		a.mu.Lock()
		inSeq := num == a.rxSeq
		if inSeq {
			a.rxSeq = (num + 1) & 7
			a.reject = false
		}
		ack := a.rxSeq
		a.mu.Unlock()
		switch {
		case inSeq:
			_ = a.write([]byte{0x80 | ack})
			a.recv(ashRandomize(f[1:]))
		case retx:
			_ = a.write([]byte{0x80 | ack}) // already received: acknowledge again
		default:
			a.nak()
		}
	case c&0xE0 == 0x80: // ACK
		a.ackReceived(c & 7)
	case c&0xE0 == 0xA0: // NAK: the waiting Send retransmits at once
		a.ackReceived(c & 7)
		select {
		case a.acked <- 0xFF:
		default:
		}
	case c == ashRSTACK && len(f) == 3:
		a.mu.Lock()
		a.txSeq, a.rxSeq, a.reject = 0, 0, false
		asked := a.reset
		a.mu.Unlock()
		if !asked { // the chip reset by itself: its network is down until a host brings it up again
			a.fail(fmt.Errorf("ash: the chip reset (code 0x%02X)", f[2]))
			return
		}
		select {
		case a.rstack <- f[2]:
		default:
		}
	case c == ashERROR && len(f) == 3:
		a.fail(fmt.Errorf("ash: chip in error state (code 0x%02X)", f[2]))
		select {
		case a.acked <- 0xFF:
		default:
		}
	}
}

func (a *ASH) nak() {
	a.mu.Lock()
	send, ack := !a.reject, a.rxSeq
	a.reject = true
	a.mu.Unlock()
	if send {
		_ = a.write([]byte{0xA0 | ack})
	}
}

func (a *ASH) ackReceived(ackNum byte) {
	select {
	case a.acked <- ackNum:
	default:
	}
}

// Reset resets the chip (as every host connection does) and waits for its RSTACK.
func (a *ASH) Reset(timeout time.Duration) error {
	a.mu.Lock()
	a.reset = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.reset = false
		a.mu.Unlock()
	}()
	for try := 0; try < 3; try++ {
		select { // a stale RSTACK
		case <-a.rstack:
		default:
		}
		a.wmu.Lock()
		_, err := a.w.Write(append(append(make([]byte, 0, 40), cancels32[:]...), append(ashEncode([]byte{ashRST}), ashFlag)...))
		a.wmu.Unlock()
		if err != nil {
			return err
		}
		select {
		case code := <-a.rstack:
			debugf("zigbee: chip reset (code 0x%02X)", code)
			return nil
		case <-a.done:
			return a.err()
		case <-time.After(timeout):
		}
	}
	return errors.New("ash: no RSTACK from the chip")
}

var cancels32 = func() (c [32]byte) {
	for i := range c {
		c[i] = ashCancel
	}
	return
}()

// Send sends one EZSP frame and waits until the chip acknowledges it.
func (a *ASH) Send(ezsp []byte) error {
	if len(ezsp) > ashMaxData {
		return fmt.Errorf("ash: frame too long (%d bytes)", len(ezsp))
	}
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	a.mu.Lock()
	num := a.txSeq
	a.txSeq = (a.txSeq + 1) & 7
	a.mu.Unlock()
	payload := ashRandomize(ezsp)
	for try := 0; try < ashAckTry; try++ {
		if err := a.err(); err != nil {
			return err
		}
		a.mu.Lock()
		c := num<<4 | a.rxSeq
		a.mu.Unlock()
		if try > 0 {
			c |= 0x08
		}
		for len(a.acked) > 0 { // stale acknowledgements
			<-a.acked
		}
		sent := time.Now()
		if err := a.write(append([]byte{c}, payload...)); err != nil {
			a.fail(err)
			return err
		}
		timer := time.NewTimer(a.timeout)
	wait:
		for {
			select {
			case n := <-a.acked:
				if n == 0xFF { // NAK or ERROR: retransmit (or stop, if the link failed)
					timer.Stop()
					break wait
				}
				if n == (num+1)&7 {
					timer.Stop()
					a.adjust(time.Since(sent))
					return nil
				}
			case <-timer.C:
				a.timeout = min(2*a.timeout, 3200*time.Millisecond)
				break wait
			case <-a.done:
				timer.Stop()
				return errASHClosed
			}
		}
	}
	if err := a.err(); err != nil {
		return err
	}
	err := errors.New("ash: the chip does not acknowledge")
	a.fail(err)
	return err
}

// adjust sets the ACK timeout to 7/8 of its value plus half the measured time, within 0.4..3.2 s (UG101).
func (a *ASH) adjust(took time.Duration) {
	t := a.timeout*7/8 + took/2
	a.timeout = max(400*time.Millisecond, min(t, 3200*time.Millisecond))
}
