package boxrec

import (
	"bytes"
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"
)

// Receipt is one receipt record: the payload of one receipt carrier. It
// acknowledges envelopes by their commitments.
type Receipt struct {
	Office  string     // key 1
	By      []byte     // key 2, the acknowledging recipient's identity key
	Acks    [][32]byte // key 3, envelope commitments, hash byte order, ascending
	Created uint64     // key 4, Unix seconds, the recipient's clock
	// Extra holds integer keys above 4, preserved verbatim and ignored.
	Extra cbor.Map
}

// checkAcks requires 1 to MaxAcks commitments in strictly ascending byte
// order, so one set has one encoding.
func checkAcks(acks [][32]byte) error {
	if len(acks) < 1 || len(acks) > MaxAcks {
		return ErrAcks
	}
	for i := 1; i < len(acks); i++ {
		if bytes.Compare(acks[i-1][:], acks[i][:]) >= 0 {
			return ErrAcks
		}
	}
	return nil
}

// Validate applies every rule that needs only the record itself.
func (r *Receipt) Validate() error {
	if err := checkExtra(r.Extra, rkLast); err != nil {
		return err
	}
	if len(r.Extra)+5 > MaxKeys {
		return fmt.Errorf("%w: more than %d keys", ErrTooLarge, MaxKeys)
	}
	if err := CheckOffice(r.Office); err != nil {
		return err
	}
	if err := CheckIdentity(r.By); err != nil {
		return err
	}
	if err := checkAcks(r.Acks); err != nil {
		return err
	}
	return inRange(r.Created, 1, MaxTime, "created")
}

// Encode writes the receipt record in canonical CBOR.
func (r *Receipt) Encode() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	acks := make([]cbor.Value, len(r.Acks))
	for i := range r.Acks {
		acks[i] = append([]byte(nil), r.Acks[i][:]...)
	}
	m := cbor.Map{
		{Key: uint64(RKMagic), Val: MagicReceipt},
		{Key: uint64(RKOffice), Val: r.Office},
		{Key: uint64(RKBy), Val: r.By},
		{Key: uint64(RKAcks), Val: acks},
		{Key: uint64(RKCreated), Val: r.Created},
	}
	m = append(m, r.Extra...)
	out, err := cbor.Encode(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCBOR, err)
	}
	if len(out) > MaxReceiptRecord {
		return nil, ErrTooLarge
	}
	return out, nil
}

// DecodeReceipt parses a receipt record in the refusal order.
func DecodeReceipt(p []byte) (*Receipt, error) {
	m, err := decodeMap(p, MaxReceiptRecord)
	if err != nil {
		return nil, err
	}
	f, extra, err := intKeys(m, rkLast)
	if err != nil {
		return nil, err
	}
	if err := checkMagic(f, MagicReceipt); err != nil {
		return nil, err
	}
	r := &Receipt{Extra: extra}
	if r.Office, err = needText(f, RKOffice); err != nil {
		return nil, err
	}
	if err := CheckOffice(r.Office); err != nil {
		return nil, err
	}
	if r.By, err = needBytes(f, RKBy); err != nil {
		return nil, err
	}
	if err := CheckIdentity(r.By); err != nil {
		return nil, err
	}
	v, err := need(f, RKAcks)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]cbor.Value)
	if !ok {
		return nil, fmt.Errorf("%w: key %d", ErrType, RKAcks)
	}
	// The count is bounded before anything is allocated for it.
	if len(arr) < 1 || len(arr) > MaxAcks {
		return nil, ErrAcks
	}
	r.Acks = make([][32]byte, len(arr))
	for i, a := range arr {
		b, ok := a.([]byte)
		if !ok || len(b) != 32 {
			return nil, ErrAcks
		}
		copy(r.Acks[i][:], b)
	}
	if err := checkAcks(r.Acks); err != nil {
		return nil, err
	}
	if r.Created, err = needUint(f, RKCreated, 1, MaxTime); err != nil {
		return nil, err
	}
	return r, nil
}
