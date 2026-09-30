package boxrec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/guard"
)

// The transaction refusals of docs/spec.md section 8.1, beside the record
// and content refusals. A transaction is refused for the first rule it
// breaks. A carrier whose record names another office than the topic's is
// refused with ErrOffice, the label of a record whose office breaks the
// grammar.
var (
	ErrCarrierShape = errors.New("boxrec: carrier has the wrong shape")
	ErrBEEF         = errors.New("boxrec: BEEF is not exactly the carrier and its proven funding tree")
	ErrMineable     = errors.New("boxrec: carrier is mineable")
	ErrUnlock       = errors.New("boxrec: unlocking script is not in canonical form")
	ErrLock         = errors.New("boxrec: output script is not the canonical one for the owner")
	ErrSignature    = errors.New("boxrec: field signature is not strict or does not verify")
	ErrFunding      = errors.New("boxrec: carrier does not spend a funding output of the owner")
	ErrUnmined      = errors.New("boxrec: sweep has no proof that verifies")
	ErrNotBbox      = errors.New("boxrec: transaction is not for this topic")
)

// TxKind is what an admitted transaction is to the topic.
type TxKind int

// The kinds of admitted transaction.
const (
	// TxEnvelope is an envelope carrier: output 0 is admitted.
	TxEnvelope TxKind = iota + 1
	// TxReceipt is a receipt carrier: output 0 is admitted.
	TxReceipt
	// TxSweep is a mined sweep, or a published funding tree: output 0 is
	// admitted and every input's outpoint is recorded.
	TxSweep
	// TxSpend admits nothing and retains the held outputs it spends.
	TxSpend
)

func (k TxKind) String() string {
	switch k {
	case TxEnvelope:
		return "envelope"
	case TxReceipt:
		return "receipt"
	case TxSweep:
		return "sweep"
	case TxSpend:
		return "spend"
	}
	return "none"
}

// Host is what admission needs from the host beyond the object: the
// topic's office, a chain tracker over the host's own block headers, and
// the host's BEEF bound (DefaultMaxBEEF when zero).
type Host struct {
	Office  string
	Headers chaintracker.ChainTracker
	MaxBEEF int
}

// Carrier is an admitted envelope or receipt carrier.
type Carrier struct {
	// Envelope and Content are set for an envelope carrier, Receipt for a
	// receipt carrier.
	Envelope *Envelope
	Content  *Content
	Receipt  *Receipt
	// Owner is the record's from or by, the key the carrier is bound to.
	Owner []byte
	// Record is the record exactly as carried.
	Record []byte
	// Commitment is C = txid(K), hash byte order.
	Commitment [32]byte
	// Funding is the outpoint the carrier spends; a mined spend of it by
	// another transaction retracts the carrier.
	Funding transaction.Outpoint
}

// Admission is what a host admits from one transaction: the outputs to
// admit and the held inputs to retain (BRC-22's admittance instructions),
// and what the lookup service records.
type Admission struct {
	Kind TxKind
	// Txid is the transaction's txid, hash byte order.
	Txid [32]byte
	// Outputs are the output indices admitted.
	Outputs []int
	// Retain are the input indices that spend an output the topic holds:
	// every one is retained, so no admitted output is deleted when
	// something spends it.
	Retain []int
	// Carrier is set for TxEnvelope and TxReceipt.
	Carrier *Carrier
	// Spent is, for TxSweep, every outpoint the sweep's inputs spend.
	Spent []transaction.Outpoint
}

