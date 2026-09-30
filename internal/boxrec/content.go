package boxrec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/pushdrop"
)

// BRC-169 envelope members and the one version this contract admits.
const (
	MetanetHandles = "1.0"

	MemberVersion   = "metanetHandles"
	MemberRecipient = "recipient"
	MemberSender    = "sender"
	MemberCreated   = "created"
	MemberQuoteID   = "quoteId"
	MemberPayment   = "payment"
	MemberContent   = "content"
	MemberSignature = "signature"
	MemberIdentity  = "identityKey"
)

// BRC78Version is the four-byte version a BRC-78 message starts with, as
// go-sdk v1.5.2's message package writes it.
var BRC78Version = []byte{0x42, 0x42, 0x10, 0x33}

// BRC78Min is the shortest BRC-78 message: version, sender, recipient, key
// id, a 32-byte IV and a 16-byte GCM tag around an empty plaintext.
const BRC78Min = 4 + 33 + 33 + 32 + 32 + 16

// Content is an envelope record's content, parsed and checked against the
// record.
type Content struct {
	Doc *Object
	// Signed is what the signature covers the SHA-256 of: the RFC 8785
	// serialization of Doc without content and signature (BRC-169 section
	// 7.2 rule 2).
	Signed []byte
	// Cipher is the BRC-78 message the content member carries.
	Cipher []byte
	// Signature is the DER signature the signature member carries.
	Signature []byte
}

// RFC3339 writes t as the one form this contract admits for created:
// YYYY-MM-DDTHH:MM:SSZ.
func RFC3339(t uint64) string {
	return time.Unix(int64(t), 0).UTC().Format("2006-01-02T15:04:05Z") //nolint:gosec // bounded by MaxTime
}

func member[T any](o *Object, name string) (T, bool) {
	var zero T
	v, ok := o.Get(name)
	if !ok {
		return zero, false
	}
	t, ok := v.(T)
	return t, ok
}

// optionalString requires name, when present, to be a string.
func optionalString(o *Object, name string) bool {
	v, ok := o.Get(name)
	if !ok {
		return true
	}
	_, ok = v.(string)
	return ok
}

// checkParty applies the recipient and sender object rules: an object whose
// identityKey is the record's key in lowercase hex, and whose other listed
// members, when present, are strings.
func checkParty(doc *Object, name string, key []byte, optional ...string) error {
	p, ok := member[*Object](doc, name)
	if !ok {
		return fmt.Errorf("%w: %s", ErrContentShape, name)
	}
	id, ok := member[string](p, MemberIdentity)
	if !ok || id != hex.EncodeToString(key) {
		return fmt.Errorf("%w: %s.%s", ErrContentShape, name, MemberIdentity)
	}
	for _, o := range optional {
		if !optionalString(p, o) {
			return fmt.Errorf("%w: %s.%s", ErrContentShape, name, o)
		}
	}
	return nil
}

// ParseContent applies the content rules short of the signature, in their
// order: content-json (the bytes are exactly the canonical serialization of
// a JSON object in the subset), content-shape (the BRC-169 members and their
// agreement with the record), content-cipher (the BRC-78 message's header
// names the record's sender and recipient). The record must already have
// passed DecodeEnvelope or Validate.
func ParseContent(e *Envelope) (*Content, error) {
	v, err := ParseJSON(e.Content)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrContentJSON, err)
	}
	doc, ok := v.(*Object)
	if !ok {
		return nil, ErrContentJSON
	}
	canon, err := Canonical(doc)
	if err != nil || !bytes.Equal(canon, e.Content) {
		return nil, fmt.Errorf("%w: not the canonical serialization", ErrContentJSON)
	}

	if s, ok := member[string](doc, MemberVersion); !ok || s != MetanetHandles {
		return nil, fmt.Errorf("%w: %s", ErrContentShape, MemberVersion)
	}
	if err := checkParty(doc, MemberRecipient, e.To, "handle", "tag", "domain"); err != nil {
		return nil, err
	}
	if err := checkParty(doc, MemberSender, e.From, "handle", "domain"); err != nil {
		return nil, err
	}
	if s, ok := member[string](doc, MemberCreated); !ok || s != RFC3339(e.Created) {
		return nil, fmt.Errorf("%w: %s", ErrContentShape, MemberCreated)
	}
	if !optionalString(doc, MemberQuoteID) {
		return nil, fmt.Errorf("%w: %s", ErrContentShape, MemberQuoteID)
	}
	if p, ok := doc.Get(MemberPayment); !ok || p != nil {
		return nil, fmt.Errorf("%w: %s must be null", ErrContentShape, MemberPayment)
	}
	b64, ok := member[string](doc, MemberContent)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrContentShape, MemberContent)
	}
	sigHex, ok := member[string](doc, MemberSignature)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrContentShape, MemberSignature)
	}

	cipher, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil || len(cipher) < BRC78Min {
		return nil, fmt.Errorf("%w: not a BRC-78 message in standard base64", ErrContentCipher)
	}
	if !bytes.Equal(cipher[:4], BRC78Version) || !bytes.Equal(cipher[4:37], e.From) || !bytes.Equal(cipher[37:70], e.To) {
		return nil, fmt.Errorf("%w: header does not name the record's sender and recipient", ErrContentCipher)
	}

	signed, err := Canonical(doc.Without(MemberContent, MemberSignature))
	if err != nil {
		return nil, ErrContentJSON
	}
	c := &Content{Doc: doc, Signed: signed, Cipher: cipher}
	c.Signature, err = hex.DecodeString(sigHex)
	if err != nil || hex.EncodeToString(c.Signature) != sigHex {
		// A lowercase hex string only; checked here, reported as the
		// signature rule.
		c.Signature = nil
	}
	return c, nil
}

