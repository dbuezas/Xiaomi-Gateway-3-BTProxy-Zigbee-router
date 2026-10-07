package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ESPHome native API server (plaintext), only the parts a Bluetooth proxy needs.

// ESPHome's own keepalive: ping a client quiet for 60 s, drop it after 2.5 times that.
const (
	keepaliveIdle = 60 * time.Second
	keepaliveDrop = 90 * time.Second
)

const (
	maxClients   = 4
	clientQueue  = 128 // frames waiting for a slow client; adverts are dropped first, then the client
	frameTimeout = 30 * time.Second
	writeTimeout = 10 * time.Second
)

const (
	msgHelloRequest                     = 1
	msgHelloResponse                    = 2
	msgDisconnectRequest                = 5
	msgDisconnectResponse               = 6
	msgPingRequest                      = 7
	msgPingResponse                     = 8
	msgDeviceInfoRequest                = 9
	msgDeviceInfoResponse               = 10
	msgListEntitiesRequest              = 11
	msgListEntitiesDoneResponse         = 19
	msgSubscribeBLEAdvertisements       = 66
	msgBluetoothDeviceRequest           = 68
	msgBluetoothDeviceConnection        = 69
	msgGATTGetServicesRequest           = 70
	msgGATTGetServicesResponse          = 71
	msgGATTGetServicesDone              = 72
	msgGATTReadRequest                  = 73
	msgGATTReadResponse                 = 74
	msgGATTWriteRequest                 = 75
	msgGATTReadDescriptorRequest        = 76
	msgGATTWriteDescriptorRequest       = 77
	msgGATTNotifyRequest                = 78
	msgGATTNotifyData                   = 79
	msgSubscribeConnectionsFree         = 80
	msgConnectionsFreeResponse          = 81
	msgGATTErrorResponse                = 82
	msgGATTWriteResponse                = 83
	msgGATTNotifyResponse               = 84
	msgBluetoothDevicePairingResponse   = 85
	msgBluetoothDeviceUnpairingResponse = 86
	msgUnsubscribeBLEAdvertisements     = 87
	msgRawAdvertisementsResponse        = 93
	msgScannerStateResponse             = 126
	msgScannerSetModeRequest            = 127

	featurePassiveScan       = 1 << 0
	featureActiveConnections = 1 << 1
	featureRemoteCaching     = 1 << 2
	featurePairing           = 1 << 3
	featureRawAdvertisements = 1 << 5
	featureStateAndMode      = 1 << 6

	// API 1.12: short_uuid in GATT services, feature flags in DeviceInfoResponse.
	apiMajor = 1
	apiMinor = 12
)

type DeviceInfo struct {
	Name, FriendlyName, MAC, Model, Version string
}

type RawAdv struct {
	Addr     uint64
	AddrType byte
	RSSI     int8
	Data     []byte
}

type Server struct {
	proxy *Proxy
	info  DeviceInfo

	mu      sync.Mutex
	clients map[*client]struct{}

	opMu sync.Mutex
	ops  map[uint64]chan func() // per device: GATT requests run one after the other, in arrival order
}

// client: frames are queued and written by the client's own goroutine, so a slow or vanished client
// never blocks the chip's event loop.
type client struct {
	conn   net.Conn
	out    chan []byte // nil: flush, then close
	done   chan struct{}
	wdone  chan struct{} // the writer has stopped
	once   sync.Once
	advSub atomic.Bool
}

func NewServer(p *Proxy, info DeviceInfo) *Server {
	s := &Server{proxy: p, info: info, clients: map[*client]struct{}{}, ops: map[uint64]chan func(){}}
	p.api = s
	return s
}

func (s *Server) Serve(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	logf("API listening on %s", addr)
	for {
		nc, err := ln.Accept()
		if err != nil {
			// e.g. out of file descriptors: wait and go on, never end the proxy for it
			logf("API accept: %v", err)
			time.Sleep(time.Second)
			continue
		}
		s.mu.Lock()
		full := len(s.clients) >= maxClients
		s.mu.Unlock()
		if full {
			logf("API client %s refused: %d clients already", nc.RemoteAddr(), maxClients)
			nc.Close()
			continue
		}
		go s.handle(nc)
	}
}

func (c *client) close() {
	c.once.Do(func() {
		close(c.done)
		c.conn.Close()
	})
}

func (c *client) send(typ int, payload []byte) { c.queue(frame(typ, payload), false) }

