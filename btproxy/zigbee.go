package main

// "gw3-btproxy zigbee": the host of the gateway's Zigbee chip while it is a router in the ZHA network. It brings the
// stored network up (every connection to the chip resets it), adds two endpoints and answers them, so ZHA sees a
// named, online device:
//
//	endpoint 1, Color Dimmable Light: the gateway's LED
//	  Basic (0x0000)          name, model, power source, version
//	  Identify (0x0003)       blinks the LED
//	  On/Off (0x0006)         on: the last colour (white at first), off: dark
//	  Level Control (0x0008)  accepted, but the LED has no brightness: any level above 0 is on
//	  Color Control (0x0300)  XY; the nearest of the LED's 7 colours
//	endpoint 2, On/Off Switch: the gateway's button
//	  On/Off client           a short press sends Toggle to the coordinator and to bound devices
//
// (The button has its own endpoint, marked as a switch: on the light's endpoint ZHA adds an "Opening" sensor for it.)
//
// It runs on the gateway itself, so all of this keeps working without WiFi or Home Assistant. The integration starts
// and stops it, and stops it before it opens the chip itself (one connection at a time on TCP 8888).

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	zbEndpoint   = 1 // the LED
	zbButtonEP   = 2 // the button
	zbProfileHA  = 0x0104
	zbDeviceType = 0x0102 // Color Dimmable Light
	zbSwitchType = 0x0000 // On/Off Switch
	zbManuf      = "Xiaomi"
	zbModel      = "Gateway 3 Zigbee router"
	zbExitFatal  = 2  // given up: the integration starts it again only after a backoff (4 min, doubling up to 1 h)
	zbBindings   = 64 // binding table entries looked at for the button, at most (it stops at the table's end)

	clBasic    = 0x0000
	clIdentify = 0x0003
	clOnOff    = 0x0006
	clLevel    = 0x0008
	clColor    = 0x0300

	zclReadAttrs      = 0x00
	zclReadAttrsRsp   = 0x01
	zclWriteAttrs     = 0x02
	zclWriteAttrsRsp  = 0x04
	zclConfigReport   = 0x06
	zclConfigRepRsp   = 0x07
	zclReportAttrs    = 0x0A
	zclDefaultRsp     = 0x0B
	zclSuccess        = 0x00
	zclUnsupCluster   = 0xC3
	zclUnsupGeneral   = 0x82
	zclUnsupCmd       = 0x81
	zclUnsupAttr      = 0x86
	zclReadOnly       = 0x88
	apsRetryDiscovery = 0x0140 // APS retry + enable route discovery
)

// fatalError ends the program: the chip holds no network, or not the router of the expected one.
type fatalError struct{ error }

type zigbeeHost struct {
	addr      string
	statusFn  string
	want      NetParams
	checkNet  bool
	ezsp      *EZSP
	ieee      uint64
	nwk       uint16
	tc        uint64 // the coordinator's IEEE address (0: unknown)
	seq       byte   // ZCL sequence numbers for frames this host starts
	led       *gatewayLED
	button    chan struct{}
	statusMu  sync.Mutex
	lastState string
}

func zigbeeMain(args []string) {
	fs := flag.NewFlagSet("zigbee", flag.ExitOnError)
	addr := fs.String("tcp", "127.0.0.1:8888", "the chip, as openmiio_agent serves it")
	status := fs.String("status", "/tmp/gw3-zigbee.status", "status file for the integration")
	epid := fs.String("epid", "", "extended PAN id the chip must route for (aa:bb:..), as ZHA has it")
	pan := fs.Int("pan", -1, "PAN id the chip must route for")
	channel := fs.Int("channel", 0, "channel the chip must route on")
	button := fs.String("button", "/dev/input/event0", "the gateway's button (empty: none)")
	fs.BoolVar(&verbose, "v", false, "debug logging")
	_ = fs.Parse(args)

	debug.SetGCPercent(50)
	debug.SetMemoryLimit(5 << 20)
	h := &zigbeeHost{addr: *addr, statusFn: *status, led: newGatewayLED(), button: make(chan struct{}, 4)}
	if *epid != "" || *pan >= 0 || *channel != 0 {
		e, err := parseEPID(*epid)
		if err != nil || *pan < 0 || *pan > 0xFFFF || *channel < 11 || *channel > 26 {
			fmt.Fprintln(os.Stderr, "zigbee: -epid, -pan and -channel go together and must be valid")
			os.Exit(1)
		}
		h.want, h.checkNet = NetParams{NodeType: emberNodeTypeRouter, ExtPanID: e, PanID: uint16(*pan), Channel: byte(*channel)}, true
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		h.setStatus("stopped")
		os.Exit(0)
	}()
	if *button != "" {
		go watchButton(*button, h.button)
	}
	delay := 5 * time.Second
	for {
		started := time.Now()
		err := h.session()
		var fatal fatalError
		if errors.As(err, &fatal) {
			logf("zigbee: %v", err)
			h.setStatus("refused " + oneLine(err.Error()))
			os.Exit(zbExitFatal)
		}
		if time.Since(started) > time.Minute {
			delay = 5 * time.Second
		}
		logf("zigbee: %v (again in %v)", err, delay)
		h.setStatus("error " + oneLine(err.Error()))
		time.Sleep(delay)
		delay = min(delay*2, time.Minute)
	}
}

