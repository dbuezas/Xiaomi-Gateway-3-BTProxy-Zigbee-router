package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func TestASHVectors(t *testing.T) {
	// RST as every EZSP host sends it (UG101): C0 38 BC 7E
	if got := append(ashEncode([]byte{ashRST}), ashFlag); !bytes.Equal(got, []byte{0xC0, 0x38, 0xBC, 0x7E}) {
		t.Fatalf("RST frame % x", got)
	}
	// RSTACK from UG101: C1 02 02 9B 7B 7E (version 2, reset code 2: power on)
	f, err := ashDecode([]byte{0xC1, 0x02, 0x02, 0x9B, 0x7B})
	if err != nil || !bytes.Equal(f, []byte{0xC1, 0x02, 0x02}) {
		t.Fatalf("RSTACK %x %v", f, err)
	}
	// the pseudo-random sequence starts 42 21 A8 54 2A (UG101)
	if got := ashRandomize(make([]byte, 5)); !bytes.Equal(got, []byte{0x42, 0x21, 0xA8, 0x54, 0x2A}) {
		t.Fatalf("random % x", got)
	}
	// stuffing round trip over every reserved byte
	in := []byte{0x00, 0x7E, 0x7D, 0x11, 0x13, 0x18, 0x1A, 0xFF}
	out, err := ashDecode(ashEncode(in))
	if err != nil || !bytes.Equal(out, in) {
		t.Fatalf("stuffing % x %v", out, err)
	}
	if bytes.ContainsAny(ashEncode(in), "\x7e\x11\x13\x18\x1a") {
		t.Fatal("reserved byte left unstuffed")
	}
}

// fakeNCP answers RST with RSTACK, acknowledges DATA frames and answers EZSP commands with handler.
type fakeNCP struct {
	t       *testing.T
	conn    net.Conn
	handler func(id byte, params []byte) []byte
	txSeq   byte
	rxSeq   byte
	sent    chan []byte // EZSP frames from the host
	dropAck int         // acknowledgements not sent (the host must retransmit)
}

func (n *fakeNCP) write(frame []byte) { _, _ = n.conn.Write(append(ashEncode(frame), ashFlag)) }

func (n *fakeNCP) data(ezsp []byte) {
	n.write(append([]byte{n.txSeq<<4 | n.rxSeq}, ashRandomize(ezsp)...))
	n.txSeq = (n.txSeq + 1) & 7
}

func (n *fakeNCP) run() {
	r := bufio.NewReader(n.conn)
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return
		}
		switch b {
		case ashCancel:
			buf = buf[:0]
			continue
		case ashFlag:
		default:
			buf = append(buf, b)
			continue
		}
		f, err := ashDecode(buf)
		buf = buf[:0]
		if err != nil {
			n.t.Errorf("fake NCP: %v", err)
			continue
		}
		c := f[0]
		switch {
		case c == ashRST:
			n.txSeq, n.rxSeq = 0, 0
			n.write([]byte{ashRSTACK, 2, 0x0B})
		case c&0x80 == 0:
			if c>>4&7 != n.rxSeq {
				continue // a retransmission of a frame already taken
			}
			if n.dropAck > 0 {
				n.dropAck--
				continue
			}
			n.rxSeq = (n.rxSeq + 1) & 7
			n.write([]byte{0x80 | n.rxSeq})
			ezsp := ashRandomize(f[1:])
			n.sent <- ezsp
			head, id, params := []byte{ezsp[0], 0x80, ezsp[2]}, ezsp[2], ezsp[3:]
			if id == 0xFF { // EZSP 5..7 frame
				head, id, params = []byte{ezsp[0], 0x80, 0xFF, 0x00, ezsp[4]}, ezsp[4], ezsp[5:]
			} else if id != ezspVersion {
				n.t.Errorf("fake NCP: legacy frame for command 0x%02X", id)
			}
			if rsp := n.handler(id, params); rsp != nil {
				n.data(append(head, rsp...))
			}
		}
	}
}

func newPair(t *testing.T, handler func(byte, []byte) []byte) (*EZSP, *fakeNCP) {
	host, chip := net.Pipe()
	t.Cleanup(func() { host.Close(); chip.Close() })
	n := &fakeNCP{t: t, conn: chip, handler: handler, sent: make(chan []byte, 64)}
	go n.run()
	e := NewEZSP()
	e.ash = NewASH(host, e.Received)
	go e.ash.Run()
	return e, n
}