// queue never blocks. A full queue drops an advert batch; for anything else the client is too slow and is dropped.
func (c *client) queue(f []byte, droppable bool) {
	if droppable && len(c.out) >= clientQueue/2 {
		return // adverts may use only half the queue: GATT replies and control frames always find room
	}
	select {
	case <-c.done:
	case c.out <- f:
	default:
		if !droppable {
			logf("API client %s too slow, dropping it", c.conn.RemoteAddr())
			c.close()
		}
	}
}

func (c *client) writer() {
	defer close(c.wdone)
	for {
		select {
		case f := <-c.out:
			if f == nil {
				c.close()
				return
			}
			c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := c.conn.Write(f); err != nil {
				c.close()
				return
			}
		case <-c.done:
			return
		}
	}
}

func (s *Server) broadcast(typ int, payload []byte, advOnly bool) {
	s.mu.Lock()
	cs := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		if !advOnly || c.advSub.Load() {
			cs = append(cs, c)
		}
	}
	s.mu.Unlock()
	if len(cs) == 0 {
		return
	}
	f := frame(typ, payload)
	for _, c := range cs {
		c.queue(f, advOnly)
	}
}

// gattOp runs fn after the device's earlier GATT requests (write chunks must reach the device in order).
func (s *Server) gattOp(addr uint64, fn func()) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	q, ok := s.ops[addr]
	if !ok {
		q = make(chan func(), 256)
		s.ops[addr] = q
		go s.runOps(addr, q)
	}
	select {
	case q <- fn:
	default:
		logf("%s: GATT queue full", macString(addr))
		go fn()
	}
}

func (s *Server) runOps(addr uint64, q chan func()) {
	idle := time.NewTimer(time.Minute)
	defer idle.Stop()
	for {
		select {
		case fn := <-q:
			fn()
			idle.Reset(time.Minute)
		case <-idle.C:
			s.opMu.Lock()
			if len(q) == 0 {
				delete(s.ops, addr)
				s.opMu.Unlock()
				return
			}
			s.opMu.Unlock()
			idle.Reset(time.Minute)
		}
	}
}

func (s *Server) handle(nc net.Conn) {
	c := &client{conn: nc, out: make(chan []byte, clientQueue), done: make(chan struct{}), wdone: make(chan struct{})}
	logf("API client %s connected", nc.RemoteAddr())
	s.mu.Lock()
	s.clients[c] = struct{}{}
	s.mu.Unlock()
	go c.writer()
	defer func() {
		s.mu.Lock()
		delete(s.clients, c)
		s.mu.Unlock()
		// send what is queued (e.g. the DisconnectResponse) before closing
		flush := time.After(2 * time.Second)
		select {
		case c.out <- nil:
			select {
			case <-c.wdone:
			case <-flush:
			}
		case <-c.done:
		case <-flush:
		}
		c.close()
		logf("API client %s gone", nc.RemoteAddr())
	}()
	r := bufio.NewReader(nc)
	// Keepalive the way ESPHome devices do it: after keepaliveIdle with nothing received, ping the
	// client; drop it only if keepaliveDrop more passes with nothing. Waiting for the CLIENT to ping is
	// not enough: aioesphomeapi skips its own ping whenever it has received anything, so a client
	// subscribed to adverts never pings, and a plain read deadline dropped Home Assistant every time
	// it ran out.
	pinged := false
	for {
		if pinged {
			nc.SetReadDeadline(time.Now().Add(keepaliveDrop))
		} else {
			nc.SetReadDeadline(time.Now().Add(keepaliveIdle))
		}
		// wait for the first byte with the keepalive deadline, then give the rest of the frame its own:
		// a deadline that fires mid-frame would lose the bytes already read
		_, err := r.Peek(1)
		var ne net.Error
		if err != nil && !pinged && errors.As(err, &ne) && ne.Timeout() {
			debugf("API client %s quiet for %v, pinging", nc.RemoteAddr(), keepaliveIdle)
			c.send(msgPingRequest, nil)
			pinged = true
			continue
		}
		pinged = false
		var typ int
		var payload []byte
		if err == nil {
			nc.SetReadDeadline(time.Now().Add(frameTimeout))
			typ, payload, err = readFrame(r)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				logf("API client %s: %v", nc.RemoteAddr(), err)
			}
			return
		}
		f, err := pbParse(payload)
		if err != nil {
			logf("API client %s: bad message %d", nc.RemoteAddr(), typ)
			return
		}
		debugf("api <- %d %x", typ, payload)
		if !s.dispatch(c, typ, f) {
			return
		}
	}
}

