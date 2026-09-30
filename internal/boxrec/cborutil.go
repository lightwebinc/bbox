package boxrec

import (
	"bytes"
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"
)

// decodeMap is refusal steps 1 to 3: the bound, one canonical CBOR item that
// is a map, and at most MaxKeys entries.
func decodeMap(b []byte, max int) (cbor.Map, error) {
	if len(b) > max {
		return nil, ErrTooLarge
	}
	v, err := cbor.DecodeValue(b)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCBOR, err)
	}
	m, ok := v.(cbor.Map)
	if !ok {
		return nil, ErrCBOR
	}
	if len(m) > MaxKeys {
		return nil, fmt.Errorf("%w: more than %d keys", ErrTooLarge, MaxKeys)
	}
	return m, nil
}

// intKeys is step 4: it splits a record map into its known integer keys and
// the unknown ones above last, which are preserved. Any key that is not an
// unsigned integer refuses the record.
func intKeys(m cbor.Map, last uint64) (map[uint64]cbor.Value, cbor.Map, error) {
	known := make(map[uint64]cbor.Value, len(m))
	var extra cbor.Map
	for _, p := range m {
		k, ok := p.Key.(uint64)
		if !ok {
			return nil, nil, ErrKeyType
		}
		if k > last {
			extra = append(extra, p)
			continue
		}
		known[k] = p.Val
	}
	return known, extra, nil
}

// checkMagic is step 5.
func checkMagic(f map[uint64]cbor.Value, magic []byte) error {
	b, ok := f[0].([]byte)
	if !ok || !bytes.Equal(b, magic) {
		return ErrMagic
	}
	return nil
}

// checkExtra holds preserved keys to the rule that they sit above the known
// keys, so re-encoding cannot shadow a known field.
func checkExtra(extra cbor.Map, last uint64) error {
	for _, p := range extra {
		k, ok := p.Key.(uint64)
		if !ok || k <= last {
			return ErrKeyType
		}
	}
	return nil
}

func need(f map[uint64]cbor.Value, k uint64) (cbor.Value, error) {
	v, ok := f[k]
	if !ok {
		return nil, fmt.Errorf("%w: key %d", ErrMissing, k)
	}
	return v, nil
}

func needBytes(f map[uint64]cbor.Value, k uint64) ([]byte, error) {
	v, err := need(f, k)
	if err != nil {
		return nil, err
	}
	b, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("%w: key %d", ErrType, k)
	}
	return b, nil
}

func needText(f map[uint64]cbor.Value, k uint64) (string, error) {
	v, err := need(f, k)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: key %d", ErrType, k)
	}
	return s, nil
}

func needUint(f map[uint64]cbor.Value, k uint64, lo, hi uint64) (uint64, error) {
	v, err := need(f, k)
	if err != nil {
		return 0, err
	}
	n, ok := v.(uint64)
	if !ok {
		return 0, fmt.Errorf("%w: key %d", ErrType, k)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("%w: key %d", ErrRange, k)
	}
	return n, nil
}

func inRange(n, lo, hi uint64, what string) error {
	if n < lo || n > hi {
		return fmt.Errorf("%w: %s", ErrRange, what)
	}
	return nil
}

// claims looks for a definite-length map head (0xa0 to 0xbb), key 0, then a
// four-byte string equal to magic. Key 0 always sorts first in a canonical
// map with unsigned-integer keys. It allocates nothing.
func claims(p, magic []byte) bool {
	if len(p) < 1 || p[0]>>5 != 5 {
		return false
	}
	var i int
	switch ai := p[0] & 0x1f; {
	case ai < 24:
		i = 1
	case ai <= 27:
		i = 1 + 1<<(ai-24)
	default:
		return false
	}
	if len(p) < i+2+len(magic) || p[i] != 0x00 || p[i+1] != 0x44 {
		return false
	}
	return bytes.Equal(p[i+2:i+2+len(magic)], magic)
}

// IsEnvelope reports whether a PushDrop's first field claims to be an
// envelope record.
func IsEnvelope(payload []byte) bool { return claims(payload, MagicEnvelope) }

// IsReceipt reports whether a PushDrop's first field claims to be a receipt
// record.
func IsReceipt(payload []byte) bool { return claims(payload, MagicReceipt) }
