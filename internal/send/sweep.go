package send

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/producer"

	"github.com/lightwebinc/bbox/internal/state"
)

// Retraction (spec section 6.4): a sweep spends funding-tree outputs, used
// or not, with bcommon carrier.Sweep, whose output 0 is a funding-shaped
// tombstone, so a host admits it. Once it mines, every carrier that spent
// one of those outputs is a double spend, and every conforming host stops
// answering it. A sweep is persisted before it is settled, waited for until
// it mines, and then published once to every office it retracts carriers
// in: in mode plane to the facade and directly to every other host named,
// in mode unicast to every host named. Only honest hosts drop what a sweep
// retracts: it is not erasure.

// ErrSweepInFlight is a new sweep asked for while one is still in flight.
var ErrSweepInFlight = errors.New("a sweep is still in flight; the next drop finishes it first")

// Retract builds and persists one sweep of vouts of the funding tree txid,
// with its fee from the pool, then settles and publishes it. offices are
// the offices the carriers it retracts are in.
func (e *Engine) Retract(ctx context.Context, txid string, vouts []uint32, offices []string) (*state.Sweep, error) {
	if e.St.InFlight() != nil {
		var refusedErr *SweepRefusedError
		if err := e.FinishSweep(ctx); errors.As(err, &refusedErr) {
			// It failed for good, which frees this drop to go on.
			e.note("%v", err)
		} else if err != nil {
			return nil, err
		}
	}
	if len(vouts) == 0 || len(offices) == 0 {
		return nil, errors.New("a sweep spends at least one output and is published to at least one office")
	}
	// An output a sweep already spends is not swept again: a drop asked
	// again after one failed part way names the outputs of the sweep
	// finished since (here, or when the engine opened).
	var left []uint32
	for _, v := range vouts {
		if e.sweptBy(txid, v) == nil {
			left = append(left, v)
		}
	}
	if len(left) == 0 {
		return e.sweptBy(txid, vouts[0]), nil
	}
	vouts = left
	tree, err := e.kept.Tx(txid)
	if err != nil {
		return nil, err
	}
	payer := e.NewPayer()
	fee, coin, err := e.sweepFee(ctx, payer)
	if err != nil {
		return nil, FeeError(fmt.Errorf("sweep fee: %w", err))
	}
	change, err := e.Signer.FundScript()
	if err != nil {
		payer.GiveBack()
		return nil, err
	}
	tx, err := carrier.Sweep(ctx, e.Signer, e.Signer.Originator, Params, tree, vouts, fee.Tx, fee.Vout, fee.Unlocker, change,
		e.Opts.Fees.SatPerByte, e.Opts.Fees.Floor)
	if err != nil {
		payer.GiveBack()
		return nil, err
	}
	sw := state.Sweep{Tree: txid, Vouts: slices.Clone(vouts), Offices: slices.Clone(offices), Txid: tx.TxID().String(), RawHex: tx.Hex(), Fee: coin}
	e.St.Sweeps = append(e.St.Sweeps, sw)
	if t := e.St.Tree; t != nil && t.Txid == txid {
		// The rest of the tree stays usable only past every output swept.
		if last := slices.Max(vouts); last >= t.Next {
			t.Next = last + 1
		}
	}
	if err := e.St.Save(); err != nil {
		payer.GiveBack()
		return nil, err
	}
	if err := e.FinishSweep(ctx); err != nil {
		return &e.St.Sweeps[len(e.St.Sweeps)-1], err
	}
	return &e.St.Sweeps[len(e.St.Sweeps)-1], nil
}

// sweepFee takes the sweep's fee coin, and the coin as the pool held it,
// so a refused sweep can give it back. When every coin left is change from
// a transaction not yet mined (a tree minted ahead took the last proven
// coin, say), it waits up to Opts.Wait for that change to mine rather than
// fail: a drop waits for a block anyway.
func (e *Engine) sweepFee(ctx context.Context, payer *producer.Payer) (mint.Input, *bwallet.Output, error) {
	var deadline time.Time
	for {
		before := e.Pool.Outputs()
		fee, err := payer.Take(ctx)
		var nc *producer.NoCoinError
		if errors.As(err, &nc) && nc.Held > 0 {
			if deadline.IsZero() {
				deadline = time.Now().Add(e.Opts.Wait)
				e.note("sweep fee: every coin is change from %d transaction(s) not yet mined; waiting for one", nc.Held)
			}
			if time.Now().After(deadline) {
				return mint.Input{}, nil, err
			}
			poll := e.Opts.Poll
			if poll <= 0 {
				poll = producer.DefaultPoll
			}
			select {
			case <-ctx.Done():
				return mint.Input{}, nil, ctx.Err()
			case <-time.After(poll):
			}
			CollectChange(ctx, e.Pool, e.Legs.Asset)
			continue
		}
		if err != nil {
			return mint.Input{}, nil, err
		}
		op := fmt.Sprintf("%s.%d", fee.Tx.TxID(), fee.Vout)
		for i := range before {
			if before[i].Outpoint() == op {
				return fee, &before[i], nil
			}
		}
		return fee, nil, nil
	}
}

