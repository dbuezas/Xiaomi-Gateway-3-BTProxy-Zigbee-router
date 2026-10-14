package main

// EZSP version 7 (the gateway's EmberZNet NCP firmware, stack 6.6.2). Frames: sequence, frame control, 0xFF, 0x00,
// frame id; only the version command (always the first) is sequence, frame control, frame id. Only the commands
// and callbacks the Zigbee router needs. Layouts as in bellows/ezsp/v4..v7/commands.py.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	ezspVersion           = 0x00
	ezspAddEndpoint       = 0x02
	ezspNetworkInit       = 0x17
	ezspNetworkState      = 0x18
	ezspStackStatus       = 0x19 // callback
	ezspGetEui64          = 0x26
	ezspGetNodeID         = 0x27
	ezspGetNetworkParams  = 0x28
	ezspGetBinding        = 0x2C
	ezspSendUnicast       = 0x34
	ezspSendBroadcast     = 0x36
	ezspSendMulticast     = 0x38
	ezspMessageSent       = 0x3F // callback
	ezspIncomingMessage   = 0x45 // callback
	ezspSetConfigValue    = 0x53
	ezspLookupNodeID      = 0x60
	ezspSecurityState     = 0x69
	ezspInvalidCommand    = 0x58
	ezspCmdTimeout        = 5 * time.Second
	ezspProtocolVersion   = 7
	ezspConfigPacketBufs  = 0x01
	ezspConfigStackProf   = 0x0C
	ezspConfigSecLevel    = 0x0D
	emberSuccess          = 0x00
	emberNetworkUp        = 0x90
	emberNetworkDown      = 0x91
	emberNotJoined        = 0x93
	emberNodeTypeRouter   = 0x02
	emberOutgoingDirect   = 0x00
	emberOutgoingBinding  = 0x02
	emberOutgoingMulticst = 0x03
)

// APSFrame is EmberApsFrame.
type APSFrame struct {
	Profile, Cluster uint16
	SrcEP, DstEP     byte
	Options, Group   uint16
	Seq              byte
}

func (f APSFrame) put(b []byte) []byte {
	b = binary.LittleEndian.AppendUint16(b, f.Profile)
	b = binary.LittleEndian.AppendUint16(b, f.Cluster)
	b = append(b, f.SrcEP, f.DstEP)
	b = binary.LittleEndian.AppendUint16(b, f.Options)
	b = binary.LittleEndian.AppendUint16(b, f.Group)
	return append(b, f.Seq)
}

func parseAPS(b []byte) (APSFrame, bool) {
	if len(b) < 11 {
		return APSFrame{}, false
	}
	return APSFrame{
		Profile: binary.LittleEndian.Uint16(b), Cluster: binary.LittleEndian.Uint16(b[2:]),
		SrcEP: b[4], DstEP: b[5], Options: binary.LittleEndian.Uint16(b[6:]),
		Group: binary.LittleEndian.Uint16(b[8:]), Seq: b[10],
	}, true
}

// Callback is an asynchronous frame from the chip.
type Callback struct {
	ID   byte
	Data []byte
}

type ezspReply struct {
	id   byte
	data []byte
}

// EZSP sends one command at a time and hands callbacks to a channel.
type EZSP struct {
	ash       *ASH
	cmdMu     sync.Mutex
	mu        sync.Mutex
	seq       byte
	wait      chan ezspReply // the reply to the command in flight
	waitSeq   byte
	waiting   bool
	Callbacks chan Callback
}

func NewEZSP() *EZSP {
	return &EZSP{Callbacks: make(chan Callback, 32)}
}

// Received is the ASH recv callback.
func (e *EZSP) Received(f []byte) {
	if len(f) < 3 {
		return
	}
	seq, fc, id, data := f[0], f[1], f[2], f[3:]
	if id == 0xFF { // EZSP 5..7 after the version command: 0xFF, 0x00 (frame control high byte), then the id
		if len(f) < 5 {
			return
		}
		id, data = f[4], f[5:]
	}
	if fc&0x18 != 0 { // callback (synchronous or asynchronous)
		select {
		case e.Callbacks <- Callback{id, data}:
		default:
			logf("zigbee: callback queue full, dropped frame 0x%02X", id)
		}
		return
	}
	e.mu.Lock()
	ok := e.waiting && seq == e.waitSeq
	if ok {
		e.waiting = false
	}
	w := e.wait
	e.mu.Unlock()
	if !ok {
		debugf("zigbee: unexpected reply 0x%02X seq %d", id, seq)
		return
	}
	w <- ezspReply{id, data}
}