func TestEZSPOverASH(t *testing.T) {
	e, n := newPair(t, func(id byte, p []byte) []byte {
		switch id {
		case ezspVersion:
			return []byte{7, 2, 0x30, 0x65}
		case ezspGetEui64:
			return binary.LittleEndian.AppendUint64(nil, 0x842e14fffe54e195)
		}
		return []byte{0}
	})
	if err := e.ash.Reset(time.Second); err != nil {
		t.Fatal(err)
	}
	proto, _, stack, err := e.Version()
	if err != nil || proto != 7 || stack != 0x6530 {
		t.Fatalf("version %d %x %v", proto, stack, err)
	}
	n.dropAck = 1 // the next frame is lost once: the host retransmits it
	ieee, err := e.Eui64()
	if err != nil || ieeeString(ieee) != "84:2e:14:ff:fe:54:e1:95" {
		t.Fatalf("eui64 %x %v", ieee, err)
	}
	// more than 8 commands: sequence numbers wrap
	for i := 0; i < 20; i++ {
		if err := e.SetConfig(ezspConfigStackProf, 2); err != nil {
			t.Fatal(i, err)
		}
	}
	// a callback in between is handed over, not taken as a reply
	n.data([]byte{0x00, 0x90, 0xFF, 0x00, ezspStackStatus, emberNetworkUp})
	select {
	case cb := <-e.Callbacks:
		if cb.ID != ezspStackStatus || cb.Data[0] != emberNetworkUp {
			t.Fatalf("callback %+v", cb)
		}
	case <-time.After(time.Second):
		t.Fatal("no callback")
	}
	// a reset nobody asked for (the chip rebooted): the link counts as lost
	n.write([]byte{ashRSTACK, 2, 0x03})
	select {
	case <-e.ash.Lost:
	case <-time.After(time.Second):
		t.Fatal("unsolicited RSTACK not noticed")
	}
}

