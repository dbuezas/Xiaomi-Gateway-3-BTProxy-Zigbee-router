package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// Pairing and bonding, with the chip's security manager (BGAPI class 0x0F).
//
// Home Assistant never knows a device's PIN: an ESPHome proxy answers the passkey request itself, from its
// own configuration. Here that configuration is a file on the gateway, one "MAC PIN" pair per line
// (passkeyFile). Bonds live in the chip's own flash, so they survive a restart of the proxy and of the chip.
//
// When it secures a link:
//   - at connect, when the device is already bonded (re-encrypt with the stored key) or a PIN is
//     configured for it (pair now, before Home Assistant's first write needs it);
//   - when Home Assistant asks to pair (BLUETOOTH_DEVICE_REQUEST_TYPE_PAIR).
// A device that asks for security by itself (a Security Request) is answered by the chip's stack.

var passkeyFile = "/data/gw3-btproxy.passkeys"

// passkeyFor reads the PIN configured for addr, or -1. Read on every request, so editing the file needs no restart.
func passkeyFor(addr uint64) int32 {
	f, err := os.Open(passkeyFile)
	if err != nil {
		return -1
	}
	defer f.Close()
	want := macString(addr)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") || !strings.EqualFold(fields[0], want) {
			continue
		}
		if pin, err := strconv.Atoi(fields[1]); err == nil && pin >= 0 && pin <= 999999 {
			return int32(pin)
		}
	}
	return -1
}

// initSecurity sets the chip up as a central that types in a PIN: keyboard only, bonding allowed.
// Xiaomi's chip firmware (stack 2.13.8 on firmware 1.5.0_0102) is built without the security manager and
// answers every class 0x0F command 0x0183 "not implemented"; pairing then stays off and is not offered.
func (p *Proxy) initSecurity() {
	_, res, err := p.bg.CmdResult(0x0f, 0x01, []byte{0x00, 2}) // sm_configure(flags 0, keyboard only)
	p.canPair = err == nil && res == 0
	if !p.canPair {
		logf("chip has no security manager (sm_configure: res=0x%x err=%v): pairing unavailable", res, err)
		return
	}
	if _, res, err := p.bg.CmdResult(0x0f, 0x00, []byte{1}); err != nil || res != 0 { // sm_set_bondable_mode(1)
		logf("sm_set_bondable_mode: res=0x%x err=%v", res, err)
	}
}

// secureOnConnect starts encryption on a fresh link when that can succeed without being asked.
func (p *Proxy) secureOnConnect(c *Conn) {
	if !p.canPair {
		return
	}
	p.mu.Lock()
	bonded, handle := c.bonding != 0xff, c.handle
	p.mu.Unlock()
	if !bonded && passkeyFor(c.addr) < 0 {
		return
	}
	if _, res, err := p.bg.CmdResult(0x0f, 0x04, []byte{handle}); err != nil || res != 0 { // sm_increase_security
		logf("%s: increase_security: res=0x%x err=%v", macString(c.addr), res, err)
	}
}

// Pair answers Home Assistant's pair request: secure the link and report how it went.
func (p *Proxy) Pair(addr uint64) (bool, int32) {
	if !p.canPair {
		return false, 0x183 // not implemented
	}
	c := p.conn(addr)
	if c == nil {
		return false, errNotConnected
	}
	p.mu.Lock()
	if c.state != stEstablished {
		p.mu.Unlock()
		return false, errNotConnected
	}
	if c.secMode > 0 {
		p.mu.Unlock()
		return true, 0
	}
	ch := make(chan int32, 1)
	c.pairCh = ch
	handle := c.handle
	p.mu.Unlock()
	if _, res, err := p.bg.CmdResult(0x0f, 0x04, []byte{handle}); err != nil || res != 0 { // sm_increase_security
		return false, int32(res)
	}
	select {
	case r := <-ch:
		return r == 0, r
	case <-c.closed:
		return false, errNotConnected
	case <-time.After(30 * time.Second):
		return false, 0x208 // timeout
	}
}

// Unpair deletes the chip's bond with addr, if it has one.
func (p *Proxy) Unpair(addr uint64) bool {
	if !p.canPair {
		return false
	}
	p.bondMu.Lock()
	defer p.bondMu.Unlock()
	p.bondList = make(chan Packet, 32)
	defer func() { p.bondList = nil }()
	if _, res, err := p.bg.CmdResult(0x0f, 0x0b, nil); err != nil || res != 0 { // sm_list_all_bondings
		logf("list_all_bondings: res=0x%x err=%v", res, err)
		return false
	}
	ok := true
	for {
		select {
		case ev := <-p.bondList:
			if ev.ID == 0x06 { // list_all_bondings_complete
				return ok
			}
			if len(ev.Payload) >= 7 && macToU64(ev.Payload[1:7]) == addr {
				_, res, err := p.bg.CmdResult(0x0f, 0x06, []byte{ev.Payload[0]}) // sm_delete_bonding
				ok = err == nil && res == 0
				logf("%s: bond %d deleted: %v", macString(addr), ev.Payload[0], ok)
			}
		case <-time.After(3 * time.Second):
			return false
		}
	}
}

// onSecurityEvent handles the security manager's events (class 0x0F).
func (p *Proxy) onSecurityEvent(ev Packet) {
	pl := ev.Payload
	if ev.ID == 0x05 || ev.ID == 0x06 { // list_bonding_entry, list_all_bondings_complete
		if ch := p.bondList; ch != nil {
			nonBlock(ch, ev)
		}
		return
	}
	if len(pl) < 1 {
		return
	}
	c := p.connByHandle(pl[0])
	if c == nil {
		return
	}
	name := macString(c.addr)
	switch ev.ID {
	case 0x01: // passkey_request
		pin := passkeyFor(c.addr)
		if pin < 0 {
			logf("%s: asks for a PIN, none configured in %s", name, passkeyFile)
		} else {
			logf("%s: asks for a PIN, entering it", name)
		}
		args := []byte{c.handle, byte(pin), byte(pin >> 8), byte(pin >> 16), byte(pin >> 24)}
		if _, res, err := p.bg.CmdResult(0x0f, 0x08, args); err != nil || res != 0 { // sm_enter_passkey
			logf("%s: enter_passkey: res=0x%x err=%v", name, res, err)
		}
	case 0x02: // confirm_passkey (numeric comparison): a keyboard-only central cannot compare
		p.bg.CmdResult(0x0f, 0x09, []byte{c.handle, 0}) // sm_passkey_confirm(reject)
	case 0x03: // bonded
		if len(pl) >= 2 {
			p.mu.Lock()
			c.bonding = pl[1]
			p.mu.Unlock()
		}
		logf("%s: paired", name)
		p.pairResult(c, 0)
	case 0x04: // bonding_failed
		reason := int32(0)
		if len(pl) >= 3 {
			reason = int32(le16(pl[1:]))
		}
		logf("%s: pairing failed (0x%x)", name, reason)
		p.pairResult(c, reason)
	case 0x09: // confirm_bonding
		p.bg.CmdResult(0x0f, 0x0e, []byte{c.handle, 1}) // sm_bonding_confirm(accept)
	}
}

func (p *Proxy) pairResult(c *Conn, r int32) {
	p.mu.Lock()
	ch := c.pairCh
	c.pairCh = nil
	p.mu.Unlock()
	if ch != nil {
		ch <- r
	}
}
