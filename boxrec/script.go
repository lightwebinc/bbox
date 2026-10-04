package boxrec

import (
	"bytes"
	"math/big"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/bcommon/guard"

	"github.com/lightwebinc/bcommon/pushdrop"
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

// ClaimOf is the admission classifier over raw locking-script bytes
// (docs/spec.md section 8.1): a script claims a record when it starts with a
// 33-byte push (0x21 and 33 bytes), then OP_CHECKSIG (0xac), then one push
// whose data the record's claim prefix begins. Nothing else about the script
// is read here; the lock rule compares the whole script later.
func ClaimOf(s []byte) Kind {
	f, ok := pushdrop.FirstPush(s)
	if !ok {
		return KindNone
	}
	return ClaimOfField(f)
}

func pushFields(s []byte) ([][]byte, bool) { return pushdrop.Fields(s) }

// PushDropScript is the one canonical lock-before PushDrop for key, fields
// and an optional field signature (bcommon pushdrop.Script).
func PushDropScript(key *ec.PublicKey, fields [][]byte, sig []byte) []byte {
	return pushdrop.Script(key, fields, sig)
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
