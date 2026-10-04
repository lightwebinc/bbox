package boxrec

import (
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"
)

// Envelope is one envelope record: the payload of one envelope carrier.
type Envelope struct {
	Office  string // key 1, the office identifier <name>_<suffix>
	To      []byte // key 2, the recipient's identity key
	Box     string // key 3, the box name
	From    []byte // key 4, the sender's identity key
	Created uint64 // key 5, Unix seconds, the sender's clock
	Expires uint64 // key 6, Unix seconds; 0 means none
	Content []byte // key 7, the BRC-169 envelope, JSON, as carried
	// Extra holds integer keys above 7, preserved verbatim and ignored.
	Extra cbor.Map
}

// Validate applies every rule that needs only the record itself, in the
// refusal order (steps 6 and 7). Encode runs it, so a sender cannot write
// what a reader refuses. The content's own rules are CheckContent's.
func (e *Envelope) Validate() error {
	if err := checkExtra(e.Extra, ekLast); err != nil {
		return err
	}
	if len(e.Extra)+8 > MaxKeys {
		return fmt.Errorf("%w: more than %d keys", ErrTooLarge, MaxKeys)
	}
	if err := CheckOffice(e.Office); err != nil {
		return err
	}
	if err := CheckIdentity(e.To); err != nil {
		return err
	}
	if err := CheckBox(e.Box); err != nil {
		return err
	}
	if err := CheckIdentity(e.From); err != nil {
		return err
	}
	if err := inRange(e.Created, 1, MaxTime, "created"); err != nil {
		return err
	}
	if err := inRange(e.Expires, 0, MaxTime, "expires"); err != nil {
		return err
	}
	if err := inRange(uint64(len(e.Content)), 1, MaxContent, "content"); err != nil {
		return err
	}
	return e.crossRules()
}

// crossRules is step 7.
func (e *Envelope) crossRules() error {
	if e.Expires != 0 && e.Expires <= e.Created {
		return ErrExpires
	}
	return nil
}

// Encode writes the envelope record in canonical CBOR.
func (e *Envelope) Encode() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	m := cbor.Map{
		{Key: uint64(EKMagic), Val: MagicEnvelope},
		{Key: uint64(EKOffice), Val: e.Office},
		{Key: uint64(EKTo), Val: e.To},
		{Key: uint64(EKBox), Val: e.Box},
		{Key: uint64(EKFrom), Val: e.From},
		{Key: uint64(EKCreated), Val: e.Created},
		{Key: uint64(EKExpires), Val: e.Expires},
		{Key: uint64(EKContent), Val: e.Content},
	}
	m = append(m, e.Extra...)
	out, err := cbor.Encode(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCBOR, err)
	}
	if len(out) > MaxEnvelopeRecord {
		return nil, ErrTooLarge
	}
	return out, nil
}

// DecodeEnvelope parses an envelope record in the refusal order: the bound,
// canonical CBOR and the map's shape, the key types, the magic, then each
// defined key in ascending order (missing, type, the key's own rule), then
// the rule across fields.
func DecodeEnvelope(p []byte) (*Envelope, error) {
	m, err := decodeMap(p, MaxEnvelopeRecord)
	if err != nil {
		return nil, err
	}
	f, extra, err := intKeys(m, ekLast)
	if err != nil {
		return nil, err
	}
	if err := checkMagic(f, MagicEnvelope); err != nil {
		return nil, err
	}
	e := &Envelope{Extra: extra}
	if e.Office, err = needText(f, EKOffice); err != nil {
		return nil, err
	}
	if err := CheckOffice(e.Office); err != nil {
		return nil, err
	}
	if e.To, err = needBytes(f, EKTo); err != nil {
		return nil, err
	}
	if err := CheckIdentity(e.To); err != nil {
		return nil, err
	}
	if e.Box, err = needText(f, EKBox); err != nil {
		return nil, err
	}
	if err := CheckBox(e.Box); err != nil {
		return nil, err
	}
	if e.From, err = needBytes(f, EKFrom); err != nil {
		return nil, err
	}
	if err := CheckIdentity(e.From); err != nil {
		return nil, err
	}
	if e.Created, err = needUint(f, EKCreated, 1, MaxTime); err != nil {
		return nil, err
	}
	if e.Expires, err = needUint(f, EKExpires, 0, MaxTime); err != nil {
		return nil, err
	}
	if e.Content, err = needBytes(f, EKContent); err != nil {
		return nil, err
	}
	if err := inRange(uint64(len(e.Content)), 1, MaxContent, "content"); err != nil {
		return nil, err
	}
	if err := e.crossRules(); err != nil {
		return nil, err
	}
	return e, nil
}
