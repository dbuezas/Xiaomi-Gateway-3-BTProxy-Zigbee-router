package main

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Proxy owns the BT chip: scanning, connections and GATT procedures.

type Config struct {
	MaxConn int
	Active  bool
	MaxMTU  uint16
}

const (
	stConnecting = iota
	stConnected  // link up, discovering services
	stEstablished
	stClosing
)

type Service struct {
	UUID   []byte // little endian, 2 or 16 bytes
	Handle uint16
	Chars  []Char
}

type Char struct {
	UUID   []byte
	Handle uint16
	Props  byte
	Descs  []Desc
}

type Desc struct {
	UUID   []byte
	Handle uint16
}

type Conn struct {
	addr     uint64
	addrType byte
	handle   byte
	state    int
	mtu      uint16
	cached   bool // client has the services cached (CONNECT_V3_WITH_CACHE)
	services []Service

	cancel     chan struct{} // closed when the client gives up on a connect in progress
	cancelOnce sync.Once
	link       chan Packet   // le_connection opened/closed while connecting
	mtuEv      chan struct{} // gatt_mtu_exchanged
	closed     chan struct{} // closed when the connection is dropped
	lost       chan struct{} // closed when the link goes down during setup (lostReason says why)
	lostReason int32
	procMu     sync.Mutex  // one GATT procedure at a time per connection
	proc       chan Packet // events of the running procedure

	bonding byte       // the chip's bond handle for this device, 0xff = none
	secMode byte       // le_connection_parameters security mode: 0 = not encrypted
	pairCh  chan int32 // a pending Pair waits here for bonded / bonding_failed
}

type Proxy struct {
	bg  *BGAPI
	cfg Config
	api *Server

	mu       sync.Mutex
	conns    map[uint64]*Conn
	byHandle map[byte]*Conn
	opened   map[byte]Packet // le_connection_opened that arrived before le_gap_connect's response was handled
	btAddr   atomic.Uint64
	active   atomic.Bool

	connectMu  sync.Mutex // the chip establishes one connection at a time
	connecting atomic.Bool
	needInit   atomic.Bool // the chip rebooted by itself
	scanMu     sync.Mutex
	scanning   bool // under scanMu
	bootCh     chan Packet
	initing    atomic.Bool
	lastAdv    time.Time // under mu

	canPair  atomic.Bool // the chip firmware has a security manager (see initSecurity)
	bondMu   sync.Mutex  // one bond listing at a time
	bondList chan Packet // list_bonding_entry events while Unpair lists them (under mu)
}

func NewProxy(bg *BGAPI, cfg Config) *Proxy {
	p := &Proxy{
		bg:       bg,
		cfg:      cfg,
		conns:    map[uint64]*Conn{},
		byHandle: map[byte]*Conn{},
		opened:   map[byte]Packet{},
		bootCh:   make(chan Packet, 1),
	}
	p.active.Store(cfg.Active)
	return p
}

// ---------- chip setup ----------