func parseEPID(s string) (uint64, error) {
	b := strings.Split(strings.ToLower(s), ":")
	if len(b) != 8 {
		return 0, errors.New("bad extended PAN id")
	}
	var v uint64
	for _, p := range b { // most significant byte first, as zigpy prints it
		n, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return 0, err
		}
		v = v<<8 | n
	}
	return v, nil
}

func oneLine(s string) string { return strings.ReplaceAll(s, "\n", " ") }

// setStatus writes "<state> key=value..." atomically; the integration reads it over telnet.
func (h *zigbeeHost) setStatus(state string) {
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	line := fmt.Sprintf("%s pid=%d version=%s", state, os.Getpid(), version)
	if h.ieee != 0 {
		line += fmt.Sprintf(" ieee=%s nwk=0x%04X", ieeeString(h.ieee), h.nwk)
	}
	if line == h.lastState {
		return
	}
	h.lastState = line
	tmp := h.statusFn + ".tmp"
	if err := os.WriteFile(tmp, []byte(line+"\n"), 0o644); err == nil {
		_ = os.Rename(tmp, h.statusFn)
	}
}

func ieeeString(v uint64) string {
	var b [8]string
	for i := range b {
		b[i] = fmt.Sprintf("%02x", byte(v>>(8*(7-i))))
	}
	return strings.Join(b[:], ":")
}

