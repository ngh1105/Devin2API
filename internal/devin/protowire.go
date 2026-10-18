// Package devin provides a hand-rolled protobuf client for Devin's upstream
// APIs (CLI OAuth, GetUserStatus). No external protobuf libraries — just
// encoding/binary and crypto/sha256.
package devin

import (
	"encoding/binary"
	"errors"
)

// ---------- wire helpers ----------------------------------------------------

var (
	WireVarint    = 0
	WireFixed32   = 5
	WireTypeBytes = 2
)

// Internal unexported aliases for backward compatibility with existing callers.
var (
	wireVarint    = WireVarint
	wireFixed32   = WireFixed32
	wireBytes     = WireTypeBytes
	pwTag         = PwTag
	pwVarint      = PwVarint
	pwString      = PwString
	pwFieldString = PwFieldString
	pwFieldVarint = PwFieldVarint
)

// PwTag emits the varint-encoded tag for (field, wire).
func PwTag(field, wire int) []byte {
	val := uint64(field)<<3 | uint64(wire)
	return PwVarint(val)
}

// PwVarint encodes a single uint64 as a varint (7 bits + MSB continuation).
func PwVarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	out = append(out, byte(v))
	return out
}

// PwString builds a length-delimited field: tag + varint-length + string bytes.
func PwString(field int, s string) []byte {
	b := []byte(s)
	out := PwTag(field, WireTypeBytes)
	out = append(out, PwVarint(uint64(len(b)))...)
	out = append(out, b...)
	return out
}

// PwFieldString builds a length-delimited field: tag + varint-length + bytes.
func PwFieldString(field int, s string) []byte {
	return PwString(field, s)
}

// PwFieldVarint builds a varint field: tag + varint value.
func PwFieldVarint(field int, v uint64) []byte {
	return append(PwTag(field, WireVarint), PwVarint(v)...)
}

// PwFieldUint32 builds a fixed32 little-endian field.
func PwFieldUint32(field int, v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return append(pwTag(field, wireFixed32), b...)
}

// ---------- reader ------------------------------------------------------------

// pwReader decodes varint tags from a protobuf byte stream.
type pwReader struct {
	data      []byte
	offset    int
	field     int
	wire      int
	val       []byte
	varint    uint64
	ok        bool
	truncated bool // true when not enough bytes remain
}

// Next reads one field from the stream. Returns true if a field was available.
func (r *pwReader) Next() (field, wire int, val []byte, varint uint64, ok bool) {
	if r.offset >= len(r.data) {
		r.ok = false
		r.truncated = false
		return 0, 0, nil, 0, false
	}

	// Read varint tag.
	tag, n := readVarint(r.data[r.offset:])
	if n == 0 {
		r.truncated = true
		r.ok = false
		return 0, 0, nil, 0, false
	}
	r.offset += n

	r.field = int(tag >> 3)
	r.wire = int(tag & 0x07)

	switch r.wire {
	case wireVarint:
		v, m := readVarint(r.data[r.offset:])
		if m == 0 {
			r.truncated = true
			r.ok = false
			return r.field, r.wire, nil, 0, false
		}
		r.offset += m
		r.varint = v
		r.val = nil
		r.ok = true
		return r.field, r.wire, nil, v, true

	case wireFixed32:
		if r.offset+4 > len(r.data) {
			r.truncated = true
			r.ok = false
			return 0, 0, nil, 0, false
		}
		r.val = r.data[r.offset : r.offset+4]
		r.offset += 4
		r.ok = true
		return r.field, r.wire, r.val, 0, true

	case wireBytes:
		lenVal, m := readVarint(r.data[r.offset:])
		if m == 0 {
			r.truncated = true
			r.ok = false
			return 0, 0, nil, 0, false
		}
		r.offset += m
		if int(lenVal) > len(r.data)-r.offset {
			r.truncated = true
			r.ok = false
			return 0, 0, nil, 0, false
		}
		r.val = r.data[r.offset : r.offset+int(lenVal)]
		r.offset += int(lenVal)
		r.ok = true
		return r.field, r.wire, r.val, 0, true

	default:
		return r.Next() // skip unknown wire types
	}
}

// Done returns nil when no more fields are left; returns errors.Truncated
// when the stream is too short.
func (r *pwReader) Done() error {
	if !r.ok {
		if r.truncated {
			return errors.New("protowire: truncated")
		}
		return nil
	}
	return nil
}

// readVarint decodes a varint from the beginning of data.
// Returns the decoded value and number of bytes consumed (0 on truncation).
func readVarint(data []byte) (uint64, int) {
	var v uint64
	for i := 0; i < 10 && i < len(data); i++ {
		b := data[i]
		if b < 0x80 {
			return v | uint64(b)<<uint(i*7), i + 1
		}
		v |= uint64(b&0x7F) << uint(i*7)
	}
	return 0, 0 // truncated
}