// Admit applies docs/spec.md section 8.1 to one submission: the BEEF as it
// arrived, and held, the indices of its subject's inputs that spend an output
// the topic holds (BRC-22's previous coins, as the engine hands them to the
// topic manager). A carrier's funding output is read from the carrier's own
// BEEF; no other verdict reads anything but the transaction, its BEEF and
// the headers, except that a transaction claiming nothing that is not a
// sweep is accepted only when held names an input. SPV of the BEEF is the
// engine's and is assumed to have run.
//
// Every refusal is an error: a topic manager raises it rather than answer
// with empty instructions, so the engine records nothing for the txid.
func Admit(ctx context.Context, beef []byte, held []int, h Host) (*Admission, error) {
	if CheckOffice(h.Office) != nil {
		return nil, fmt.Errorf("boxrec: host office %q breaks the grammar", h.Office)
	}
	bound := h.MaxBEEF
	if bound == 0 {
		bound = DefaultMaxBEEF
	}
	b, tx, _, err := guard.ParseBEEF(beef, bound)
	if err != nil || tx == nil {
		return nil, fmt.Errorf("%w: %v", ErrBEEF, err)
	}
	retain, err := retained(tx, held)
	if err != nil {
		return nil, err
	}
	a := &Admission{Txid: [32]byte(*tx.TxID()), Retain: retain}

	// Classify: the one output that claims a record.
	claim, claims := KindNone, 0
	for _, o := range tx.Outputs {
		if o == nil || o.LockingScript == nil {
			continue
		}
		if k := ClaimOf(*o.LockingScript); k != KindNone {
			claim = k
			claims++
		}
	}
	switch {
	case claims > 1:
		return nil, fmt.Errorf("%w: %d outputs claim a record", ErrCarrierShape, claims)
	case claims == 1:
		c, err := checkCarrier(beef, b, tx, claim, h)
		if err != nil {
			return nil, err
		}
		a.Kind, a.Carrier, a.Outputs = TxReceipt, c, []int{0}
		if claim == KindEnvelope {
			a.Kind = TxEnvelope
		}
		return a, nil
	}

	// Sweep: output 0 funding-shaped under any key, and mined.
	if len(tx.Outputs) > 0 && tx.Outputs[0] != nil && tx.Outputs[0].LockingScript != nil && IsFundingShape(*tx.Outputs[0].LockingScript) {
		if !Mined(ctx, tx, h.Headers) {
			return nil, ErrUnmined
		}
		a.Kind, a.Outputs = TxSweep, []int{0}
		for _, in := range tx.Inputs {
			a.Spent = append(a.Spent, transaction.Outpoint{Txid: *in.SourceTXID, Index: in.SourceTxOutIndex})
		}
		return a, nil
	}

	// Spend: it retains what it spends, and admits nothing.
	if len(retain) > 0 {
		a.Kind = TxSpend
		return a, nil
	}
	return nil, ErrNotBbox
}