func (p *Proxy) Init() error {
	p.initing.Store(true)
	defer p.initing.Store(false)
	p.needInit.Store(false)
	p.mu.Lock()
	clear(p.opened)
	p.mu.Unlock()
	var err error
	for i := 0; i < 5; i++ {
		if _, err = p.bg.Cmd(0x01, 0x00, nil, time.Second); err == nil { // system_hello
			break
		}
	}
	if err != nil {
		logf("chip does not answer hello, pulsing reset GPIO")
		gpioReset()
	}
	// A reset just before this one (the switch script's, or the GPIO pulse above) may still deliver its boot event:
	// let it arrive and drop it, so it is not taken for the boot event of the reset below.
	time.Sleep(300 * time.Millisecond)
	for len(p.bootCh) > 0 {
		<-p.bootCh
	}
	// system_reset(0) has no response, only a boot event. (1 = DFU mode, the chip then goes silent.)
	p.bg.w.Write([]byte{0x20, 0x01, 0x01, 0x01, 0x00})
	select {
	case b := <-p.bootCh:
		if len(b.Payload) >= 6 {
			logf("chip booted: stack %d.%d.%d", le16(b.Payload), le16(b.Payload[2:]), le16(b.Payload[4:]))
		}
	case <-time.After(4 * time.Second):
		return fmt.Errorf("no boot event after reset")
	}
	r, err := p.bg.Cmd(0x01, 0x03, nil, 2*time.Second) // system_get_bt_address
	if err != nil || len(r) < 6 {
		return fmt.Errorf("get_bt_address: %v", err)
	}
	p.btAddr.Store(macToU64(r[:6]))
	logf("chip address %s", macString(p.btAddr.Load()))
	if p.cfg.MaxMTU > 23 {
		if _, res, err := p.bg.CmdResult(0x09, 0x00, put16(p.cfg.MaxMTU)); err != nil || res != 0 { // gatt_set_max_mtu
			logf("set_max_mtu: res=0x%x err=%v", res, err)
		}
	}
	// Extended scan reports bypass the Xiaomi firmware's advert filter (legacy reports only carry Xiaomi adverts).
	if _, res, err := p.bg.CmdResult(0x03, 0x1c, []byte{1}); err != nil || res != 0 {
		return fmt.Errorf("set_discovery_extended_scan_response: res=0x%x err=%v", res, err)
	}
	p.initSecurity()
	p.scanMu.Lock()
	defer p.scanMu.Unlock()
	p.scanning = false
	return p.startScanLocked()
}

func (p *Proxy) startScan() error {
	p.scanMu.Lock()
	defer p.scanMu.Unlock()
	return p.startScanLocked()
}

func (p *Proxy) startScanLocked() error {
	p.bg.CmdResult(0x03, 0x03, nil) // end_procedure, in case
	// full duty while idle; with open links leave the radio 40 % of the time for them
	interval, window := uint16(16), uint16(16) // 10 ms / 10 ms
	p.mu.Lock()
	if len(p.byHandle) > 0 {
		interval, window = 80, 48 // 50 ms / 30 ms
	}
	p.mu.Unlock()
	scanType := byte(0)
	if p.active.Load() {
		scanType = 1
	}
	steps := []struct {
		id   byte
		args []byte
	}{
		// Every time, not once at boot: a stack that falls back to legacy reports passes only
		// Xiaomi's adverts, so every other device vanishes while the watchdog still sees traffic.
		{0x1c, []byte{1}},           // set_discovery_extended_scan_response(1)
		{0x17, []byte{1, scanType}}, // set_discovery_type(1M, passive/active)
		{0x16, append([]byte{1}, append(put16(interval), put16(window)...)...)}, // set_discovery_timing(1M, interval, window)
		{0x18, []byte{1, 2}}, // start_discovery(1M, observation)
	}
	for _, s := range steps {
		_, res, err := p.bg.CmdResult(0x03, s.id, s.args)
		if err != nil || res != 0 {
			return fmt.Errorf("scan setup 3.%d: res=0x%x err=%v", s.id, res, err)
		}
	}
	p.scanning = true
	p.mu.Lock()
	p.lastAdv = time.Now()
	p.mu.Unlock()
	return nil
}

func (p *Proxy) stopScanLocked() {
	if p.scanning {
		p.bg.CmdResult(0x03, 0x03, nil)
		p.scanning = false
	}
}

func (p *Proxy) SetActive(active bool) {
	p.scanMu.Lock()
	defer p.scanMu.Unlock()
	p.active.Store(active)
	if p.scanning {
		if err := p.startScanLocked(); err != nil {
			logf("rescan: %v", err)
		}
	}
}