// Command sends a command and returns its reply's parameters.
func (e *EZSP) Command(id byte, params []byte) ([]byte, error) {
	e.cmdMu.Lock()
	defer e.cmdMu.Unlock()
	w := make(chan ezspReply, 1)
	e.mu.Lock()
	seq := e.seq
	e.seq++
	e.wait, e.waitSeq, e.waiting = w, seq, true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.waiting = false
		e.mu.Unlock()
	}()
	head := []byte{seq, 0x00, 0xFF, 0x00, id} // EZSP 5..7 frame (bellows v5 _ezsp_frame_tx)
	if id == ezspVersion {
		head = []byte{seq, 0x00, id} // the version command always has the legacy format
	}
	if err := e.ash.Send(append(head, params...)); err != nil {
		return nil, err
	}
	select {
	case r := <-w:
		if r.id == ezspInvalidCommand {
			return nil, fmt.Errorf("ezsp: command 0x%02X rejected (status 0x%02X)", id, first(r.data))
		}
		if r.id != id {
			return nil, fmt.Errorf("ezsp: reply 0x%02X to command 0x%02X", r.id, id)
		}
		return r.data, nil
	case <-time.After(ezspCmdTimeout):
		return nil, fmt.Errorf("ezsp: no reply to command 0x%02X", id)
	case <-e.ash.done:
		return nil, errASHClosed
	}
}

func first(b []byte) byte {
	if len(b) == 0 {
		return 0xFF
	}
	return b[0]
}

// status runs a command whose reply starts with a status byte, and fails unless it is 0.
func (e *EZSP) status(name string, id byte, params []byte) ([]byte, error) {
	r, err := e.Command(id, params)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(r) == 0 {
		return nil, fmt.Errorf("%s: empty reply", name)
	}
	if r[0] != emberSuccess {
		return r, &StatusError{name, r[0]}
	}
	return r[1:], nil
}

// StatusError is a command that the chip answered with a status other than success.
type StatusError struct {
	Cmd    string
	Status byte
}

func (s *StatusError) Error() string { return fmt.Sprintf("%s: status 0x%02X", s.Cmd, s.Status) }

func isStatus(err error, status byte) bool {
	var s *StatusError
	return errors.As(err, &s) && s.Status == status
}

func (e *EZSP) Version() (protocol, stackType byte, stack uint16, err error) {
	r, err := e.Command(ezspVersion, []byte{ezspProtocolVersion})
	if err != nil {
		return 0, 0, 0, fmt.Errorf("version: %w", err)
	}
	if len(r) < 4 {
		return 0, 0, 0, errors.New("version: short reply")
	}
	return r[0], r[1], binary.LittleEndian.Uint16(r[2:]), nil
}

func (e *EZSP) SetConfig(id byte, value uint16) error {
	_, err := e.status(fmt.Sprintf("setConfigurationValue 0x%02X", id), ezspSetConfigValue, binary.LittleEndian.AppendUint16([]byte{id}, value))
	return err
}

func (e *EZSP) AddEndpoint(ep byte, profile, device uint16, in, out []uint16) error {
	b := binary.LittleEndian.AppendUint16([]byte{ep}, profile)
	b = binary.LittleEndian.AppendUint16(b, device)
	b = append(b, 0, byte(len(in)), byte(len(out)))
	for _, c := range append(append([]uint16{}, in...), out...) {
		b = binary.LittleEndian.AppendUint16(b, c)
	}
	_, err := e.status("addEndpoint", ezspAddEndpoint, b)
	return err
}

func (e *EZSP) NetworkInit() error {
	_, err := e.status("networkInit", ezspNetworkInit, []byte{0, 0})
	return err
}

// NetworkState is EmberNetworkStatus: 0 no network, 1 joining, 2 joined, 3 joined without parent, 4 leaving.
func (e *EZSP) NetworkState() (byte, error) {
	r, err := e.Command(ezspNetworkState, nil)
	if err != nil {
		return 0, fmt.Errorf("networkState: %w", err)
	}
	return first(r), nil
}

func (e *EZSP) Eui64() (uint64, error) {
	r, err := e.Command(ezspGetEui64, nil)
	if err != nil {
		return 0, fmt.Errorf("getEui64: %w", err)
	}
	if len(r) < 8 {
		return 0, errors.New("getEui64: short reply")
	}
	return binary.LittleEndian.Uint64(r), nil
}

func (e *EZSP) NodeID() (uint16, error) {
	r, err := e.Command(ezspGetNodeID, nil)
	if err != nil {
		return 0, fmt.Errorf("getNodeId: %w", err)
	}
	if len(r) < 2 {
		return 0, errors.New("getNodeId: short reply")
	}
	return binary.LittleEndian.Uint16(r), nil
}

// NetParams is the part of getNetworkParameters the router checks.
type NetParams struct {
	NodeType byte
	ExtPanID uint64
	PanID    uint16
	Channel  byte
}

