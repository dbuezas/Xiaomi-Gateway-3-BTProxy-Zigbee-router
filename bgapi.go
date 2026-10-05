package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// BGAPI over UART to the Silicon Labs chip of the Xiaomi Gateway 3 (Bluetooth SDK 2.13).
// Chip->host frames are SLIP-wrapped (C0 ... C0, DB DC = C0, DB DD = DB).
// Host->chip commands must be raw: SLIP-wrapped ones fail with 0x0195 "command incomplete".

type Packet struct {
	Evt     bool
	Class   byte
	ID      byte
	Payload []byte
}

func (p Packet) String() string {
	kind := "rsp"
	if p.Evt {
		kind = "evt"
	}
	return fmt.Sprintf("%s %d.%d %x", kind, p.Class, p.ID, p.Payload)
}

var ErrTimeout = errors.New("bgapi: timeout")

type BGAPI struct {
	w      io.Writer
	cmdMu  sync.Mutex
	rsp    chan Packet
	Events chan Packet
	Adv    chan Packet // advertisement events, dropped when full
}

func NewBGAPI(rw io.ReadWriter) *BGAPI {
	b := &BGAPI{
		w:      rw,
		rsp:    make(chan Packet, 4),
		Events: make(chan Packet, 256),
		Adv:    make(chan Packet, 256),
	}
	go b.readLoop(rw)
	return b
}

func (b *BGAPI) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 4096)
	var frame []byte
	inSlip := false
	for {
		c, err := br.ReadByte()
		if err != nil {
			logf("bgapi: read error: %v", err)
			close(b.Events)
			return
		}
		if c == 0xC0 {
			if inSlip && len(frame) > 0 {
				b.dispatch(slipDecode(frame))
				frame = frame[:0]
				// the closing C0 may also open the next frame
			}
			inSlip = true
			continue
		}
		if inSlip {
			frame = append(frame, c)
			if len(frame) > 2100 {
				frame = frame[:0]
				inSlip = false
			}
		}
		// bytes outside a SLIP frame are line noise; the chip always frames its output
	}
}

func slipDecode(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		if in[i] == 0xDB && i+1 < len(in) {
			i++
			if in[i] == 0xDC {
				out = append(out, 0xC0)
			} else {
				out = append(out, 0xDB)
			}
			continue
		}
		out = append(out, in[i])
	}
	return out
}

func (b *BGAPI) dispatch(f []byte) {
	if len(f) < 4 || f[0]&0x78 != 0x20 {
		debugf("bgapi: bad frame %x", f)
		return
	}
	n := int(f[0]&7)<<8 | int(f[1])
	if len(f) != n+4 {
		debugf("bgapi: length mismatch %x", f)
		return
	}
	p := Packet{Evt: f[0]&0x80 != 0, Class: f[2], ID: f[3], Payload: f[4:]}
	switch {
	case !p.Evt:
		select {
		case b.rsp <- p:
		default:
			debugf("bgapi: unexpected %v", p)
		}
	case p.Class == 0x03 && (p.ID == 0x00 || p.ID == 0x04):
		select {
		case b.Adv <- p:
		default:
		}
	default:
		b.Events <- p
	}
}

// Cmd sends one command and waits for its response. The chip handles one command at a time.
func (b *BGAPI) Cmd(class, id byte, payload []byte, timeout time.Duration) ([]byte, error) {
	b.cmdMu.Lock()
	defer b.cmdMu.Unlock()
	// drop responses of earlier commands that timed out
	for len(b.rsp) > 0 {
		<-b.rsp
	}
	frame := append([]byte{0x20 | byte(len(payload)>>8&7), byte(len(payload)), class, id}, payload...)
	debugf("-> %x", frame)
	if _, err := b.w.Write(frame); err != nil {
		return nil, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case p := <-b.rsp:
			if p.Class == class && p.ID == id {
				return p.Payload, nil
			}
			debugf("bgapi: stray %v", p)
		case <-t.C:
			return nil, fmt.Errorf("cmd %d.%d: %w", class, id, ErrTimeout)
		}
	}
}

// CmdResult runs a command whose response starts with a uint16 result code.
func (b *BGAPI) CmdResult(class, id byte, payload []byte) ([]byte, uint16, error) {
	r, err := b.Cmd(class, id, payload, 3*time.Second)
	if err != nil {
		return nil, 0, err
	}
	if len(r) < 2 {
		return r, 0, fmt.Errorf("cmd %d.%d: short response %x", class, id, r)
	}
	return r, le16(r), nil
}

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
func put16(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }
func put32(v uint32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