// gpioReset pulses the chip reset line (GPIO31), the way daemon_miio.sh (firmware 1.5.0) and
// reset_bt_target.sh 1 (1.5.4 and later) do. On 1.5.4+ GPIO37 selects the chip's bootloader when it is low
// during the reset: it is set high first, so the chip always starts its application, never the bootloader.
// Only there: 1.5.0 uses GPIO26 for that pin (in its own flash scripts) and never touches GPIO37.
func gpioReset() {
	const v = "/sys/class/gpio/gpio31/value"
	if _, err := os.Stat(v); err != nil {
		return
	}
	if boot := "/sys/class/gpio/gpio37/value"; fileExists("/bin/reset_bt_target.sh") && fileExists(boot) {
		os.WriteFile(boot, []byte("1"), 0)
	}
	os.WriteFile(v, []byte("0"), 0)
	time.Sleep(time.Second)
	os.WriteFile(v, []byte("1"), 0)
	time.Sleep(time.Second)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ---------- event loop ----------

func (p *Proxy) Run() {
	go p.advLoop()
	go p.watchdog()
	for ev := range p.bg.Events {
		p.onEvent(ev)
	}
	logf("chip link lost")
	os.Exit(1)
}

func (p *Proxy) onEvent(ev Packet) {
	pl := ev.Payload
	switch {
	case ev.Class == 0x01 && ev.ID == 0x00: // system_boot
		if r, ok := p.bg.w.(interface{ Rearm() }); ok {
			r.Rearm() // hardware flow control after the reset: before anything else is sent
		}
		if !p.initing.Load() {
			// the chip rebooted by itself: every link is gone and it no longer scans. Let the watchdog set it up again.
			logf("chip rebooted unexpectedly")
			p.needInit.Store(true)
			p.dropAll(0x23e)
			return
		}
		select {
		case p.bootCh <- ev:
		default:
		}
	case ev.Class == 0x01 && ev.ID == 0x06: // system_error
		logf("chip error event %v", ev)
	case ev.Class == 0x08 && ev.ID == 0x00 && len(pl) >= 9: // le_connection_opened
		p.mu.Lock()
		c := p.byHandle[pl[8]]
		if c == nil {
			p.opened[pl[8]] = ev // openLink has not registered the handle yet
			p.mu.Unlock()
			return
		}
		if len(pl) >= 10 {
			c.bonding = pl[9]
			c.secMode = 0
		}
		link := c.link
		p.mu.Unlock()
		nonBlock(link, ev)
	case ev.Class == 0x08 && ev.ID == 0x02 && len(pl) >= 8: // le_connection_parameters
		if c := p.connByHandle(pl[0]); c != nil {
			p.mu.Lock()
			was := c.secMode
			c.secMode = pl[7]
			p.mu.Unlock()
			if pl[7] != was {
				logf("%s: security mode %d", macString(c.addr), pl[7])
			}
			if pl[7] > 0 {
				p.pairResult(c, 0)
			}
		}
	case ev.Class == 0x0f:
		p.onSecurityEvent(ev)
	case ev.Class == 0x08 && ev.ID == 0x01 && len(pl) >= 3: // le_connection_closed
		p.mu.Lock()
		delete(p.opened, pl[2])
		c := p.byHandle[pl[2]]
		if c == nil {
			p.mu.Unlock()
			return
		}
		switch c.state {
		case stConnecting:
			link := c.link
			p.mu.Unlock()
			nonBlock(link, ev)
		case stConnected: // during setup: setupLink decides about a retry
			if c.lostReason == 0 {
				c.lostReason = int32(le16(pl))
				close(c.lost)
			}
			delete(p.byHandle, c.handle)
			p.mu.Unlock()
		default:
			p.mu.Unlock()
			p.dropConn(c, int32(le16(pl)))
		}
	case ev.Class == 0x09 && len(pl) >= 1:
		c := p.connByHandle(pl[0])
		if c == nil {
			return
		}
		switch {
		case ev.ID == 0x00 && len(pl) >= 3: // gatt_mtu_exchanged
			p.mu.Lock()
			c.mtu = le16(pl[1:])
			mtuEv := c.mtuEv
			p.mu.Unlock()
			select {
			case mtuEv <- struct{}{}:
			default:
			}
		case ev.ID == 0x04 && len(pl) >= 7 && (pl[3] == 0x1b || pl[3] == 0x1d): // notification / indication
			p.api.NotifyData(c.addr, le16(pl[1:]), pl[7:])
			if pl[3] == 0x1d {
				go p.bg.CmdResult(0x09, 0x0d, []byte{c.handle}) // send_characteristic_confirmation
			}
		default:
			p.mu.Lock()
			ch := c.proc
			p.mu.Unlock()
			if ch != nil {
				nonBlock(ch, ev)
			}
		}
	default:
		debugf("unhandled %v", ev)
	}
}

func nonBlock(ch chan Packet, ev Packet) {
	select {
	case ch <- ev:
	default:
	}
}

func (p *Proxy) connByHandle(h byte) *Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byHandle[h]
}

