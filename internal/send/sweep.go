package send

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"

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
		if err := e.FinishSweep(ctx); err != nil {
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
	fee, err := payer.Take(ctx)
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
	sw := state.Sweep{Tree: txid, Vouts: slices.Clone(vouts), Offices: slices.Clone(offices), Txid: tx.TxID().String(), RawHex: tx.Hex()}
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

// FinishSweep takes the sweep in flight, if any, through settlement, its
// proof and its publication. A sweep a network refuses is dropped from
// flight and reported.
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
				return fmt.Errorf("sweep %s: settle via %s: %w (it is persisted; the next drop sends it again)", sw.Txid, e.Legs.Settler.Name(), err)
			}
			sw.Submitted = true
			if err := e.St.Save(); err != nil {
				return err
			}
		}
		mp, height, err := payer.Await(ctx, "sweep", tx)
		if err != nil {
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
// or nil.
func (e *Engine) sweptBy(txid string, vout uint32) *state.Sweep {
	for i := range e.St.Sweeps {
		if sw := &e.St.Sweeps[i]; sw.Tree == txid && slices.Contains(sw.Vouts, vout) {
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