func (e *EZSP) NetworkParams() (NetParams, error) {
	r, err := e.status("getNetworkParameters", ezspGetNetworkParams, nil)
	if err != nil {
		return NetParams{}, err
	}
	// nodeType, then EmberNetworkParameters: extendedPanId(8) panId(2) radioTxPower(1) radioChannel(1) ...
	if len(r) < 13 {
		return NetParams{}, errors.New("getNetworkParameters: short reply")
	}
	return NetParams{NodeType: r[0], ExtPanID: binary.LittleEndian.Uint64(r[1:]), PanID: binary.LittleEndian.Uint16(r[9:]), Channel: r[12]}, nil
}

// Binding is the part of EmberBindingTableEntry the router uses.
type Binding struct {
	Type     byte // 1 unicast, 2 many to one, 3 multicast (group)
	Local    byte
	Cluster  uint16
	Remote   byte
	Identity uint64 // IEEE address, or the group id in the low 2 bytes
}

// Binding returns entry i of the chip's binding table (Type 0: unused). Past the end of the table it fails with
// a StatusError.
func (e *EZSP) Binding(i byte) (Binding, error) {
	r, err := e.status("getBinding", ezspGetBinding, []byte{i})
	if err != nil {
		return Binding{}, err
	}
	if len(r) < 14 {
		return Binding{}, errors.New("getBinding: short reply")
	}
	return Binding{Type: r[0], Local: r[1], Cluster: binary.LittleEndian.Uint16(r[2:]), Remote: r[4], Identity: binary.LittleEndian.Uint64(r[5:])}, nil
}

// TrustCenter returns the trust center's IEEE address (in ZHA: the coordinator).
func (e *EZSP) TrustCenter() (uint64, error) {
	r, err := e.status("getCurrentSecurityState", ezspSecurityState, nil)
	if err != nil {
		return 0, err
	}
	if len(r) < 10 { // bitmask(2) trustCenterLongAddress(8)
		return 0, errors.New("getCurrentSecurityState: short reply")
	}
	return binary.LittleEndian.Uint64(r[2:]), nil
}

// NodeIDOf returns the node id of an IEEE address the chip knows (0xFFFF: unknown).
func (e *EZSP) NodeIDOf(ieee uint64) (uint16, error) {
	r, err := e.Command(ezspLookupNodeID, binary.LittleEndian.AppendUint64(nil, ieee))
	if err != nil {
		return 0, fmt.Errorf("lookupNodeIdByEui64: %w", err)
	}
	if len(r) < 2 {
		return 0, errors.New("lookupNodeIdByEui64: short reply")
	}
	return binary.LittleEndian.Uint16(r), nil
}

// SendUnicast sends to a node id directly (kind emberOutgoingDirect) or through binding table entry `dest`.
func (e *EZSP) SendUnicast(kind byte, dest uint16, aps APSFrame, tag byte, msg []byte) error {
	b := binary.LittleEndian.AppendUint16([]byte{kind}, dest)
	b = aps.put(b)
	b = append(append(b, tag, byte(len(msg))), msg...)
	_, err := e.status("sendUnicast", ezspSendUnicast, b)
	return err
}

// SendMulticast sends to the group in aps.Group.
func (e *EZSP) SendMulticast(aps APSFrame, tag byte, msg []byte) error {
	b := aps.put(nil)
	b = append(append(b, 0, 7, tag, byte(len(msg))), msg...) // hops 0 (= maximum), nonmember radius 7
	_, err := e.status("sendMulticast", ezspSendMulticast, b)
	return err
}

func (e *EZSP) SendBroadcast(dest uint16, aps APSFrame, tag byte, msg []byte) error {
	b := binary.LittleEndian.AppendUint16(nil, dest)
	b = aps.put(b)
	b = append(append(b, 0, tag, byte(len(msg))), msg...) // radius 0 = maximum
	_, err := e.status("sendBroadcast", ezspSendBroadcast, b)
	return err
}

// Incoming is an incomingMessageHandler callback.
type Incoming struct {
	Type   byte
	APS    APSFrame
	LQI    byte
	Sender uint16
	Msg    []byte
}

func parseIncoming(d []byte) (Incoming, bool) {
	// type(1) apsFrame(11) lastHopLqi(1) lastHopRssi(1) sender(2) bindingIndex(1) addressIndex(1) length(1) message
	if len(d) < 19 {
		return Incoming{}, false
	}
	aps, _ := parseAPS(d[1:])
	n := int(d[18])
	if len(d) < 19+n {
		return Incoming{}, false
	}
	return Incoming{Type: d[0], APS: aps, LQI: d[12], Sender: binary.LittleEndian.Uint16(d[14:]), Msg: d[19 : 19+n]}, true
}