// FinishSweep takes the sweep in flight, if any, through settlement, its
// proof and its publication. A sweep the network definitively refuses is
// marked failed, its fee coin given back when the node shows it unspent,
// and a *SweepRefusedError returned: it is no longer in flight, so a later
// drop goes on. Any other failure leaves it in flight, to be tried again.
func (e *Engine) FinishSweep(ctx context.Context) error {
	sw := e.St.InFlight()
	if sw == nil {
		return nil
	}
	tx, err := funding.Rebuild(sw.RawHex, sw.BumpHex, "")
	if err != nil {
		return fmt.Errorf("sweep %s: %w", sw.Txid, err)
	}
	if sw.BumpHex == "" {
		payer := e.NewPayer()
		if !sw.Submitted {
			if err := e.sources(ctx, tx); err != nil {
				return fmt.Errorf("sweep %s: %w (it is persisted; the next drop sends it again)", sw.Txid, err)
			}
			if err := e.Legs.Settler.Submit(ctx, tx); err != nil && !alreadyKnown(err) {
				if why := e.refusal(ctx, tx, err); why != "" {
					return e.failSweep(ctx, sw, why)
				}
				return fmt.Errorf("sweep %s: settle via %s: %w (it is persisted; the next drop sends it again)", sw.Txid, e.Legs.Settler.Name(), err)
			}
			sw.Submitted = true
			if err := e.St.Save(); err != nil {
				return err
			}
		}
		// A leg with no answer (the tcp ingress) or a verdict that came
		// later: the node's view of the inputs says before any wait.
		if why := e.refusal(ctx, tx, nil); why != "" {
			return e.failSweep(ctx, sw, why)
		}
		mp, height, err := payer.Await(ctx, "sweep", tx)
		if err != nil {
			if why := e.refusal(ctx, tx, nil); why != "" {
				return e.failSweep(ctx, sw, why)
			}
			return fmt.Errorf("sweep %s: %w (the next drop waits again)", sw.Txid, err)
		}
		sw.BumpHex, sw.Height = mp.Hex(), height
		payer.Change(tx, height, mp)
		if err := e.St.Save(); err != nil {
			return err
		}
		tx.MerklePath = mp
	}
	beef, err := tx.AtomicBEEF(false)
	if err != nil {
		return err
	}
	p := state.Pending{Kind: state.KindSweep, Offices: sw.Offices, Txid: sw.Txid, Beef: hex.EncodeToString(beef),
		Outpoint: fmt.Sprintf("%s.%d", sw.Tree, sw.Vouts[0])}
	sw.Done = true
	for i := range e.St.Sent {
		s := &e.St.Sent[i]
		if s.Tree == sw.Tree && slices.Contains(sw.Vouts, s.Vout) {
			s.Swept = sw.Txid
		}
	}
	e.St.Outbox = append(e.St.Outbox, p)
	if err := e.St.Save(); err != nil {
		return err
	}
	if err := e.publish(ctx, p); err != nil {
		return err
	}
	e.St.Done(p.Txid)
	return e.St.Save()
}

// sweptBy is the sweep that spends output vout of the funding tree txid,
// or nil. A failed sweep spends nothing.
func (e *Engine) sweptBy(txid string, vout uint32) *state.Sweep {
	for i := range e.St.Sweeps {
		if sw := &e.St.Sweeps[i]; sw.Failed == "" && sw.Tree == txid && slices.Contains(sw.Vouts, vout) {
			return sw
		}
	}
	return nil
}

// sources gives every input of a sweep rebuilt from its raw bytes the
// transaction it spends: a settlement leg takes Extended Format (BRC-30),
// which carries each input's previous output. The funding tree is the
// home's own; the fee coin's parent is read from the node.
func (e *Engine) sources(ctx context.Context, tx *transaction.Transaction) error {
	for i, in := range tx.Inputs {
		if in.SourceTransaction != nil {
			continue
		}
		id := in.SourceTXID.String()
		src, err := e.kept.Tx(id)
		if err != nil {
			if e.Legs.Asset == nil {
				return fmt.Errorf("input %d: %s is not kept and no node is configured to read it from", i, id)
			}
			raw, ferr := e.Legs.Asset.TxRaw(ctx, id)
			if ferr != nil {
				return fmt.Errorf("input %d: reading %s from the node: %w", i, id, ferr)
			}
			if src, err = guard.ParseTransaction(raw, guard.DefaultBound); err != nil {
				return fmt.Errorf("input %d: %s from the node: %w", i, id, err)
			}
		}
		if src.TxID().String() != id || int(in.SourceTxOutIndex) >= len(src.Outputs) {
			return fmt.Errorf("input %d: the transaction found for %s is not the one it spends", i, id)
		}
		in.SourceTransaction = src
	}
	return nil
}

// TreeOf finds a funding tree the home recorded.
func (e *Engine) TreeOf(txid string) (*funding.Tree, bool) {
	if t := e.St.Tree; t != nil && t.Txid == txid {
		return t, true
	}
	for i := range e.St.Trees {
		if e.St.Trees[i].Txid == txid {
			return &e.St.Trees[i], true
		}
	}
	for i := range e.St.Ahead {
		if e.St.Ahead[i].Txid == txid {
			return &e.St.Ahead[i], true
		}
	}
	return nil, false
}

// Tx is a funding tree the home keeps, rebuilt.
func (e *Engine) Tx(txid string) (*transaction.Transaction, error) { return e.kept.Tx(txid) }
