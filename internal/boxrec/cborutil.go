package boxrec

import (
	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/record"
)

func decodeMap(b []byte, max int) (cbor.Map, error) { return record.DecodeMap(b, max) }

func intKeys(m cbor.Map, last uint64) (*record.Fields, cbor.Map, error) {
	f, err := record.Split(m, last)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Extra, nil
}

func checkMagic(f *record.Fields, magic []byte) error { return f.CheckMagic(magic) }

func checkExtra(extra cbor.Map, last uint64) error { return record.CheckExtraKeys(extra, last) }

func need(f *record.Fields, k uint64) (cbor.Value, error) { return f.Need(k) }

func needBytes(f *record.Fields, k uint64) ([]byte, error) { return f.Bytes(k) }

func needText(f *record.Fields, k uint64) (string, error) { return f.Text(k) }

func needUint(f *record.Fields, k uint64, lo, hi uint64) (uint64, error) { return f.Uint(k, lo, hi) }

func inRange(n, lo, hi uint64, what string) error { return record.InRange(n, lo, hi, what) }

func claims(p, magic []byte) bool { return record.Claims(p, magic) }

// IsEnvelope reports whether a PushDrop's first field claims to be an
// envelope record.
func IsEnvelope(payload []byte) bool { return claims(payload, MagicEnvelope) }

// IsReceipt reports whether a PushDrop's first field claims to be a receipt
// record.
func IsReceipt(payload []byte) bool { return claims(payload, MagicReceipt) }