func (s *Server) dispatch(c *client, typ int, f pbFields) bool {
	switch typ {
	case msgHelloRequest:
		var w pb
		w.Uint(1, apiMajor)
		w.Uint(2, apiMinor)
		w.String(3, "gw3-btproxy "+s.info.Version)
		w.String(4, s.info.Name)
		c.send(msgHelloResponse, w.b)
	case msgDisconnectRequest:
		c.send(msgDisconnectResponse, nil)
		return false
	case msgPingRequest:
		c.send(msgPingResponse, nil)
	case msgDeviceInfoRequest:
		var w pb
		w.String(2, s.info.Name)
		w.String(3, s.info.MAC)
		w.String(4, "2026.9.0")
		w.String(5, buildTime)
		w.String(6, s.info.Model)
		w.String(12, "Xiaomi")
		w.String(13, s.info.FriendlyName)
		features := uint64(featurePassiveScan | featureActiveConnections | featureRemoteCaching | featureRawAdvertisements | featureStateAndMode)
		if s.proxy.canPair.Load() {
			features |= featurePairing
		}
		w.Uint(15, features)
		w.String(18, macString(s.proxy.btAddr.Load()))
		c.send(msgDeviceInfoResponse, w.b)
	case msgListEntitiesRequest:
		c.send(msgListEntitiesDoneResponse, nil)
	case msgSubscribeBLEAdvertisements:
		c.advSub.Store(true)
		s.sendScannerState(c)
	case msgUnsubscribeBLEAdvertisements:
		c.advSub.Store(false)
	case msgScannerSetModeRequest:
		// not inline: a connect in progress holds the scan lock for up to a minute, and this client's pings must go on
		active := f.Uint(1) == 1
		go func() {
			s.proxy.SetActive(active)
			s.broadcastScannerState()
		}()
	case msgSubscribeConnectionsFree:
		s.ConnectionsFree()
	case msgBluetoothDeviceRequest:
		addr := f.Uint(1)
		switch f.Uint(2) {
		case 4, 5: // CONNECT_V3_WITH_CACHE, CONNECT_V3_WITHOUT_CACHE
			s.proxy.Connect(addr, byte(f.Uint(4)), f.Uint(2) == 4)
		case 1: // DISCONNECT
			go s.proxy.Disconnect(addr)
		case 2: // PAIR
			s.gattOp(addr, func() {
				ok, errCode := s.proxy.Pair(addr)
				var w pb
				w.Uint(1, addr)
				w.Bool(2, ok)
				w.Int32(3, errCode)
				s.broadcast(msgBluetoothDevicePairingResponse, w.b, false)
			})
		case 3: // UNPAIR
			s.gattOp(addr, func() {
				var w pb
				w.Uint(1, addr)
				w.Bool(2, s.proxy.Unpair(addr))
				s.broadcast(msgBluetoothDeviceUnpairingResponse, w.b, false)
			})
		default: // V1 connect and cache clearing are not supported
			s.DeviceConnection(addr, false, 0, 0)
		}
	case msgGATTGetServicesRequest:
		addr := f.Uint(1)
		s.gattOp(addr, func() { s.sendServices(addr) })
	case msgGATTReadRequest, msgGATTReadDescriptorRequest:
		addr, handle := f.Uint(1), uint16(f.Uint(2))
		s.gattOp(addr, func() {
			data, res := s.proxy.Read(addr, handle, typ == msgGATTReadDescriptorRequest)
			if res != 0 {
				s.gattError(addr, handle, res)
				return
			}
			var w pb
			w.Uint(1, addr)
			w.Uint(2, uint64(handle))
			w.Bytes(3, data)
			s.broadcast(msgGATTReadResponse, w.b, false)
		})
	case msgGATTWriteRequest, msgGATTWriteDescriptorRequest:
		addr, handle := f.Uint(1), uint16(f.Uint(2))
		descriptor := typ == msgGATTWriteDescriptorRequest
		response := descriptor || f.Bool(3)
		data := clone(f.Bytes(4))
		if descriptor {
			data = clone(f.Bytes(3))
		}
		s.gattOp(addr, func() {
			if res := s.proxy.Write(addr, handle, data, response, descriptor); res != 0 {
				s.gattError(addr, handle, res)
				return
			}
			if response {
				var w pb
				w.Uint(1, addr)
				w.Uint(2, uint64(handle))
				s.broadcast(msgGATTWriteResponse, w.b, false)
			}
		})
	case msgGATTNotifyRequest:
		// Like ESPHome: only acknowledge, the client writes the CCCD itself. The chip reports every notification.
		addr, handle := f.Uint(1), uint16(f.Uint(2))
		if s.proxy.conn(addr) == nil {
			s.gattError(addr, handle, errNotConnected)
			break
		}
		var w pb
		w.Uint(1, addr)
		w.Uint(2, uint64(handle))
		c.send(msgGATTNotifyResponse, w.b)
	default:
		debugf("api: ignoring message %d", typ)
	}
	return true
}