// session runs one connection to the chip, until it fails.
func (h *zigbeeHost) session() error {
	h.setStatus("starting")
	conn, err := net.DialTimeout("tcp", h.addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	e := NewEZSP()
	a := NewASH(conn, e.Received)
	e.ash = a
	h.ezsp = e
	runErr := make(chan error, 1)
	go func() { runErr <- a.Run() }()
	if err := h.bringUp(); err != nil {
		var fatal fatalError
		if errors.As(err, &fatal) {
			// the chip may have just brought up a network it must not run (a coordinator, another network):
			// reset it, so that network is down again
			if rerr := a.Reset(2 * time.Second); rerr != nil {
				logf("zigbee: could not reset the chip: %v", rerr)
			}
		}
		return err
	}
	h.setStatus("up")
	logf("zigbee: router 0x%04X (%s) up, endpoints %d and %d", h.nwk, ieeeString(h.ieee), zbEndpoint, zbButtonEP)
	for len(h.button) > 0 { // presses while the chip was not up: too late now
		<-h.button
	}
	h.announce()
	for {
		select {
		case err := <-runErr:
			return fmt.Errorf("chip link: %w", err)
		case <-a.Lost:
			return fmt.Errorf("chip link: %w", a.err())
		case cb := <-e.Callbacks:
			if err := h.callback(cb); err != nil {
				return err
			}
		case <-h.button:
			h.buttonPressed()
		}
	}
}

func (h *zigbeeHost) bringUp() error {
	e := h.ezsp
	if err := e.ash.Reset(2 * time.Second); err != nil {
		return err
	}
	proto, stackType, stack, err := e.Version()
	if err != nil {
		return err
	}
	debugf("zigbee: EZSP %d, stack type %d, stack 0x%04X", proto, stackType, stack)
	if proto != ezspProtocolVersion {
		return fatalError{fmt.Errorf("the chip speaks EZSP %d, not %d", proto, ezspProtocolVersion)}
	}
	// as the integration does before every networkInit: otherwise the chip answers NOT_JOINED
	if err := e.SetConfig(ezspConfigStackProf, 2); err != nil {
		return err
	}
	if err := e.SetConfig(ezspConfigSecLevel, 5); err != nil {
		return err
	}
	// All free configuration RAM to packet buffers, set last (as bellows does). With the chip's default, messages
	// it sends are garbled after their first 11 bytes (seen on this chip: the rest is stale buffer memory).
	if err := e.SetConfig(ezspConfigPacketBufs, 0xFF); err != nil {
		logf("zigbee: %v (messages from the chip may be garbled)", err)
	}
	if err := e.AddEndpoint(zbEndpoint, zbProfileHA, zbDeviceType, []uint16{clBasic, clIdentify, clOnOff, clLevel, clColor}, nil); err != nil {
		return err
	}
	if err := e.AddEndpoint(zbButtonEP, zbProfileHA, zbSwitchType, nil, []uint16{clOnOff}); err != nil {
		return err
	}
	if h.ieee, err = e.Eui64(); err != nil {
		return err
	}
	for len(e.Callbacks) > 0 {
		<-e.Callbacks
	}
	if err := e.NetworkInit(); err != nil {
		if isStatus(err, emberNotJoined) {
			return fatalError{errors.New("the chip has no network stored")}
		}
		return err
	}
	if err := h.waitStack(emberNetworkUp, 30*time.Second); err != nil {
		return err
	}
	p, err := e.NetworkParams()
	if err != nil {
		return err
	}
	if p.NodeType != emberNodeTypeRouter {
		return fatalError{fmt.Errorf("the chip is not a router (node type %d)", p.NodeType)}
	}
	if h.checkNet && (p.ExtPanID != h.want.ExtPanID || p.PanID != h.want.PanID || p.Channel != h.want.Channel) {
		return fatalError{fmt.Errorf("the chip routes for another network (%s, PAN 0x%04X, channel %d)", ieeeString(p.ExtPanID), p.PanID, p.Channel)}
	}
	if h.tc, err = e.TrustCenter(); err != nil {
		logf("zigbee: %v (a button press may reach ZHA twice)", err)
	}
	h.nwk, err = e.NodeID()
	return err
}

func (h *zigbeeHost) waitStack(want byte, timeout time.Duration) error {
	t := time.After(timeout)
	for {
		select {
		case cb := <-h.ezsp.Callbacks:
			if cb.ID == ezspStackStatus && len(cb.Data) > 0 {
				if cb.Data[0] == want {
					return nil
				}
				debugf("zigbee: stack status 0x%02X", cb.Data[0])
			}
		case <-t:
			return fmt.Errorf("no stack status 0x%02X within %v", want, timeout)
		case <-h.ezsp.ash.done:
			return errASHClosed
		}
	}
}

func (h *zigbeeHost) callback(cb Callback) error {
	switch cb.ID {
	case ezspStackStatus:
		if len(cb.Data) > 0 && cb.Data[0] == emberNetworkDown {
			state, err := h.ezsp.NetworkState()
			if err == nil && state == 0 {
				// a leave (ZHA removed the device): do not bring the network back up
				return fatalError{errors.New("the chip left the network")}
			}
			return fmt.Errorf("network down (state %d, %v)", state, err) // connect again: networkInit brings it up
		}
	case ezspIncomingMessage:
		if in, ok := parseIncoming(cb.Data); ok {
			h.incoming(in)
		}
	}
	return nil
}

// announce sends a ZDO Device_annce, so ZHA notes that the router is there (and its last seen time).
func (h *zigbeeHost) announce() {
	msg := binary.LittleEndian.AppendUint16([]byte{h.nextSeq()}, h.nwk)
	msg = binary.LittleEndian.AppendUint64(msg, h.ieee)
	msg = append(msg, 0x8E) // capability: full function device, mains powered, receiver on when idle, allocate address
	aps := APSFrame{Profile: 0, Cluster: 0x0013, Options: 0}
	if err := h.ezsp.SendBroadcast(0xFFFD, aps, 0, msg); err != nil {
		logf("zigbee: device announce: %v", err)
	}
}

func (h *zigbeeHost) nextSeq() byte {
	h.seq++
	return h.seq
}

// ---------- ZCL ----------

type zclFrame struct {
	fc, seq, cmd byte
	manuf        bool
	payload      []byte
}

func parseZCL(b []byte) (zclFrame, bool) {
	if len(b) < 3 {
		return zclFrame{}, false
	}
	f := zclFrame{fc: b[0]}
	i := 1
	if f.fc&0x04 != 0 {
		if len(b) < 5 {
			return zclFrame{}, false
		}
		f.manuf, i = true, 3
	}
	f.seq, f.cmd, f.payload = b[i], b[i+1], b[i+2:]
	return f, true
}

func (h *zigbeeHost) incoming(in Incoming) {
	if (in.APS.DstEP != zbEndpoint && in.APS.DstEP != zbButtonEP) || in.APS.Profile != zbProfileHA {
		return
	}
	f, ok := parseZCL(in.Msg)
	if !ok || f.fc&0x08 != 0 { // too short, or sent server to client: not for us to answer
		return
	}
	debugf("zigbee: from 0x%04X cluster 0x%04X cmd 0x%02X % x", in.Sender, in.APS.Cluster, f.cmd, f.payload)
	unicast := in.Type <= 1 // group and broadcast frames get no answer
	reply := func(fc, cmd byte, payload []byte) {
		if unicast {
			h.send(in.APS.DstEP, in.Sender, in.APS.SrcEP, in.APS.Cluster, append([]byte{fc, f.seq, cmd}, payload...))
		}
	}
	status := byte(zclSuccess)
	cluster := in.APS.Cluster
	known := in.APS.DstEP == zbEndpoint && // the button's endpoint has no server clusters
		(cluster == clBasic || cluster == clIdentify || cluster == clOnOff || cluster == clLevel || cluster == clColor)
	switch {
	case f.manuf:
		status = zclUnsupCmd
		if f.fc&0x03 == 0 {
			status = zclUnsupGeneral
		}
	case !known:
		status = zclUnsupCluster
	case f.fc&0x03 == 0: // profile-wide
		switch f.cmd {
		case zclReadAttrs:
			reply(0x18, zclReadAttrsRsp, h.readAttrs(cluster, f.payload))
			return
		case zclWriteAttrs:
			reply(0x18, zclWriteAttrsRsp, h.writeAttrs(cluster, f.payload))
			return
		case zclConfigReport:
			// Reports are sent on every change made over Zigbee; accept the configuration (one record: all OK)
			reply(0x18, zclConfigRepRsp, []byte{zclSuccess})
			return
		case zclDefaultRsp:
			return
		default:
			status = zclUnsupGeneral
		}
	default: // cluster-specific
		status = h.clusterCommand(cluster, f)
	}
	if f.fc&0x10 == 0 || status != zclSuccess {
		reply(0x18, zclDefaultRsp, []byte{f.cmd, status})
	}
}

func (h *zigbeeHost) clusterCommand(cluster uint16, f zclFrame) byte {
	switch cluster {
	case clIdentify:
		switch f.cmd {
		case 0x00: // Identify(time)
			if len(f.payload) < 2 {
				return 0x80 // malformed command
			}
			h.led.Identify(time.Duration(binary.LittleEndian.Uint16(f.payload)) * time.Second)
			return zclSuccess
		case 0x40: // Trigger effect
			h.led.Identify(2 * time.Second)
			return zclSuccess
		}
	case clOnOff:
		var on bool
		switch f.cmd {
		case 0x00:
		case 0x01:
			on = true
		case 0x02:
			on = !h.led.On()
		default:
			return zclUnsupCmd
		}
		h.led.Set(on)
		h.reportOnOff(on)
		return zclSuccess
	case clLevel:
		switch f.cmd {
		case 0x00, 0x04: // Move to level (with on/off): no brightness, so only level 0 means off
			if len(f.payload) < 1 {
				return 0x80
			}
			if on := f.payload[0] > 0; f.cmd == 0x04 || !on {
				if f.cmd == 0x04 || h.led.On() != on {
					h.led.Set(on)
					h.reportOnOff(on)
				}
			}
			return zclSuccess
		case 0x01, 0x02, 0x03, 0x05, 0x06, 0x07: // move, step, stop: nothing to dim
			return zclSuccess
		}
	case clColor:
		switch f.cmd {
		case 0x07: // Move to color (x, y, transition)
			if len(f.payload) < 4 {
				return 0x80
			}
			h.led.SetColour(xyToPins(binary.LittleEndian.Uint16(f.payload), binary.LittleEndian.Uint16(f.payload[2:])))
			return zclSuccess
		case 0x47: // Stop move step
			return zclSuccess
		}
	}
	return zclUnsupCmd
}

// xyToPins is the LED colour nearest to a CIE xy colour: each channel at half its brightest or more (gamma encoded,
// as colours are picked) is lit. RGB <-> XYZ with the matrices Home Assistant uses (homeassistant/util/color.py,
// color_xy_to_RGB and color_RGB_to_xy), so a colour picked there lands where Home Assistant expects.
func xyToPins(x, y uint16) [3]bool {
	fx, fy := float64(x)/65535, float64(y)/65535
	if fy <= 0 {
		return [3]bool{true, true, true}
	}
	X, Z := fx/fy, (1-fx-fy)/fy
	rgb := [3]float64{
		1.656492*X - 0.354851 - 0.255038*Z,
		-0.707196*X + 1.655397 + 0.036152*Z,
		0.051713*X - 0.121364 + 1.011530*Z,
	}
	m := max(rgb[0], rgb[1], rgb[2])
	if m <= 0 {
		return [3]bool{true, true, true}
	}
	lit := func(c float64) bool {
		c = max(c, 0) / m
		if c > 0.0031308 {
			c = 1.055*math.Pow(c, 1/2.4) - 0.055
		} else {
			c *= 12.92
		}
		return c >= 0.5
	}
	return [3]bool{lit(rgb[0]), lit(rgb[1]), lit(rgb[2])}
}

// pinsToXY is the CIE xy colour of the lit LED colours (Home Assistant's matrix, as above), as ZCL's 0..65535 values.
func pinsToXY(c [3]bool) (uint16, uint16) {
	v := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	r, g, b := v(c[0]), v(c[1]), v(c[2])
	X := 0.664511*r + 0.154324*g + 0.162028*b
	Y := 0.283881*r + 0.668433*g + 0.047685*b
	Z := 0.000088*r + 0.072310*g + 0.986039*b
	if X+Y+Z == 0 {
		return pinsToXY([3]bool{true, true, true})
	}
	return uint16(X / (X + Y + Z) * 65535), uint16(Y / (X + Y + Z) * 65535)
}

// attribute values: ZCL type, then the encoded value
func (h *zigbeeHost) attr(cluster, id uint16) (byte, []byte, bool) {
	str := func(s string) []byte { return append([]byte{byte(len(s))}, s...) }
	switch cluster {
	case clBasic:
		switch id {
		case 0x0000:
			return 0x20, []byte{3}, true // ZCL version
		case 0x0001:
			return 0x20, []byte{1}, true // application version
		case 0x0003:
			return 0x20, []byte{1}, true // hardware version
		case 0x0004:
			return 0x42, str(zbManuf), true
		case 0x0005:
			return 0x42, str(zbModel), true
		case 0x0007:
			return 0x30, []byte{1}, true // power source: mains (single phase)
		case 0x4000:
			return 0x42, str(version), true // software build id
		}
	case clIdentify:
		if id == 0x0000 {
			return 0x21, binary.LittleEndian.AppendUint16(nil, uint16(h.led.IdentifyLeft()/time.Second)), true
		}
	case clOnOff:
		if id == 0x0000 {
			if h.led.On() {
				return 0x10, []byte{1}, true
			}
			return 0x10, []byte{0}, true
		}
	case clLevel:
		if id == 0x0000 { // current level: always full
			return 0x20, []byte{254}, true
		}
	case clColor:
		u16 := func(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }
		x, y := pinsToXY(h.led.Colour())
		switch id {
		case 0x0003:
			return 0x21, u16(x), true // current x
		case 0x0004:
			return 0x21, u16(y), true // current y
		case 0x0008, 0x4001:
			return 0x30, []byte{1}, true // (enhanced) color mode: xy
		case 0x000F:
			return 0x18, []byte{0}, true // options
		case 0x400A:
			return 0x19, u16(0x0008), true // color capabilities: xy only
		}
	}
	return 0, nil, false
}

func (h *zigbeeHost) readAttrs(cluster uint16, p []byte) []byte {
	var out []byte
	for i := 0; i+1 < len(p); i += 2 {
		id := binary.LittleEndian.Uint16(p[i:])
		out = binary.LittleEndian.AppendUint16(out, id)
		if typ, val, ok := h.attr(cluster, id); ok {
			out = append(append(out, zclSuccess, typ), val...)
		} else {
			out = append(out, zclUnsupAttr)
		}
	}
	return out
}

func (h *zigbeeHost) writeAttrs(cluster uint16, p []byte) []byte {
	// Identify time is the only writable attribute; everything else is read-only or unknown.
	var out []byte
	ok := true
	for i := 0; i+2 < len(p); {
		id, typ := binary.LittleEndian.Uint16(p[i:]), p[i+2]
		n := zclTypeLen(typ, p[i+3:])
		if n < 0 || i+3+n > len(p) {
			break
		}
		val := p[i+3 : i+3+n]
		i += 3 + n
		status := byte(zclSuccess)
		switch {
		case cluster == clIdentify && id == 0x0000 && typ == 0x21:
			h.led.Identify(time.Duration(binary.LittleEndian.Uint16(val)) * time.Second)
		case func() bool { _, _, known := h.attr(cluster, id); return known }():
			status = zclReadOnly
		default:
			status = zclUnsupAttr
		}
		if status != zclSuccess {
			ok = false
			out = binary.LittleEndian.AppendUint16(append(out, status), id)
		}
	}
	if ok {
		return []byte{zclSuccess}
	}
	return out
}

// zclTypeLen is the length of a value of ZCL type typ (strings: from their length byte); -1 if not known.
func zclTypeLen(typ byte, v []byte) int {
	switch typ {
	case 0x10, 0x18, 0x20, 0x28, 0x30, 0x08:
		return 1
	case 0x19, 0x21, 0x29, 0x31, 0x09:
		return 2
	case 0x1A, 0x22, 0x2A, 0x0A:
		return 3
	case 0x1B, 0x23, 0x2B, 0x0B, 0x39:
		return 4
	case 0x41, 0x42:
		if len(v) == 0 {
			return -1
		}
		return 1 + int(v[0])
	}
	return -1
}

// send sends a ZCL frame from one of this node's endpoints to a node.
func (h *zigbeeHost) send(srcEP byte, dest uint16, dstEP byte, cluster uint16, zcl []byte) {
	aps := APSFrame{Profile: zbProfileHA, Cluster: cluster, SrcEP: srcEP, DstEP: dstEP, Options: apsRetryDiscovery}
	debugf("zigbee: ep %d to 0x%04X ep %d cluster 0x%04X: % x", srcEP, dest, dstEP, cluster, zcl)
	if err := h.ezsp.SendUnicast(emberOutgoingDirect, dest, aps, 0, zcl); err != nil {
		logf("zigbee: send to 0x%04X: %v", dest, err)
	}
}

func (h *zigbeeHost) reportOnOff(on bool) {
	v := byte(0)
	if on {
		v = 1
	}
	h.send(zbEndpoint, 0x0000, 1, clOnOff, []byte{0x18, h.nextSeq(), zclReportAttrs, 0x00, 0x00, 0x10, v})
}

// buttonPressed sends On/Off Toggle (client to server) to the coordinator, which ZHA shows as a zha_event, and to
// every other device bound to the On/Off cluster of endpoint 1, which works without Home Assistant. (ZHA may bind the
// cluster to the coordinator itself: that binding is skipped, so ZHA gets the press once.)
func (h *zigbeeHost) buttonPressed() {
	zcl := []byte{0x01, h.nextSeq(), 0x02} // cluster-specific, client to server, Toggle
	logf("zigbee: button pressed: Toggle")
	h.send(zbButtonEP, 0x0000, 1, clOnOff, zcl)
	for i := 0; i < zbBindings; i++ {
		b, err := h.ezsp.Binding(byte(i))
		if err != nil {
			var s *StatusError
			if !errors.As(err, &s) { // a status: past the end of the table
				logf("zigbee: binding %d: %v", i, err)
			}
			break
		}
		if b.Type == 0 || b.Local != zbButtonEP || b.Cluster != clOnOff {
			continue
		}
		aps := APSFrame{Profile: zbProfileHA, Cluster: clOnOff, SrcEP: zbButtonEP, DstEP: b.Remote, Options: apsRetryDiscovery}
		switch b.Type {
		case 1: // unicast: to the node id the chip knows for that address, else through the binding
			if b.Identity == h.tc {
				continue
			}
			nwk, lerr := h.ezsp.NodeIDOf(b.Identity)
			switch {
			case nwk == 0x0000:
				continue // the coordinator after all
			case lerr != nil || nwk == 0xFFFF:
				err = h.ezsp.SendUnicast(emberOutgoingBinding, uint16(i), aps, 0, zcl)
			default:
				err = h.ezsp.SendUnicast(emberOutgoingDirect, nwk, aps, 0, zcl)
			}
		case 3: // group
			aps.Group, aps.DstEP, aps.Options = uint16(b.Identity), 0xFF, 0
			err = h.ezsp.SendMulticast(aps, 0, zcl)
		}
		if err != nil {
			logf("zigbee: binding %d: %v", i, err)
		}
	}
}
