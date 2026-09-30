package boxrec

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	hash "github.com/bsv-blockchain/go-sdk/primitives/hash"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/guard"
)

// The recipient's refusals of what it decrypts (docs/spec.md sections 4.5,
// 10 and 13). A host never sees the plaintext, so none of these is a host
// rule: they are the recipient client's, in the order a client applies
// them, and a client that refuses a payment shows the message without it.
var (
	ErrUndecryptable  = errors.New("boxrec: the recipient cannot decrypt the envelope")
	ErrPlaintextJSON  = errors.New("boxrec: plaintext is not a JSON object in the subset")
	ErrPlaintextShape = errors.New("boxrec: plaintext member has the wrong shape")
	ErrPaymentShape   = errors.New("boxrec: payment has the wrong shape")
	ErrPaymentExpires = errors.New("boxrec: payment envelope does not expire at least two hours after it was created")
	ErrPaymentLate    = errors.New("boxrec: payment is past its internalization window")
	ErrPaymentBEEF    = errors.New("boxrec: payment is not an Atomic BEEF that parses")
	ErrPaymentOutput  = errors.New("boxrec: payment output does not pay the recipient what it says")
	ErrPaymentSPV     = errors.New("boxrec: payment does not verify against the recipient's headers")
)

// Plaintext members (docs/spec.md section 4.5).
const (
	PlainBody    = "body"
	PlainPayment = "payment"
	PlainRefs    = "refs"
)

// PaymentProtocol is BRC-29's derivation protocol: a payment output is
// P2PKH to the key derived under it with key id "<prefix> <suffix>".
var PaymentProtocol = wallet.Protocol{SecurityLevel: 2, Protocol: "3241645161d8"}

// Payment timing (docs/spec.md section 10), seconds.
const (
	// PaymentMinLife is the least expires - created of an envelope whose
	// payment a recipient takes.
	PaymentMinLife = 7200
	// PaymentMargin is how long before expires a recipient stops
	// internalizing, and after it a sender may reclaim.
	PaymentMargin = 3600
)

// Plaintext is what the recipient decrypts: a canonical JSON object in the
// subset whose members of section 4.5 have their shapes. Doc keeps every
// member, unknown ones included.
type Plaintext struct {
	Doc     *Object
	Body    *string
	Payment *Payment
	Refs    []Ref
}

// Ref is a reference to content larger than the bound.
type Ref struct {
	URL    string
	SHA256 [32]byte
	Length uint64
	// Key is the AES-256 key the referenced bytes are encrypted under, or
	// nil when they are not.
	Key []byte
}

// Payment is a BRC-29 payment made as BRC-169 section 6.1 describes.
type Payment struct {
	// BEEF is the payment transaction as Atomic BEEF (BRC-95).
	BEEF             []byte
	DerivationPrefix string
	Outputs          []PaymentOutput
}

// PaymentOutput is one output paid to the recipient.
type PaymentOutput struct {
	OutputIndex      uint32
	DerivationSuffix string
	Satoshis         uint64
}

// Decrypter is the wallet method Open needs; every wallet.Interface has it.
type Decrypter interface {
	Decrypt(ctx context.Context, args wallet.DecryptArgs, originator string) (*wallet.DecryptResult, error)
}

// Open decrypts an envelope's BRC-78 message through the recipient's
// wallet: protocol [2, "message encryption"], the message's key id in
// base64, counterparty the sender. A failure is ErrUndecryptable, which a
// reader shows as such, never as an empty message.
func Open(ctx context.Context, w Decrypter, originator string, c *Content, from []byte) ([]byte, error) {
	sender, err := guard.ParsePubKey(from)
	if err != nil || len(c.Cipher) < BRC78Min {
		return nil, ErrUndecryptable
	}
	r, err := w.Decrypt(ctx, wallet.DecryptArgs{
		EncryptionArgs: wallet.EncryptionArgs{
			ProtocolID:   wallet.Protocol{SecurityLevel: 2, Protocol: "message encryption"},
			KeyID:        base64.StdEncoding.EncodeToString(c.Cipher[70:102]),
			Counterparty: wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: sender},
		},
		Ciphertext: c.Cipher[102:],
	}, originator)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUndecryptable, err)
	}
	return r.Plaintext, nil
}