func (s *Server) sendScannerState(c *client) {
	mode := uint64(0)
	if s.proxy.active.Load() {
		mode = 1
	}
	var w pb
	w.Uint(1, 2) // RUNNING
	w.Uint(2, mode)
	w.Uint(3, mode)
	c.send(msgScannerStateResponse, w.b)
}

func (s *Server) broadcastScannerState() {
	s.mu.Lock()
	cs := make([]*client, 0, len(s.clients))
	for c := range s.clients {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	for _, c := range cs {
		s.sendScannerState(c)
	}
}

func (s *Server) gattError(addr uint64, handle uint16, res int32) {
	var w pb
	w.Uint(1, addr)
	w.Uint(2, uint64(handle))
	w.Int32(3, res)
	s.broadcast(msgGATTErrorResponse, w.b, false)
}

func uuidField(w *pb, uuidField, shortField int, uuid []byte) {
	if len(uuid) != 2 && len(uuid) != 4 && len(uuid) != 16 {
		return // not a UUID: a corrupted chip frame
	}
	if len(uuid) == 2 || len(uuid) == 4 {
		var v uint64
		for i := len(uuid) - 1; i >= 0; i-- {
			v = v<<8 | uint64(uuid[i])
		}
		w.Uint(shortField, v)
		return
	}
	var hi, lo uint64
	for i := 15; i >= 8; i-- {
		hi = hi<<8 | uint64(uuid[i])
	}
	for i := 7; i >= 0; i-- {
		lo = lo<<8 | uint64(uuid[i])
	}
	w.key(uuidField, 0)
	w.varint(hi)
	w.key(uuidField, 0)
	w.varint(lo)
}

func (s *Server) sendServices(addr uint64) {
	svcs, res := s.proxy.Services(addr)
	if res != 0 {
		s.gattError(addr, 0, res)
		return
	}
	for _, svc := range svcs {
		var sw pb
		uuidField(&sw, 1, 4, svc.UUID)
		sw.Uint(2, uint64(svc.Handle))
		for _, ch := range svc.Chars {
			var cw pb
			uuidField(&cw, 1, 5, ch.UUID)
			cw.Uint(2, uint64(ch.Handle))
			cw.Uint(3, uint64(ch.Props))
			for _, d := range ch.Descs {
				var dw pb
				uuidField(&dw, 1, 3, d.UUID)
				dw.Uint(2, uint64(d.Handle))
				cw.Bytes(4, dw.b)
			}
			sw.Bytes(3, cw.b)
		}
		var w pb
		w.Uint(1, addr)
		w.Bytes(2, sw.b)
		s.broadcast(msgGATTGetServicesResponse, w.b, false)
	}
	var w pb
	w.Uint(1, addr)
	s.broadcast(msgGATTGetServicesDone, w.b, false)
}

// ---------- called by the proxy ----------

func (s *Server) Adverts(advs []RawAdv) {
	var w pb
	for _, a := range advs {
		var aw pb
		aw.key(1, 0)
		aw.varint(a.Addr)
		aw.Sint32(2, int32(a.RSSI))
		addrType := uint64(a.AddrType)
		if addrType > 1 {
			addrType = 1
		}
		aw.Uint(3, addrType)
		aw.Bytes(4, a.Data)
		w.Bytes(1, aw.b)
	}
	s.broadcast(msgRawAdvertisementsResponse, w.b, true)
}

func (s *Server) DeviceConnection(addr uint64, connected bool, mtu uint16, errCode int32) {
	var w pb
	w.Uint(1, addr)
	w.Bool(2, connected)
	w.Uint(3, uint64(mtu))
	w.Int32(4, errCode)
	s.broadcast(msgBluetoothDeviceConnection, w.b, false)
}

func (s *Server) ConnectionsFree() {
	free, limit, allocated := s.proxy.ConnectionsFree()
	var w pb
	w.Uint(1, uint64(free))
	w.Uint(2, uint64(limit))
	for _, a := range allocated {
		w.Uint(3, a)
	}
	s.broadcast(msgConnectionsFreeResponse, w.b, false)
}

func (s *Server) NotifyData(addr uint64, handle uint16, data []byte) {
	var w pb
	w.Uint(1, addr)
	w.Uint(2, uint64(handle))
	w.Bytes(3, data)
	s.broadcast(msgGATTNotifyData, w.b, false)
}