// retained checks held against the inputs and returns it sorted, once each.
func retained(tx *transaction.Transaction, held []int) ([]int, error) {
	seen := map[int]bool{}
	var out []int
	for _, i := range held {
		if i < 0 || i >= len(tx.Inputs) {
			return nil, fmt.Errorf("boxrec: previous coin names input %d of %d", i, len(tx.Inputs))
		}
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out, nil
}

// checkCarrier is the carrier's rules 1 to 10. Rules 5 to 8 are bcommon
// carrier.Validate's order: finality, the unlocking script, the lock, the
// signature.
func checkCarrier(raw []byte, b *transaction.Beef, tx *transaction.Transaction, claim Kind, h Host) (*Carrier, error) {
	// 1. carrier-shape.
	if len(tx.Inputs) != 1 || len(tx.Outputs) != 1 {
		return nil, fmt.Errorf("%w: %d inputs, %d outputs", ErrCarrierShape, len(tx.Inputs), len(tx.Outputs))
	}
	in, out := tx.Inputs[0], tx.Outputs[0]
	src := in.SourceTxOutput()
	if src == nil || src.LockingScript == nil || in.SourceTXID == nil {
		// The value cannot be compared without the funding tree, whose
		// absence is the beef rule's.
		return nil, fmt.Errorf("%w: the spent output is not in the BEEF", ErrBEEF)
	}
	if src.Satoshis != out.Satoshis {
		return nil, fmt.Errorf("%w: output %d of input %d", ErrCarrierShape, out.Satoshis, src.Satoshis)
	}
	// 2. beef.
	if err := checkCarrierBEEF(raw, b, tx); err != nil {
		return nil, err
	}
	// 3. the record's own rules.
	record, _ := firstPush(*out.LockingScript, 35)
	c := &Carrier{Record: record, Commitment: carrier.Commitment(tx),
		Funding: transaction.Outpoint{Txid: *in.SourceTXID, Index: in.SourceTxOutIndex}}
	var office string
	if claim == KindEnvelope {
		e, err := DecodeEnvelope(record)
		if err != nil {
			return nil, err
		}
		c.Envelope, c.Owner, office = e, e.From, e.Office
	} else {
		r, err := DecodeReceipt(record)
		if err != nil {
			return nil, err
		}
		c.Receipt, c.Owner, office = r, r.By, r.Office
	}
	// 4. office.
	if office != h.Office {
		return nil, fmt.Errorf("%w: not this topic's office", ErrOffice)
	}
	// 5. mineable.
	if tx.LockTime < carrier.LockTime || in.SequenceNumber == transaction.MaxTxInSequenceNum {
		return nil, ErrMineable
	}
	// 6. unlock, bcommon's check.
	if err := carrier.CheckUnlocking(tx); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnlock, err)
	}
	// 7. lock.
	key, err := EnvelopeDerivation.ExpectedLockingKey(ownerKey(c.Owner))
	if err != nil {
		return nil, err
	}
	fields, _ := pushFields(*out.LockingScript)
	if len(fields) != 2 {
		return nil, fmt.Errorf("%w: %d pushes, want the record and a signature", ErrLock, len(fields))
	}
	sig := fields[1]
	if !bytes.Equal(*out.LockingScript, PushDropScript(key, [][]byte{record}, sig)) {
		return nil, ErrLock
	}
	// 8. signature.
	if !verifyField(key, record, sig) {
		return nil, ErrSignature
	}
	// 9. funding.
	if !bytes.Equal(*src.LockingScript, FundingScript(key)) {
		return nil, ErrFunding
	}
	// 10. the content rules.
	if c.Envelope != nil {
		if c.Content, err = CheckContent(c.Envelope); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// checkCarrierBEEF is rule 2: the BEEF declares exactly one BUMP and two
// transactions, which are the carrier (unproven) and the transaction its
// input spends (proven by that BUMP, minimally).
func checkCarrierBEEF(raw []byte, b *transaction.Beef, tx *transaction.Transaction) error {
	bumps, txs, ok := beefCounts(raw)
	if !ok || bumps != 1 || txs != 2 || len(b.BUMPs) != 1 || len(b.Transactions) != 2 {
		return fmt.Errorf("%w: %d BUMPs and %d transactions", ErrBEEF, bumps, txs)
	}
	txid, parent := *tx.TxID(), *tx.Inputs[0].SourceTXID
	self, fund := b.Transactions[txid], b.Transactions[parent]
	if self == nil || fund == nil || self.Transaction == nil || fund.Transaction == nil {
		return fmt.Errorf("%w: not the carrier and its funding tree", ErrBEEF)
	}
	if self.DataFormat != transaction.RawTx || fund.DataFormat != transaction.RawTxAndBumpIndex || fund.BumpIndex != 0 {
		return fmt.Errorf("%w: the proof is not the funding tree's alone", ErrBEEF)
	}
	if !minimalPath(b.BUMPs[0], parent) {
		return fmt.Errorf("%w: the proof holds more than the funding tree's path", ErrBEEF)
	}
	return nil
}

func ownerKey(k []byte) *ec.PublicKey {
	pub, err := guard.ParsePubKey(k)
	if err != nil {
		// The record's identity rule already held it to a canonical key.
		panic(err)
	}
	return pub
}

// verifyField checks a field signature: strict low-S DER, over
// SHA-256(signed), under key.
func verifyField(key *ec.PublicKey, signed, der []byte) bool {
	if !StrictSignature(der) {
		return false
	}
	s, err := ec.ParseDERSignature(der)
	if err != nil {
		return false
	}
	d := sha256.Sum256(signed)
	return s.Verify(d[:], key)
}

// Mined reports whether tx carries a proof that verifies against the
// headers. No headers means nothing is mined.
func Mined(ctx context.Context, tx *transaction.Transaction, headers chaintracker.ChainTracker) bool {
	if tx.MerklePath == nil || headers == nil {
		return false
	}
	txid := chainhash.Hash(*tx.TxID())
	ok, err := tx.MerklePath.Verify(ctx, &txid, headers)
	return err == nil && ok
}

// Params are the bcommon carrier parameters of a bbox carrier: the record
// output and funding outputs under [1, "bbox message"] key id envelope, the
// funding tag, and a payload validator that takes an envelope or a receipt
// record. A sender mints with carrier.Mint and sweeps with carrier.Sweep
// under them.
func Params() carrier.Params {
	return carrier.Params{
		Derivation: EnvelopeDerivation,
		FundingTag: TagFunding,
		ValidatePayload: func(p []byte) error {
			switch ClaimOfField(p) {
			case KindEnvelope:
				_, err := DecodeEnvelope(p)
				return err
			case KindReceipt:
				_, err := DecodeReceipt(p)
				return err
			}
			return ErrMagic
		},
		ErrIdentity: ErrIdentity,
	}
}