// advLoop batches advertisements for the API clients.
func (p *Proxy) advLoop() {
	tick := time.NewTicker(100 * time.Millisecond)
	var batch []RawAdv
	for {
		select {
		case ev := <-p.bg.Adv:
			pl := ev.Payload
			var a RawAdv
			if ev.ID == 0x04 && len(pl) >= 18 { // extended_scan_response
				a = RawAdv{Addr: macToU64(pl[1:7]), AddrType: pl[7], RSSI: int8(pl[13]), Data: pl[18:]}
			} else if ev.ID == 0x00 && len(pl) >= 11 { // legacy scan_response
				a = RawAdv{Addr: macToU64(pl[2:8]), AddrType: pl[8], RSSI: int8(pl[0]), Data: pl[11:]}
			} else {
				continue
			}
			if len(a.Data) > 62 {
				a.Data = a.Data[:62]
			}
			p.mu.Lock()
			p.lastAdv = time.Now()
			p.mu.Unlock()
			batch = append(batch, a)
			if len(batch) >= 16 {
				p.api.Adverts(batch)
				batch = nil
			}
		case <-tick.C:
			if len(batch) > 0 {
				p.api.Adverts(batch)
				batch = nil
			}
		}
	}
}

// watchdog restarts scanning (or the whole chip) when adverts stop arriving.
func (p *Proxy) watchdog() {
	for range time.Tick(15 * time.Second) {
		if p.connecting.Load() || p.initing.Load() {
			continue // scanning is paused while a link is set up (up to a minute), or Init is still running
		}
		p.mu.Lock()
		quiet := time.Since(p.lastAdv)
		p.mu.Unlock()
		if quiet < 60*time.Second && !p.needInit.Load() {
			continue
		}
		if p.needInit.Load() {
			logf("setting the chip up again after its reboot")
		} else {
			logf("no adverts for %v, restarting chip", quiet.Round(time.Second))
		}
		p.dropAll(0x23e)   // as "connection failed to be established"
		p.connectMu.Lock() // no le_gap_connect may go out while the chip resets
		p.dropAll(0x23e)   // a connect that slipped in before the lock was taken
		err := p.Init()
		p.connectMu.Unlock()
		if err != nil {
			logf("re-init failed: %v", err)
			gpioReset()
		}
	}
}

// ---------- connections ----------

func (p *Proxy) dropAll(reason int32) {
	p.mu.Lock()
	conns := make([]*Conn, 0, len(p.conns))
	for _, c := range p.conns {
		conns = append(conns, c)
	}
	clear(p.opened)
	p.mu.Unlock()
	for _, c := range conns {
		c.cancelConnect()
		p.dropConn(c, reason)
	}
}

func (c *Conn) cancelConnect() { c.cancelOnce.Do(func() { close(c.cancel) }) }

func (p *Proxy) ConnectionsFree() (free, limit int, allocated []uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for a := range p.conns {
		allocated = append(allocated, a)
	}
	return p.cfg.MaxConn - len(p.conns), p.cfg.MaxConn, allocated
}

