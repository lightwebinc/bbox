// Package purse is a BRC-100 wallet over a bbox home: bcommon's embedded
// wallet (the identity key and its coin pool) with the two actions the
// embedded wallet does not implement and bbox needs.
//
//   - CreateAction pays one output from the pool, unbroadcast, and returns
//     the payment as Atomic BEEF: what the SDK's AuthFetch asks a wallet for
//     when a host answers a priced question with 402 (BRC-105). It pays at
//     most MaxPay, and nothing else: any other shape is refused.
//   - InternalizeAction takes a BRC-29 payment to this identity ("wallet
//     payment" outputs, BRC-100): it checks each output pays the key this
//     identity derives for the remittance and the sender, verifies the
//     transaction against the headers, broadcasts it through the settlement
//     leg, waits for its proof, and adds the outputs to the pool with their
//     derivation, so the pool can spend them. What a recipient does with a
//     payment inside an envelope, and what an operator does with a payment
//     a host accepted for a priced question.
//
// A remote BRC-100 wallet does both itself; a home that uses one needs
// neither.
package purse

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/termsafe"

	"github.com/lightwebinc/bbox/internal/chainview"
)

// Purse is the home's wallet. The embedded wallet answers the key and
// signature methods; the fields below serve the two actions.
type Purse struct {
	*bwallet.Embedded
	// NewPayer is a Payer over the home's pool, for CreateAction's fee
	// coin and change.
	NewPayer func() *producer.Payer
	// Fees is the payment's fee policy.
	Fees mint.Fees
	// MaxPay is the most one CreateAction pays; zero pays nothing.
	MaxPay uint64
	// Settler and Asset broadcast an internalized payment and wait for its
	// proof; Headers verify it first. Wait bounds the wait, Poll paces it.
	Settler publish.Settler
	Asset   *nodeapi.Asset
	Headers chaintracker.ChainTracker
	Wait    time.Duration
	Poll    time.Duration

	made []made
}

var _ wallet.Interface = (*Purse)(nil)

type made struct {
	payer *producer.Payer
	tx    *transaction.Transaction
}

// ErrRefusedAction is an action this wallet does not make.
var ErrRefusedAction = errors.New("purse: this wallet makes no such action")

// CreateAction pays exactly one output of at most MaxPay satoshis to its
// locking script from the pool, with change back to the fund key, signs it
// and returns it as Atomic BEEF without broadcasting it. The payee
// broadcasts it (BRC-105). Settle or Refund must follow.
func (p *Purse) CreateAction(ctx context.Context, args wallet.CreateActionArgs, _ string) (*wallet.CreateActionResult, error) {
	if len(args.Inputs) != 0 || len(args.Outputs) != 1 || len(args.InputBEEF) != 0 {
		return nil, fmt.Errorf("%w: only one payment output, funded by this wallet", ErrRefusedAction)
	}
	o := args.Outputs[0]
	switch {
	case o.Satoshis == 0:
		return nil, fmt.Errorf("%w: a payment of nothing", ErrRefusedAction)
	case o.Satoshis > p.MaxPay:
		return nil, fmt.Errorf("purse: the host asks %d satoshis, more than the %d this command may pay (-max-sats)", o.Satoshis, p.MaxPay)
	case len(o.LockingScript) == 0:
		return nil, fmt.Errorf("%w: no locking script", ErrRefusedAction)
	}
	dest := script.Script(o.LockingScript)
	if !dest.IsP2PKH() {
		return nil, fmt.Errorf("%w: the payment output is not P2PKH", ErrRefusedAction)
	}
	payer := p.NewPayer()
	fee, err := payer.TakeAtLeast(ctx, o.Satoshis+p.Fees.Floor)
	if err != nil {
		return nil, err
	}
	change, err := p.Signer().FundScript()
	if err != nil {
		payer.GiveBack()
		return nil, err
	}
	tx, err := mint.Payment(ctx, &dest, o.Satoshis, fee, change, p.Fees)
	if err != nil {
		payer.GiveBack()
		return nil, err
	}
	beef, err := funding.BEEF(tx)
	if err != nil {
		payer.GiveBack()
		return nil, err
	}
	p.made = append(p.made, made{payer, tx})
	return &wallet.CreateActionResult{Txid: *tx.TxID(), Tx: beef}, nil
}

// Settle keeps the last payment CreateAction made, which the payee
// accepted: its fee coin is spent and its change, unproven until the payee
// broadcasts it, is held in the pool. Every earlier payment is refunded.
// It returns the kept payment, or nil when none was made.
func (p *Purse) Settle() *transaction.Transaction {
	if len(p.made) == 0 {
		return nil
	}
	last := p.made[len(p.made)-1]
	for _, m := range p.made[:len(p.made)-1] {
		m.payer.GiveBack()
	}
	p.made = nil
	last.payer.Change(last.tx, 0, nil)
	return last.tx
}

