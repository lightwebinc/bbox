package boxrec

import "encoding/binary"

// Kind is what a locking script claims to carry.
type Kind int

// The kinds.
const (
	KindNone Kind = iota
	KindEnvelope
	KindReceipt
)

func (k Kind) String() string {
	switch k {
	case KindEnvelope:
		return "envelope"
	case KindReceipt:
		return "receipt"
	}
	return "none"
}

// ClaimOfField is what a PushDrop's first field claims to be: an envelope
// or a receipt record when it starts with a CBOR definite-length map head,
// key 0 and that record's magic.
func ClaimOfField(p []byte) Kind {
	switch {
	case IsEnvelope(p):
		return KindEnvelope
	case IsReceipt(p):
		return KindReceipt
	}
	return KindNone
}

// firstPush reads the push at s[i]: an opcode 0x01 to 0x4b pushing that
// many bytes, or OP_PUSHDATA1, 2 or 4 with a little-endian length. It
// returns the pushed bytes, or false when s[i] is not a push or the bytes
// are not all present.
func firstPush(s []byte, i int) ([]byte, bool) {
	if i >= len(s) {
		return nil, false
	}
	op := s[i]
	i++
	var n int
	switch {
	case op >= 0x01 && op <= 0x4b:
		n = int(op)
	case op == 0x4c && i+1 <= len(s):
		n, i = int(s[i]), i+1
	case op == 0x4d && i+2 <= len(s):
		n, i = int(binary.LittleEndian.Uint16(s[i:])), i+2
	case op == 0x4e && i+4 <= len(s):
		m := binary.LittleEndian.Uint32(s[i:])
		if uint64(m) > uint64(len(s)) {
			return nil, false
		}
		n, i = int(m), i+4
	default:
		return nil, false
	}
	if n > len(s)-i {
		return nil, false
	}
	return s[i : i+n], true
}

// ClaimOf is the admission classifier over raw locking-script bytes
// (docs/spec.md section 8.1): a script claims a record when it starts with a
// 33-byte push (0x21 and 33 bytes), then OP_CHECKSIG (0xac), then one push
// whose data the record's claim prefix begins. Nothing else about the script
// is read here; the lock rule compares the whole script later.
func ClaimOf(s []byte) Kind {
	if len(s) < 35 || s[0] != 0x21 || s[34] != 0xac {
		return KindNone
	}
	f, ok := firstPush(s, 35)
	if !ok {
		return KindNone
	}
	return ClaimOfField(f)
}