func (p *Proxy) Connect(addr uint64, addrType byte, cached bool) {
	p.mu.Lock()
	if c, ok := p.conns[addr]; ok {
		state, mtu := c.state, c.mtu
		p.mu.Unlock()
		// stClosing: the old link's close (at most 5 s) answers with connected=false, and the client retries
		if state == stEstablished {
			p.api.DeviceConnection(addr, true, mtu, 0)
		}
		return
	}
	if len(p.conns) >= p.cfg.MaxConn {
		p.mu.Unlock()
		logf("%s: no free connection slot", macString(addr))
		p.api.DeviceConnection(addr, false, 0, 0)
		return
	}
	c := &Conn{
		addr: addr, addrType: addrType, cached: cached, state: stConnecting, mtu: 23, bonding: 0xff,
		link: make(chan Packet, 2), mtuEv: make(chan struct{}, 1), closed: make(chan struct{}), lost: make(chan struct{}),
		cancel: make(chan struct{}),
	}
	p.conns[addr] = c
	p.mu.Unlock()
	p.api.ConnectionsFree()
	go p.connect(c)
}

// connect sets up one link with scanning paused: the chip refuses to connect while discovering,
// and a link that has to share the radio with a full-duty scan right away drops (0x23e).
func (p *Proxy) connect(c *Conn) {
	p.connectMu.Lock()
	defer p.connectMu.Unlock()
	p.connecting.Store(true)
	defer p.connecting.Store(false)
	select {
	case <-c.cancel: // the client gave up while this connect waited for another one
		p.dropConn(c, 0)
		return
	default:
	}
	p.scanMu.Lock()
	p.stopScanLocked()
	// a link that fails right after opening (0x23e, "failed to be established") usually works on a retry
	var err int32
	for try := 1; try <= 3; try++ {
		if err = p.setupLink(c); err != 0x23e {
			break
		}
		logf("%s: link failed to establish, retry %d", macString(c.addr), try)
		p.mu.Lock()
		cancelled := false
		select {
		case <-c.cancel:
			cancelled = true
		default:
		}
		if p.conns[c.addr] != c || c.state == stClosing || cancelled {
			p.mu.Unlock()
			break
		}
		delete(p.byHandle, c.handle)
		c.state = stConnecting
		c.link = make(chan Packet, 2)
		c.mtuEv = make(chan struct{}, 1)
		c.lost = make(chan struct{})
		c.lostReason = 0
		p.mu.Unlock()
	}
	if err := p.startScanLocked(); err != nil {
		logf("resume scan: %v", err)
	}
	p.scanMu.Unlock()
	if err != 0 {
		logf("%s: connect failed: 0x%x", macString(c.addr), err)
		switch err {
		case errNotConnected: // already dropped and reported
			p.closeLink(c)
			p.dropConn(c, 0)
		case errCancelled: // the client asked for the disconnect: report a plain one
			p.closeLink(c)
			p.dropConn(c, 0)
		default:
			p.closeLink(c)
			p.dropConn(c, err)
		}
		return
	}
	p.mu.Lock()
	lost := c.lostReason
	select {
	case <-c.cancel:
		if lost == 0 {
			lost = errCancelled
		}
	default:
	}
	if p.conns[c.addr] != c || c.state == stClosing {
		// dropped or being disconnected meanwhile: whoever did that reports it
		dropped := p.conns[c.addr] != c
		p.mu.Unlock()
		if dropped {
			p.closeLink(c)
			p.dropConn(c, 0)
		}
		return
	}
	if lost == 0 {
		c.state = stEstablished
	}
	mtu := c.mtu
	p.mu.Unlock()
	if lost != 0 { // the link went down (or the client gave up) between setup and here
		if lost == errCancelled {
			p.closeLink(c)
		}
		p.dropConn(c, 0)
		return
	}
	p.api.DeviceConnection(c.addr, true, mtu, 0)
	p.api.ConnectionsFree()
}