// Refund returns the fee coin of every payment CreateAction made to the
// pool: none was accepted, so none can be broadcast by the payee.
func (p *Purse) Refund() {
	for _, m := range p.made {
		m.payer.GiveBack()
	}
	p.made = nil
}

// InternalizeAction takes the "wallet payment" outputs of args.Tx, an
// Atomic BEEF, into the pool. Each output must pay the key this identity
// derives under BRC-29 for its remittance's prefix, suffix and sender; the
// transaction must verify against Headers. It is broadcast through the
// settlement leg (a transaction the network already holds is not an
// error) and its proof awaited, then the outputs are pooled with their
// derivation. A basket insertion is refused: this wallet has one basket.
//
// It is Check, Broadcast, Await and Take in turn; a caller settling many
// payments runs those halves itself, so it can broadcast them all before
// it waits.
func (p *Purse) InternalizeAction(ctx context.Context, args wallet.InternalizeActionArgs, _ string) (*wallet.InternalizeActionResult, error) {
	in, err := p.Check(ctx, args)
	if err != nil {
		return nil, err
	}
	if err := p.Broadcast(ctx, in); err != nil {
		return nil, err
	}
	if err := p.Await(ctx, in); err != nil {
		return nil, err
	}
	if err := p.Take(in); err != nil {
		return nil, err
	}
	return &wallet.InternalizeActionResult{Accepted: true}, nil
}

// Incoming is a payment Check found paying this identity and verifying
// against the headers, on its way into the pool.
type Incoming struct {
	Tx   *transaction.Transaction
	Txid string
	outs []bwallet.Output
	mp   *transaction.MerklePath
	h    uint32
}

// Check is InternalizeAction's checks: the outputs pay this identity's
// derived keys, and the transaction verifies against Headers.
func (p *Purse) Check(ctx context.Context, args wallet.InternalizeActionArgs) (*Incoming, error) {
	if len(args.Outputs) == 0 {
		return nil, fmt.Errorf("%w: nothing to internalize", ErrRefusedAction)
	}
	_, tx, txid, err := guard.ParseBEEF(args.Tx, guard.DefaultBound)
	if err != nil || tx == nil {
		return nil, fmt.Errorf("purse: the payment is not a BEEF with its subject: %v", err)
	}
	me := p.Signer().IdentityHex()
	outs := make([]bwallet.Output, 0, len(args.Outputs))
	for _, o := range args.Outputs {
		if o.Protocol != wallet.InternalizeProtocolWalletPayment || o.PaymentRemittance == nil {
			return nil, fmt.Errorf("%w: output %d is not a wallet payment", ErrRefusedAction, o.OutputIndex)
		}
		if uint64(o.OutputIndex) >= uint64(len(tx.Outputs)) {
			return nil, fmt.Errorf("purse: output %d of %d", o.OutputIndex, len(tx.Outputs))
		}
		r := o.PaymentRemittance
		if r.SenderIdentityKey == nil {
			return nil, errors.New("purse: the remittance names no sender")
		}
		d := &bwallet.Derivation{
			SecurityLevel: int(bwallet.PaymentProtocol.SecurityLevel), Protocol: bwallet.PaymentProtocol.Protocol,
			KeyID:           bwallet.PaymentKeyID(base64.StdEncoding.EncodeToString(r.DerivationPrefix), base64.StdEncoding.EncodeToString(r.DerivationSuffix)),
			CounterpartyHex: hex.EncodeToString(r.SenderIdentityKey.Compressed()), OwnerHex: me,
		}
		want, err := p.Signer().DerivedScript(ctx, d)
		if err != nil {
			return nil, err
		}
		out := tx.Outputs[o.OutputIndex]
		if out.LockingScript == nil || !bytes.Equal(*out.LockingScript, *want) {
			return nil, fmt.Errorf("purse: output %d does not pay the key this identity derives for the remittance and the sender", o.OutputIndex)
		}
		outs = append(outs, bwallet.Output{TxID: txid.String(), Vout: o.OutputIndex, Satoshis: out.Satoshis,
			LockingScript: out.LockingScript.String(), Derivation: d})
	}
	if p.Headers == nil {
		return nil, errors.New("purse: no header source to verify the payment against")
	}
	if ok, err := spv.Verify(ctx, tx, p.Headers, nil); err != nil || !ok {
		return nil, fmt.Errorf("purse: the payment does not verify against the headers: %v", err)
	}
	return &Incoming{Tx: tx, Txid: txid.String(), outs: outs}, nil
}