// strictDER parses a strict DER signature with a low S value: the one
// encoding go-sdk writes.
func strictDER(b []byte) (*ec.Signature, bool) {
	if len(b) == 0 {
		return nil, false
	}
	sig, err := ec.ParseDERSignature(b)
	if err != nil || !bytes.Equal(sig.Serialize(), b) {
		return nil, false
	}
	return sig, true
}

// VerifySignature is the content-signature rule: the signature member is
// lowercase hex of a strict low-S DER signature over SHA-256(Signed) that
// verifies under the sender's key for [1, "bbox message"], key id
// "signature", counterparty anyone.
func (c *Content) VerifySignature(from []byte) error {
	sig, ok := strictDER(c.Signature)
	if !ok {
		return fmt.Errorf("%w: not lowercase hex of a strict low-S DER signature", ErrContentSignature)
	}
	fromKey, err := ec.PublicKeyFromBytes(from)
	if err != nil {
		return ErrIdentity
	}
	key, err := SignatureDerivation.ExpectedLockingKey(fromKey)
	if err != nil {
		return err
	}
	h := sha256.Sum256(c.Signed)
	if !sig.Verify(h[:], key) {
		return ErrContentSignature
	}
	return nil
}

// CheckContent applies every content rule in order: ParseContent, then
// VerifySignature.
func CheckContent(e *Envelope) (*Content, error) {
	c, err := ParseContent(e)
	if err != nil {
		return nil, err
	}
	if err := c.VerifySignature(e.From); err != nil {
		return nil, err
	}
	return c, nil
}

// Signer is the one wallet method Seal needs; every wallet.Interface has it.
type Signer interface {
	CreateSignature(ctx context.Context, args wallet.CreateSignatureArgs, originator string) (*wallet.CreateSignatureResult, error)
}

// Seal completes a BRC-169 envelope for the sender: it sets payment to null,
// the content member to the BRC-78 message in standard base64, and the
// signature member to the wallet's signature, under the sender's key for
// [1, "bbox message"] and key id "signature", over SHA-256 of the RFC 8785
// serialization without content and signature. It returns the carried
// bytes: the canonical serialization of the whole envelope.
func Seal(ctx context.Context, w Signer, originator string, doc *Object, brc78 []byte) ([]byte, error) {
	if w == nil {
		return nil, errors.New("boxrec: nil wallet")
	}
	doc.Set(MemberPayment, nil)
	doc.Set(MemberContent, base64.StdEncoding.EncodeToString(brc78))
	signed, err := Canonical(doc.Without(MemberContent, MemberSignature))
	if err != nil {
		return nil, err
	}
	r, err := w.CreateSignature(ctx, wallet.CreateSignatureArgs{
		EncryptionArgs: wallet.EncryptionArgs{
			ProtocolID: SignatureDerivation.Protocol, KeyID: SignatureDerivation.KeyID, Counterparty: pushdrop.Anyone(),
		},
		Data: signed,
	}, originator)
	if err != nil {
		return nil, err
	}
	doc.Set(MemberSignature, hex.EncodeToString(r.Signature.Serialize()))
	return Canonical(doc)
}