// setupLink opens the link, waits for the MTU exchange and discovers the services unless the client has them.
func (p *Proxy) setupLink(c *Conn) int32 {
	if err := p.openLink(c); err != 0 {
		return err
	}
	logf("%s: link up (conn %d)", macString(c.addr), c.handle)
	go p.secureOnConnect(c)
	p.mu.Lock()
	mtuEv, lost, link := c.mtuEv, c.lost, c.link
	p.mu.Unlock()
	select {
	case <-mtuEv:
	case <-time.After(1500 * time.Millisecond):
	case ev := <-link: // closed right after opened, before setup listened on c.lost
		if ev.ID == 0x01 && len(ev.Payload) >= 2 {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.byHandle[c.handle] == c {
				delete(p.byHandle, c.handle) // the link is gone: nothing to close any more
			}
			if c.lostReason == 0 {
				c.lostReason = int32(le16(ev.Payload))
			}
			return c.lostReason
		}
	case <-lost:
		p.mu.Lock()
		defer p.mu.Unlock()
		return c.lostReason
	case <-c.closed:
		return errNotConnected
	case <-c.cancel:
		return errCancelled
	}
	if !c.cached {
		if err := p.discover(c); err != 0 {
			p.mu.Lock()
			lost := c.lostReason
			p.mu.Unlock()
			if lost != 0 {
				return lost
			}
			return err
		}
	}
	return 0
}

func (p *Proxy) openLink(c *Conn) int32 {
	r, res, err := p.bg.CmdResult(0x03, 0x1a, append(u64ToMac(c.addr), c.addrType, 1)) // le_gap_connect
	if err != nil {
		return 0x100
	}
	if res != 0 || len(r) < 3 {
		return int32(res)
	}
	p.mu.Lock()
	c.handle = r[2]
	p.byHandle[c.handle] = c
	if ev, ok := p.opened[c.handle]; ok { // the link opened before this goroutine got the response
		delete(p.opened, c.handle)
		nonBlock(c.link, ev)
	}
	p.mu.Unlock()
	select {
	case <-c.cancel:
		p.bg.CmdResult(0x08, 0x04, []byte{c.handle})
		select {
		case <-c.link:
		case <-time.After(3 * time.Second):
		}
		p.bg.CmdResult(0x03, 0x03, nil)
		return errCancelled
	case ev := <-c.link:
		if ev.ID == 0x00 {
			p.mu.Lock()
			c.state = stConnected
			p.mu.Unlock()
			return 0
		}
		return int32(le16(ev.Payload))
	case <-time.After(20 * time.Second):
		// cancel the pending connection; the chip also needs end_procedure, else it stays in "wrong state"
		p.bg.CmdResult(0x08, 0x04, []byte{c.handle})
		select {
		case <-c.link:
		case <-time.After(3 * time.Second):
		}
		p.bg.CmdResult(0x03, 0x03, nil)
		return 0x208 // connection timeout
	}
}

func (p *Proxy) Disconnect(addr uint64) {
	p.mu.Lock()
	c, ok := p.conns[addr]
	connecting := ok && c.state == stConnecting
	if ok && !connecting {
		c.state = stClosing
	}
	if connecting {
		c.cancelConnect() // under p.mu: connect's final check cannot slip past it
	}
	p.mu.Unlock()
	if !ok {
		p.api.DeviceConnection(addr, false, 0, 0)
		p.api.ConnectionsFree()
		return
	}
	if connecting {
		return // connect cancels the pending link, frees the slot and reports the disconnect
	}
	if _, res, err := p.bg.CmdResult(0x08, 0x04, []byte{c.handle}); err != nil || res != 0 {
		p.dropConn(c, 0)
		return
	}
	select {
	case <-c.closed:
	case <-time.After(5 * time.Second):
		p.dropConn(c, 0)
	}
}

