// Package boxrec is the byte-level contract of bbox: the envelope record an
// envelope carrier holds, the receipt record a receipt carrier holds, the
// BRC-169 envelope a record's content is, the office and box name grammars,
// and the lookup question classes. docs/spec.md is the normative text; this
// package generates the golden vectors under testdata/vectors.
//
// Every record is canonical CBOR through bcommon's cbor package, so one value
// has one encoding a reader accepts. Every decoder bounds its input before
// the CBOR decoder runs, and the CBOR decoder bounds every declared length
// against the bytes present before it allocates.
package boxrec

import (
	"errors"

	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/pushdrop"
)

// Registry values (docs/frozen.md); frozen once the first record is
// published to a public host.
const (
	// ProtocolName is the BRC-43 protocol name, security level 1. BRC-43
	// key derivation refuses a name under five characters, so the
	// application name alone ("bbox") cannot be the protocol name.
	ProtocolName = "bbox message"
	// KeyEnvelope locks every carrier's record output (envelopes and
	// receipts), signs its field, and locks the funding-tree outputs a
	// carrier spends.
	KeyEnvelope = "envelope"
	// KeySignature signs the BRC-169 envelope a record's content is.
	KeySignature = "signature"
	// KeyFund is the embedded wallet's funding key.
	KeyFund = "fund"

	// TopicPrefix and LookupService are the overlay names (BRC-87).
	TopicPrefix   = "tm_bbox_"
	LookupService = "ls_bbox"
)

// Tags and magics. The tag prefix "bb" belongs to bbox.
var (
	TagPrefix = []byte("bb")
	// TagFunding is the one field of a funding-tree output.
	TagFunding = []byte{'b', 'b', 0x02}
	// MagicEnvelope is key 0 of an envelope record: "bb", 'e', version 1.
	MagicEnvelope = []byte{'b', 'b', 'e', 0x01}
	// MagicReceipt is key 0 of a receipt record: "bb", 'r', version 1.
	MagicReceipt = []byte{'b', 'b', 'r', 0x01}
)

// Derivations under [1, "bbox message"], each with counterparty anyone, so a
// reader derives the key from the identity key alone.
var (
	Protocol            = wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryApp, Protocol: ProtocolName}
	EnvelopeDerivation  = pushdrop.Derivation{Protocol: Protocol, KeyID: KeyEnvelope}
	SignatureDerivation = pushdrop.Derivation{Protocol: Protocol, KeyID: KeySignature}
	FundDerivation      = pushdrop.Derivation{Protocol: Protocol, KeyID: KeyFund}
)

// Bounds. A reader accepts every record within them; a later version may
// raise a bound but never lower it (docs/frozen.md).
const (
	// MaxSafe is 2^53 - 1.
	MaxSafe = 1<<53 - 1
	// MaxTime is 9999-12-31T23:59:59Z, the last second RFC 3339 can write.
	MaxTime = 253402300799
	// SuffixLen is the length of an office's random suffix.
	SuffixLen = 10
	// MaxOffice is the longest office identifier: BRC-87 caps a name at 50
	// characters and the topic prefix takes 8.
	MaxOffice = 50 - len(TopicPrefix)
	// MaxOfficeName is the longest readable part of an office identifier.
	MaxOfficeName = MaxOffice - 1 - SuffixLen
	// MinOffice is the shortest office identifier.
	MinOffice = 1 + 1 + SuffixLen
	// MaxBox is the longest box name.
	MaxBox = 50
	// MaxContent bounds an envelope's content (key 7), the BRC-169
	// envelope as carried, in bytes.
	MaxContent = 16 << 10
	// MaxEnvelopeRecord bounds an encoded envelope record.
	MaxEnvelopeRecord = MaxContent + 4096
	// MaxAcks bounds the commitments one receipt acknowledges.
	MaxAcks = 64
	// MaxReceiptRecord bounds an encoded receipt record.
	MaxReceiptRecord = 4096
	// MaxKeys bounds the entries of a record map, unknown keys included.
	MaxKeys = 64
)

// Envelope record keys.
const (
	EKMagic   = 0
	EKOffice  = 1
	EKTo      = 2
	EKBox     = 3
	EKFrom    = 4
	EKCreated = 5
	EKExpires = 6
	EKContent = 7
	ekLast    = EKContent
)

// Receipt record keys.
const (
	RKMagic   = 0
	RKOffice  = 1
	RKBy      = 2
	RKAcks    = 3
	RKCreated = 4
	rkLast    = RKCreated
)

// The refusals. A host counts them by Reason.
var (
	ErrTooLarge = errors.New("boxrec: record exceeds its bound")
	ErrCBOR     = errors.New("boxrec: not a canonical CBOR map")
	ErrKeyType  = errors.New("boxrec: record key is not an unsigned integer")
	ErrMagic    = errors.New("boxrec: wrong magic")
	ErrMissing  = errors.New("boxrec: required key missing")
	ErrType     = errors.New("boxrec: field has the wrong type")
	ErrRange    = errors.New("boxrec: field out of range")
	ErrIdentity = errors.New("boxrec: identity key is not a canonical compressed key")
	ErrOffice   = errors.New("boxrec: office identifier breaks the grammar")
	ErrBox      = errors.New("boxrec: box name breaks the grammar")
	ErrExpires  = errors.New("boxrec: expires is not zero and not after created")
	ErrAcks     = errors.New("boxrec: acks are not 1 to 64 distinct commitments in ascending order")

	ErrContentJSON      = errors.New("boxrec: content is not a JSON object in the subset")
	ErrContentShape     = errors.New("boxrec: content is not a bbox BRC-169 envelope")
	ErrContentCipher    = errors.New("boxrec: content's encrypted payload does not match the record")
	ErrContentSignature = errors.New("boxrec: content signature does not verify")

	ErrQuery = errors.New("boxrec: question is not one of the defined classes")
)

// Reason is the fixed label a host counts a refusal by.
func Reason(err error) string {
	for _, e := range []struct {
		err error
		s   string
	}{
		{ErrTooLarge, "too-large"}, {ErrCBOR, "cbor"}, {ErrKeyType, "key-type"},
		{ErrMagic, "magic"}, {ErrMissing, "missing"}, {ErrType, "type"},
		{ErrRange, "range"}, {ErrIdentity, "identity"}, {ErrOffice, "office"},
		{ErrBox, "box"}, {ErrExpires, "expires"}, {ErrAcks, "acks"},
		{ErrContentJSON, "content-json"}, {ErrContentShape, "content-shape"},
		{ErrContentCipher, "content-cipher"}, {ErrContentSignature, "content-signature"},
		{ErrQuery, "query"},
	} {
		if errors.Is(err, e.err) {
			return e.s
		}
	}
	return "other"
}