// Broadcast hands a checked payment to the settlement leg. One already
// mined is taken as it is; one the network already holds is not an error.
func (p *Purse) Broadcast(ctx context.Context, in *Incoming) error {
	if p.Asset == nil {
		return errors.New("purse: no node to wait for the payment's proof (config key asset)")
	}
	if mp, h, err := p.Asset.Proof(ctx, in.Txid); err == nil {
		in.mp, in.h = mp, h
		return nil
	}
	if p.Settler == nil {
		return errors.New("purse: no settlement leg to broadcast the payment (config key settle)")
	}
	if err := p.Settler.Submit(ctx, in.Tx); err != nil && !alreadyKnown(err) {
		if why, ok := chainview.RefusedAnswer(err); ok {
			return &RefusedError{Txid: in.Txid, Why: why}
		}
		if why := chainview.SpentElsewhere(ctx, p.Asset, in.Tx); why != "" {
			return &RefusedError{Txid: in.Txid, Why: why}
		}
		return fmt.Errorf("purse: broadcasting payment %s: %w (the payer may have spent its inputs elsewhere)", in.Txid, err)
	}
	// A leg's acceptance is not the network's: arcade has answered
	// ACCEPTED_BY_NETWORK for a payment whose input was already spent and
	// mined. The node's view of the inputs says before any wait.
	if why := chainview.SpentElsewhere(ctx, p.Asset, in.Tx); why != "" {
		return &RefusedError{Txid: in.Txid, Why: why}
	}
	return nil
}

// RefusedError is a payment the network will never mine: the leg refused
// it for good, or one of its inputs is spent by another transaction. The
// payer took the coins back; there is nothing to settle.
type RefusedError struct {
	Txid string
	Why  string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("purse: payment %s is refused by the network and will never mine: %s (the payer spent its inputs elsewhere)", e.Txid, termsafe.Text(e.Why))
}

// Await waits, up to Wait, for a broadcast payment to mine, and stops at
// once when the node shows one of its inputs spent by another transaction.
func (p *Purse) Await(ctx context.Context, in *Incoming) error {
	if in.mp != nil {
		return nil
	}
	wait, poll := p.Wait, p.Poll
	if wait <= 0 {
		wait = producer.DefaultTimeout
	}
	if poll <= 0 {
		poll = producer.DefaultPoll
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	mp, h, err := nodeapi.WaitSettled(wctx, p.Asset, in.Tx, poll)
	if err != nil {
		if why := chainview.SpentElsewhere(ctx, p.Asset, in.Tx); why != "" {
			return &RefusedError{Txid: in.Txid, Why: why}
		}
		return fmt.Errorf("purse: payment %s is broadcast and not yet mined (%v); run the command again to take it into the pool once it is", in.Txid, err)
	}
	in.mp, in.h = mp, h
	return nil
}

// Take adds a mined payment's outputs to the pool with their derivation.
func (p *Purse) Take(in *Incoming) error {
	if in.mp == nil {
		return fmt.Errorf("purse: payment %s has not mined", in.Txid)
	}
	outs := slices.Clone(in.outs)
	for i := range outs {
		outs[i].Height, outs[i].Raw, outs[i].Bump = in.h, in.Tx.Hex(), in.mp.Hex()
	}
	_, err := p.Pool.Add(outs...)
	return err
}

// alreadyKnown reports a node's answer for a transaction it already holds.
func alreadyKnown(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already") || strings.Contains(s, "txn-already-known") || strings.Contains(s, "transaction already in block chain")
}

// Remittance is the BRC-100 remittance of a payment, from its base64 prefix
// and suffix and the sender's key, as spec section 4.5 and the host's
// payment ledger write them.
func Remittance(prefix, suffix, senderHex string) (*wallet.Payment, error) {
	pb, err := base64.StdEncoding.Strict().DecodeString(prefix)
	if err != nil || len(pb) == 0 {
		return nil, fmt.Errorf("purse: derivation prefix %q is not base64", prefix)
	}
	sb, err := base64.StdEncoding.Strict().DecodeString(suffix)
	if err != nil || len(sb) == 0 {
		return nil, fmt.Errorf("purse: derivation suffix %q is not base64", suffix)
	}
	sender, err := guard.ParsePubKeyHex(senderHex)
	if err != nil {
		return nil, fmt.Errorf("purse: sender %q: %w", senderHex, err)
	}
	return &wallet.Payment{DerivationPrefix: pb, DerivationSuffix: sb, SenderIdentityKey: sender}, nil
}

// Txid is a hash as the display txid.
func Txid(h chainhash.Hash) string { return h.String() }