// closeLink closes an open link and waits for the chip to confirm.
func (p *Proxy) closeLink(c *Conn) {
	if p.connByHandle(c.handle) != c {
		return
	}
	p.mu.Lock()
	link, lost := c.link, c.lost
	p.mu.Unlock()
	if _, res, err := p.bg.CmdResult(0x08, 0x04, []byte{c.handle}); err != nil || res != 0 {
		return
	}
	// the closed event goes to c.link while connecting, to c.lost during setup, and to dropConn (c.closed) after
	select {
	case <-c.closed:
	case <-lost:
	case <-link:
	case <-time.After(5 * time.Second):
	}
}

// dropConn forgets a connection and tells the clients.
func (p *Proxy) dropConn(c *Conn, reason int32) {
	p.mu.Lock()
	stale := false
	if p.byHandle[c.handle] == c {
		delete(p.byHandle, c.handle)
		stale = p.conns[c.addr] != c
	}
	if p.conns[c.addr] != c {
		idle := stale && len(p.byHandle) == 0
		p.mu.Unlock()
		if idle { // the last open link was a stale one: back to full-duty scanning
			go func() {
				if err := p.startScan(); err != nil {
					logf("rescan: %v", err)
				}
			}()
		}
		return
	}
	delete(p.conns, c.addr)
	close(c.closed)
	idle := len(p.byHandle) == 0
	p.mu.Unlock()
	if idle {
		go func() { // back to full-duty scanning
			if err := p.startScan(); err != nil {
				logf("rescan: %v", err)
			}
		}()
	}
	logf("%s: disconnected (0x%x)", macString(c.addr), reason)
	p.api.DeviceConnection(c.addr, false, 0, reason)
	p.api.ConnectionsFree()
}

func (p *Proxy) conn(addr uint64) *Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.conns[addr]
	if c == nil || c.state != stEstablished {
		return nil
	}
	return c
}

// ---------- GATT ----------

const (
	errNotConnected = -1
	errCancelled    = -2
)

// gattProc runs one GATT procedure and collects its events until gatt_procedure_completed.
func (p *Proxy) gattProc(c *Conn, id byte, args []byte) ([]Packet, int32) {
	c.procMu.Lock()
	defer c.procMu.Unlock()
	ch := make(chan Packet, 128)
	p.mu.Lock()
	c.proc = ch
	lost := c.lost
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		c.proc = nil
		p.mu.Unlock()
	}()
	_, res, err := p.bg.CmdResult(0x09, id, append([]byte{c.handle}, args...))
	if err != nil {
		return nil, 0x100
	}
	if res != 0 {
		return nil, int32(res)
	}
	var evs []Packet
	timeout := time.After(15 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.ID == 0x06 { // procedure_completed: connection, result
				if len(ev.Payload) < 3 {
					return nil, 0x100
				}
				return evs, int32(le16(ev.Payload[1:]))
			}
			evs = append(evs, ev)
		case <-c.closed:
			return nil, errNotConnected
		case <-lost:
			return nil, errNotConnected
		case <-c.cancel:
			return nil, errCancelled
		case <-timeout:
			return nil, 0x100
		}
	}
}

