package boxrec

import (
	"bytes"
	"encoding/binary"
	"math/big"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"

	"github.com/lightwebinc/bcommon/guard"
)

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

// op is one parsed script element: an opcode, and for a data push the bytes
// pushed.
type op struct {
	code byte
	data []byte
	push bool // a data push (OP_0, a direct push or OP_PUSHDATA1/2/4)
}

// parseScript splits s into opcodes. A push whose declared length runs past
// the end refuses the whole script. The reader is this package's own rather
// than the SDK's, so that the Go and TypeScript codecs read every byte
// string the same way.
func parseScript(s []byte) ([]op, bool) {
	var out []op
	for i := 0; i < len(s); {
		c := s[i]
		i++
		var n int
		switch {
		case c == script.Op0:
			out = append(out, op{code: c, data: []byte{}, push: true})
			continue
		case c >= 1 && c <= 75:
			n = int(c)
		case c == script.OpPUSHDATA1:
			if i+1 > len(s) {
				return nil, false
			}
			n = int(s[i])
			i++
		case c == script.OpPUSHDATA2:
			if i+2 > len(s) {
				return nil, false
			}
			n = int(binary.LittleEndian.Uint16(s[i:]))
			i += 2
		case c == script.OpPUSHDATA4:
			if i+4 > len(s) {
				return nil, false
			}
			m := uint64(binary.LittleEndian.Uint32(s[i:]))
			i += 4
			if m > uint64(len(s)-i) {
				return nil, false
			}
			n = int(m)
		default:
			out = append(out, op{code: c})
			continue
		}
		if n > len(s)-i {
			return nil, false
		}
		out = append(out, op{code: c, data: s[i : i+n], push: true})
		i += n
	}
	return out, true
}

// pushFields reads s as a lock-before PushDrop, leniently: a 33-byte push,
// OP_CHECKSIG, then every data push up to the first opcode that is not one
// (the field signature included). Whether s is the canonical script is
// decided by rebuilding it (PushDropScript) and comparing bytes.
func pushFields(s []byte) ([][]byte, bool) {
	ops, ok := parseScript(s)
	if !ok || len(ops) < 3 || !ops[0].push || len(ops[0].data) != 33 || ops[1].code != script.OpCHECKSIG {
		return nil, false
	}
	var fields [][]byte
	for _, o := range ops[2:] {
		if !o.push {
			break
		}
		fields = append(fields, o.data)
	}
	return fields, len(fields) > 0
}

// PushDropScript is the one canonical lock-before PushDrop for key, fields
// and an optional field signature, exactly as bcommon pushdrop writes it:
// the 33-byte compressed key, OP_CHECKSIG, each field and then the
// signature as one minimal push, and the fewest OP_2DROP then OP_DROP that
// clear them. Every script a host checks is compared with this byte for
// byte (docs/spec.md section 8.1).
func PushDropScript(key *ec.PublicKey, fields [][]byte, sig []byte) []byte {
	ops := []*script.ScriptChunk{{Op: 33, Data: key.Compressed()}, {Op: script.OpCHECKSIG}}
	all := append([][]byte{}, fields...)
	if sig != nil {
		all = append(all, sig)
	}
	for _, f := range all {
		ops = append(ops, sdkpushdrop.CreateMinimallyEncodedScriptChunk(f))
	}
	for n := len(all); n > 0; n -= 2 {
		if n == 1 {
			ops = append(ops, &script.ScriptChunk{Op: script.OpDROP})
		} else {
			ops = append(ops, &script.ScriptChunk{Op: script.Op2DROP})
		}
	}
	s, err := script.NewScriptFromScriptOps(ops)
	if err != nil {
		// Every chunk above is well formed; this cannot happen.
		panic(err)
	}
	return *s
}

// FundingScript is a funding-tree output locked to key:
// <key> OP_CHECKSIG <62 62 02> OP_DROP.
func FundingScript(key *ec.PublicKey) []byte {
	return PushDropScript(key, [][]byte{TagFunding}, nil)
}

// fundingLen is the length of every funding script.
const fundingLen = 1 + 33 + 1 + 1 + 3 + 1

// IsFundingShape reports whether s is a well-formed funding output under
// some canonical compressed key: exactly the funding script rebuilt from its
// own key. Whose key it is cannot be read from the script (the derivation is
// one way); a carrier's funding rule compares with the owner's, and a sweep
// takes any.
func IsFundingShape(s []byte) bool {
	if len(s) != fundingLen || s[0] != 33 {
		return false
	}
	k, err := guard.ParsePubKey(s[1:34])
	if err != nil {
		return false
	}
	return bytes.Equal(s, FundingScript(k))
}

// StrictSignature reports whether der is a strict DER ECDSA signature
// (BIP 66) with R in [1, n-1] and S in [1, n/2]: the one encoding of a
// signature a host accepts, so nobody can re-encode it into another byte
// string that still verifies.
func StrictSignature(der []byte) bool {
	if len(der) < 8 || len(der) > 72 || der[0] != 0x30 || int(der[1]) != len(der)-2 {
		return false
	}
	lenR := int(der[3])
	if der[2] != 0x02 || lenR == 0 || 6+lenR > len(der) {
		return false
	}
	lenS := int(der[5+lenR])
	if der[4+lenR] != 0x02 || lenS == 0 || 6+lenR+lenS != len(der) {
		return false
	}
	r, s := der[4:4+lenR], der[6+lenR:]
	for _, v := range [][]byte{r, s} {
		if v[0]&0x80 != 0 || (len(v) > 1 && v[0] == 0 && v[1]&0x80 == 0) {
			return false
		}
	}
	n := ec.S256().N
	R, S := new(big.Int).SetBytes(r), new(big.Int).SetBytes(s)
	return R.Sign() > 0 && R.Cmp(n) < 0 && S.Sign() > 0 && S.Cmp(new(big.Int).Rsh(n, 1)) <= 0
}