// isB64 reports whether s is non-empty standard base64 with padding.
func isB64(s string) bool {
	if s == "" {
		return false
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	return err == nil && base64.StdEncoding.EncodeToString(b) == s
}

// ParsePlaintext applies the plaintext's rules in order: plaintext-json (the
// bytes are the canonical serialization of a JSON object in the subset),
// plaintext-shape (body a string; refs an array of well-formed references),
// payment-shape (payment an object with beef, derivationPrefix and a
// non-empty outputs array of distinct output indices). Unknown members are
// kept in Doc and ignored.
func ParsePlaintext(b []byte) (*Plaintext, error) {
	v, err := ParseJSON(b)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPlaintextJSON, err)
	}
	doc, ok := v.(*Object)
	if !ok {
		return nil, ErrPlaintextJSON
	}
	if c, err := Canonical(doc); err != nil || !bytes.Equal(c, b) {
		return nil, fmt.Errorf("%w: not the canonical serialization", ErrPlaintextJSON)
	}
	p := &Plaintext{Doc: doc}
	if v, ok := doc.Get(PlainBody); ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrPlaintextShape, PlainBody)
		}
		p.Body = &s
	}
	if v, ok := doc.Get(PlainRefs); ok {
		if p.Refs, err = parseRefs(v); err != nil {
			return nil, err
		}
	}
	if v, ok := doc.Get(PlainPayment); ok {
		if p.Payment, err = parsePayment(v); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func parseRefs(v any) ([]Ref, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrPlaintextShape, PlainRefs)
	}
	var out []Ref
	for i, e := range arr {
		o, ok := e.(*Object)
		if !ok {
			return nil, fmt.Errorf("%w: refs[%d]", ErrPlaintextShape, i)
		}
		url, _ := member[string](o, "url")
		digest, _ := member[string](o, "sha256")
		length, lok := member[Int](o, "length")
		if !(strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "uhrp://")) || !isLowerHex(digest, 64) || !lok || length < 0 {
			return nil, fmt.Errorf("%w: refs[%d]", ErrPlaintextShape, i)
		}
		r := Ref{URL: url, Length: uint64(length)}
		copy(r.SHA256[:], mustHex(digest))
		if k, ok := o.Get("key"); ok {
			ks, ok := k.(string)
			if !ok || !isLowerHex(ks, 64) {
				return nil, fmt.Errorf("%w: refs[%d].key", ErrPlaintextShape, i)
			}
			r.Key = mustHex(ks)
		}
		out = append(out, r)
	}
	return out, nil
}

func parsePayment(v any) (*Payment, error) {
	o, ok := v.(*Object)
	if !ok {
		return nil, fmt.Errorf("%w: not an object", ErrPaymentShape)
	}
	b64, _ := member[string](o, "beef")
	prefix, _ := member[string](o, "derivationPrefix")
	outs, ok := member[[]any](o, "outputs")
	if !isB64(b64) || !isB64(prefix) || !ok || len(outs) == 0 {
		return nil, fmt.Errorf("%w: beef, derivationPrefix or outputs", ErrPaymentShape)
	}
	p := &Payment{DerivationPrefix: prefix}
	p.BEEF, _ = base64.StdEncoding.DecodeString(b64)
	seen := map[uint32]bool{}
	for i, e := range outs {
		eo, ok := e.(*Object)
		if !ok {
			return nil, fmt.Errorf("%w: outputs[%d]", ErrPaymentShape, i)
		}
		idx, iok := member[Int](eo, "outputIndex")
		suffix, _ := member[string](eo, "derivationSuffix")
		sats, sok := member[Int](eo, "satoshis")
		if !iok || idx < 0 || idx > 0xffffffff || !isB64(suffix) || !sok || sats < 1 {
			return nil, fmt.Errorf("%w: outputs[%d]", ErrPaymentShape, i)
		}
		if seen[uint32(idx)] {
			return nil, fmt.Errorf("%w: output %d listed twice", ErrPaymentShape, idx)
		}
		seen[uint32(idx)] = true
		p.Outputs = append(p.Outputs, PaymentOutput{OutputIndex: uint32(idx), DerivationSuffix: suffix, Satoshis: uint64(sats)})
	}
	return p, nil
}