func (p *Proxy) discover(c *Conn) int32 {
	evs, res := p.gattProc(c, 0x01, nil) // discover_primary_services
	if res != 0 {
		return res
	}
	var svcs []Service
	var svcRefs []uint32
	for _, ev := range evs {
		pl := ev.Payload
		if ev.ID != 0x01 || len(pl) < 6 || len(pl) < 6+int(pl[5]) {
			continue
		}
		svcs = append(svcs, Service{UUID: clone(pl[6 : 6+int(pl[5])])})
		svcRefs = append(svcRefs, le32(pl[1:]))
	}
	for i := range svcs {
		evs, res := p.gattProc(c, 0x03, put32(svcRefs[i])) // discover_characteristics
		if res != 0 {
			return res
		}
		for _, ev := range evs {
			pl := ev.Payload
			if ev.ID != 0x02 || len(pl) < 5 || len(pl) < 5+int(pl[4]) {
				continue
			}
			svcs[i].Chars = append(svcs[i].Chars, Char{Handle: le16(pl[1:]), Props: pl[3], UUID: clone(pl[5 : 5+int(pl[4])])})
		}
		for j := range svcs[i].Chars {
			ch := &svcs[i].Chars[j]
			// Clients only need descriptors for the CCCD; looking them up costs ~0.4 s per characteristic.
			if ch.Props&0x30 == 0 { // neither notify nor indicate
				continue
			}
			evs, res := p.gattProc(c, 0x06, put16(ch.Handle)) // discover_descriptors
			if res != 0 {
				return res
			}
			for _, ev := range evs {
				pl := ev.Payload
				if ev.ID != 0x03 || len(pl) < 4 || len(pl) < 4+int(pl[3]) {
					continue
				}
				ch.Descs = append(ch.Descs, Desc{Handle: le16(pl[1:]), UUID: clone(pl[4 : 4+int(pl[3])])})
			}
		}
		// The chip's service reference is opaque; the declaration handle sits two below the first value handle.
		svcs[i].Handle = uint16(svcRefs[i])
		if len(svcs[i].Chars) > 0 {
			svcs[i].Handle = svcs[i].Chars[0].Handle - 2
		}
	}
	p.mu.Lock()
	c.services = svcs
	p.mu.Unlock()
	return 0
}

func (p *Proxy) Services(addr uint64) ([]Service, int32) {
	c := p.conn(addr)
	if c == nil {
		return nil, errNotConnected
	}
	p.mu.Lock()
	svcs := c.services
	p.mu.Unlock()
	if svcs != nil {
		return svcs, 0
	}
	if res := p.discover(c); res != 0 {
		return nil, res
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return c.services, 0
}

func (p *Proxy) Read(addr uint64, handle uint16, descriptor bool) ([]byte, int32) {
	c := p.conn(addr)
	if c == nil {
		return nil, errNotConnected
	}
	var evs []Packet
	var res int32
	if descriptor {
		evs, res = p.gattProc(c, 0x0e, put16(handle)) // read_descriptor_value
	} else {
		evs, res = p.gattProc(c, 0x07, put16(handle)) // read_characteristic_value
	}
	if res != 0 {
		return nil, res
	}
	var data []byte
	for _, ev := range evs {
		pl := ev.Payload
		switch {
		case ev.ID == 0x04 && len(pl) >= 7: // characteristic_value: conn, char, opcode, offset, len, data
			data = append(data, pl[7:]...)
		case ev.ID == 0x05 && len(pl) >= 6: // descriptor_value: conn, desc, offset, len, data
			data = append(data, pl[6:]...)
		}
	}
	return data, 0
}

func (p *Proxy) Write(addr uint64, handle uint16, data []byte, response, descriptor bool) int32 {
	c := p.conn(addr)
	if c == nil {
		return errNotConnected
	}
	if len(data) > 255 { // BGAPI uint8array; long writes would need prepare/execute
		return 0x0d // ATT: invalid attribute value length
	}
	args := append(put16(handle), byte(len(data)))
	args = append(args, data...)
	if descriptor {
		_, res := p.gattProc(c, 0x0f, args) // write_descriptor_value
		return res
	}
	if !response {
		c.procMu.Lock() // the chip rejects commands on a link with a procedure running
		defer c.procMu.Unlock()
		_, res, err := p.bg.CmdResult(0x09, 0x0a, append([]byte{c.handle}, args...)) // write_without_response
		if err != nil {
			return 0x100
		}
		return int32(res)
	}
	_, res := p.gattProc(c, 0x09, args) // write_characteristic_value
	return res
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }

// ---------- addresses ----------

// BGAPI addresses are little endian; ESPHome uses the MAC as a big endian uint64.
func macToU64(b []byte) uint64 {
	var v uint64
	for i := 5; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}

func u64ToMac(v uint64) []byte {
	b := make([]byte, 6)
	for i := range b {
		b[i] = byte(v >> (8 * i))
	}
	return b
}

func macString(v uint64) string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", byte(v>>40), byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