func TestAddEndpointLayout(t *testing.T) {
	e, n := newPair(t, func(byte, []byte) []byte { return []byte{0} })
	if err := e.ash.Reset(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := e.AddEndpoint(1, 0x0104, 0x0100, []uint16{0, 3, 6}, []uint16{6}); err != nil {
		t.Fatal(err)
	}
	got := <-n.sent
	want := []byte{0, 0, 0xFF, 0, ezspAddEndpoint, 1, 0x04, 0x01, 0x00, 0x01, 0, 3, 1, 0, 0, 3, 0, 6, 0, 6, 0}
	if !bytes.Equal(got, want) {
		t.Fatalf("addEndpoint\n got % x\nwant % x", got, want)
	}
	// the button's endpoint: no input clusters
	if err := e.AddEndpoint(2, 0x0104, 0x0000, nil, []uint16{6}); err != nil {
		t.Fatal(err)
	}
	got = <-n.sent
	want = []byte{1, 0, 0xFF, 0, ezspAddEndpoint, 2, 0x04, 0x01, 0x00, 0x00, 0, 0, 1, 6, 0}
	if !bytes.Equal(got, want) {
		t.Fatalf("addEndpoint\n got % x\nwant % x", got, want)
	}
}

// TestEndpoint answers a Read Attributes of the Basic cluster as ZHA's interview sends it.
func TestEndpoint(t *testing.T) {
	e, n := newPair(t, func(id byte, p []byte) []byte { return []byte{0, 0} })
	if err := e.ash.Reset(time.Second); err != nil {
		t.Fatal(err)
	}
	h := &zigbeeHost{ezsp: e, led: newGatewayLED()}
	// incomingMessageHandler: unicast from 0x0000 ep 1 to ep 1, Basic, read manufacturer, model, 0x1234
	aps := APSFrame{Profile: 0x0104, Cluster: 0, SrcEP: 1, DstEP: 1, Seq: 9}
	zcl := []byte{0x00, 0x42, zclReadAttrs, 0x04, 0x00, 0x05, 0x00, 0x34, 0x12}
	d := aps.put([]byte{0})
	d = append(d, 0xFF, 0xC0, 0x00, 0x00, 0, 0, byte(len(zcl)))
	d = append(d, zcl...)
	in, ok := parseIncoming(d)
	if !ok || in.Sender != 0 || in.APS != aps || !bytes.Equal(in.Msg, zcl) {
		t.Fatalf("parseIncoming %+v %v", in, ok)
	}
	h.incoming(in)
	got := <-n.sent
	if got[4] != ezspSendUnicast {
		t.Fatalf("sent 0x%02X", got[4])
	}
	// type, destination, aps(11), tag, length, message
	msg := got[5+3+11+2:]
	want := []byte{0x18, 0x42, zclReadAttrsRsp, 0x04, 0x00, 0, 0x42, 6}
	want = append(want, "Xiaomi"...)
	want = append(want, 0x05, 0x00, 0, 0x42, byte(len(zbModel)))
	want = append(want, zbModel...)
	want = append(want, 0x34, 0x12, zclUnsupAttr)
	if !bytes.Equal(msg, want) {
		t.Fatalf("read response\n got % x\nwant % x", msg, want)
	}
	sentAPS, _ := parseAPS(got[8:])
	if sentAPS.DstEP != 1 || sentAPS.SrcEP != 1 || sentAPS.Cluster != 0 || sentAPS.Profile != 0x0104 {
		t.Fatalf("response APS %+v", sentAPS)
	}

	// a command for an unknown cluster gets a default response "unsupported cluster"
	in.APS.Cluster = 0x0201 // thermostat
	in.Msg = []byte{0x01, 0x43, 0x00}
	h.incoming(in)
	got = <-n.sent
	if msg := got[5+3+11+2:]; !bytes.Equal(msg, []byte{0x18, 0x43, zclDefaultRsp, 0x00, zclUnsupCluster}) {
		t.Fatalf("default response % x", msg)
	}

	// a group frame gets no answer
	in.Type = 2
	in.APS.Cluster = 0
	in.Msg = zcl
	h.incoming(in)
	select {
	case got := <-n.sent:
		t.Fatalf("answered a group frame: % x", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestReadButton(t *testing.T) {
	ev := func(typ uint16, value int32) []byte {
		b := make([]byte, 16)
		binary.LittleEndian.PutUint16(b[8:], typ)
		binary.LittleEndian.PutUint16(b[10:], 0x198)
		binary.LittleEndian.PutUint32(b[12:], uint32(value))
		return b
	}
	// as recorded on the gateway: key 102 value 0 (down), EV_SYN, value 1 (up), EV_SYN
	var in []byte
	in = append(in, ev(1, 0)...)
	in = append(in, ev(0, 0)...)
	in = append(in, ev(1, 1)...)
	in = append(in, ev(0, 0)...)
	in = append(in, ev(1, 1)...) // a stray up (the program started while the button was down): no press
	presses := make(chan struct{}, 4)
	readButton(bytes.NewReader(in), presses)
	if len(presses) != 1 {
		t.Fatalf("%d presses", len(presses))
	}
}

func TestParseEPID(t *testing.T) {
	v, err := parseEPID("a7:50:56:db:25:eb:40:84")
	if err != nil || v != 0xa75056db25eb4084 || ieeeString(v) != "a7:50:56:db:25:eb:40:84" {
		t.Fatalf("%x %v", v, err)
	}
}

func TestColours(t *testing.T) {
	// every LED colour survives the round trip through the xy ZHA shows
	for i := 1; i < 8; i++ {
		c := [3]bool{i&4 != 0, i&2 != 0, i&1 != 0}
		if got := xyToPins(pinsToXY(c)); got != c {
			t.Errorf("%v -> %v", c, got)
		}
	}
	// as Home Assistant sends them (xy x 65535): orange (255, 165, 0: xy 0.562, 0.416) is nearest to yellow, red-orange
	// (255, 80, 0: xy 0.663, 0.331) to red, blue (0, 0, 255: xy 0.136, 0.040) to blue
	if got := xyToPins(36830, 27263); got != [3]bool{true, true, false} {
		t.Errorf("orange -> %v", got)
	}
	if got := xyToPins(43449, 21679); got != [3]bool{true, false, false} {
		t.Errorf("red-orange -> %v", got)
	}
	if got := xyToPins(8913, 2621); got != [3]bool{false, false, true} {
		t.Errorf("blue -> %v", got)
	}
}
