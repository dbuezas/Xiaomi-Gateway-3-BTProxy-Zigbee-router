package main

import (
	"bufio"
	"errors"
	"io"
)

// Minimal protobuf encoding for the ESPHome native API (plaintext framing).

type pb struct{ b []byte }

func (w *pb) varint(v uint64) {
	for v >= 0x80 {
		w.b = append(w.b, byte(v)|0x80)
		v >>= 7
	}
	w.b = append(w.b, byte(v))
}

func (w *pb) key(field, wire int) { w.varint(uint64(field<<3 | wire)) }

func (w *pb) Uint(field int, v uint64) {
	if v != 0 {
		w.key(field, 0)
		w.varint(v)
	}
}

func (w *pb) Int32(field int, v int32) {
	if v != 0 {
		w.key(field, 0)
		w.varint(uint64(int64(v)))
	}
}

// Sint32 always writes the field (rssi is "force" in api.proto).
func (w *pb) Sint32(field int, v int32) {
	w.key(field, 0)
	w.varint(uint64(uint32(v<<1) ^ uint32(v>>31)))
}

func (w *pb) Bool(field int, v bool) {
	if v {
		w.key(field, 0)
		w.varint(1)
	}
}

func (w *pb) Bytes(field int, v []byte) {
	w.key(field, 2)
	w.varint(uint64(len(v)))
	w.b = append(w.b, v...)
}

func (w *pb) String(field int, v string) { w.Bytes(field, []byte(v)) }

type pbFields struct {
	ints  map[int]uint64
	bytes map[int][]byte
}

func (f pbFields) Uint(field int) uint64 { return f.ints[field] }
func (f pbFields) Bool(field int) bool   { return f.ints[field] != 0 }
func (f pbFields) Bytes(field int) []byte {
	return f.bytes[field]
}

var errProto = errors.New("protobuf: malformed message")

func pbParse(b []byte) (pbFields, error) {
	f := pbFields{ints: map[int]uint64{}, bytes: map[int][]byte{}}
	for len(b) > 0 {
		k, n := uvarint(b)
		if n <= 0 {
			return f, errProto
		}
		b = b[n:]
		field, wire := int(k>>3), int(k&7)
		switch wire {
		case 0:
			v, n := uvarint(b)
			if n <= 0 {
				return f, errProto
			}
			f.ints[field] = v
			b = b[n:]
		case 2:
			l, n := uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return f, errProto
			}
			f.bytes[field] = b[n : n+int(l)]
			b = b[n+int(l):]
		case 1:
			if len(b) < 8 {
				return f, errProto
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return f, errProto
			}
			b = b[4:]
		default:
			return f, errProto
		}
	}
	return f, nil
}

func uvarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

func readUvarint(r *bufio.Reader) (uint64, error) {
	var v uint64
	for i := 0; i < 10; i++ {
		c, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			return v, nil
		}
	}
	return 0, errProto
}

var errNoise = errors.New("api: client wants an encrypted (noise) connection; only plaintext is supported")

// readFrame reads one plaintext frame: 0x00, varint size, varint type, payload.
func readFrame(r *bufio.Reader) (int, []byte, error) {
	first, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	if first != 0 {
		return 0, nil, errNoise
	}
	size, err := readUvarint(r)
	if err != nil {
		return 0, nil, err
	}
	typ, err := readUvarint(r)
	if err != nil {
		return 0, nil, err
	}
	if size > 1<<16 {
		return 0, nil, errProto
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return int(typ), payload, nil
}

func frame(typ int, payload []byte) []byte {
	w := pb{b: make([]byte, 0, len(payload)+8)}
	w.b = append(w.b, 0)
	w.varint(uint64(len(payload)))
	w.varint(uint64(typ))
	w.b = append(w.b, payload...)
	return w.b
}