// KeyGetter is the wallet method CheckPayment needs; every
// wallet.Interface has it.
type KeyGetter interface {
	GetPublicKey(ctx context.Context, args wallet.GetPublicKeyArgs, originator string) (*wallet.GetPublicKeyResult, error)
}

// P2PKH is the pay-to-public-key-hash locking script of key.
func P2PKH(key *ec.PublicKey) []byte {
	return append(append([]byte{0x76, 0xa9, 0x14}, hash.Hash160(key.Compressed())...), 0x88, 0xac)
}

// CheckPayment is what a recipient checks before it internalizes a payment
// from envelope e, at its time now, in order: payment-expires (e expires at
// least PaymentMinLife after it was created), payment-late (now is before
// expires - PaymentMargin), payment-beef (an Atomic BEEF that parses, with
// its subject), payment-output (each listed output exists, holds the
// satoshis listed, and is P2PKH to the key the recipient's wallet derives
// under BRC-29 for the prefix, the suffix and the sender), payment-spv (the
// transaction verifies against the recipient's headers, BRC-67). It
// returns the payment transaction, for the wallet's internalizeAction.
func CheckPayment(ctx context.Context, w KeyGetter, originator string, e *Envelope, p *Payment,
	headers chaintracker.ChainTracker, now uint64) (*transaction.Transaction, error) {
	if e.Expires == 0 || e.Expires < e.Created+PaymentMinLife {
		return nil, ErrPaymentExpires
	}
	if now+PaymentMargin >= e.Expires {
		return nil, ErrPaymentLate
	}
	if len(p.BEEF) < 4 || binary.LittleEndian.Uint32(p.BEEF) != 0x01010101 {
		return nil, fmt.Errorf("%w: not Atomic BEEF", ErrPaymentBEEF)
	}
	_, tx, _, err := guard.ParseBEEF(p.BEEF, MaxContent)
	if err != nil || tx == nil {
		return nil, fmt.Errorf("%w: %v", ErrPaymentBEEF, err)
	}
	sender, err := guard.ParsePubKey(e.From)
	if err != nil {
		return nil, ErrIdentity
	}
	forSelf := true
	for _, o := range p.Outputs {
		if uint64(o.OutputIndex) >= uint64(len(tx.Outputs)) {
			return nil, fmt.Errorf("%w: output %d of %d", ErrPaymentOutput, o.OutputIndex, len(tx.Outputs))
		}
		out := tx.Outputs[o.OutputIndex]
		if out.Satoshis != o.Satoshis {
			return nil, fmt.Errorf("%w: output %d holds %d, not %d", ErrPaymentOutput, o.OutputIndex, out.Satoshis, o.Satoshis)
		}
		k, err := w.GetPublicKey(ctx, wallet.GetPublicKeyArgs{
			EncryptionArgs: wallet.EncryptionArgs{
				ProtocolID:   PaymentProtocol,
				KeyID:        p.DerivationPrefix + " " + o.DerivationSuffix,
				Counterparty: wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: sender},
			},
			ForSelf: &forSelf,
		}, originator)
		if err != nil {
			return nil, err
		}
		if out.LockingScript == nil || !bytes.Equal(*out.LockingScript, P2PKH(k.PublicKey)) {
			return nil, fmt.Errorf("%w: output %d does not pay the recipient's key", ErrPaymentOutput, o.OutputIndex)
		}
	}
	if ok, err := spv.Verify(ctx, tx, headers, nil); err != nil || !ok {
		return nil, fmt.Errorf("%w: %v", ErrPaymentSPV, err)
	}
	return tx, nil
}

// mustHex decodes hex a caller has already held to isLowerHex.
func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
