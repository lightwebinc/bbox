package send

import (
	"context"
	"math"
	"slices"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/producer"

	"github.com/lightwebinc/bbox/internal/state"
)

// A funding tree's fee coin leaves the pool when the tree is signed, and
// the tree is adopted into the state only once the settlement leg took it;
// a tree minted ahead is recorded later still, when the command ends. A
// run that stops in between leaves a tree that may be on the chain and a
// pool without the coin. The record closes that window: the library hands
// the tree and its coin over before the tree reaches the leg
// (producer.Trees.Prepare), the home saves both (state.PendingTrees), and
// the next command that spends asks the node what became of each
// (RecoverTrees). Adopting a tree drops its record in the same save.

// prepareTree records a tree signed for coin before it reaches the
// settlement leg. A record that cannot be saved aborts the mint.
func (e *Engine) prepareTree(tree funding.Tree, coin bwallet.Output) error {
	e.St.KeepPendingTree(state.PendingTree{Tree: tree, Coin: coin})
	if err := e.St.Save(); err != nil {
		e.St.DropPendingTree(tree.Txid)
		return err
	}
	return nil
}

// RecoverTrees settles every tree a run signed for a coin and did not
// adopt, before anything is spent:
//
//   - a tree the chain holds is adopted, or held as the tree minted ahead
//     while the current tree has outputs left, and its change is taken. A
//     held tree is recorded among the trees minted ahead, and its record
//     stays until the switch adopts it;
//   - a tree the node does not know whose coin another transaction spent is
//     gone, and its record is dropped;
//   - a tree the node does not know whose coin is unspent never reached the
//     chain as far as the node can say now. The coin goes back to the pool,
//     and the record is kept for one more command: a tree handed to the leg
//     an instant before the run stopped can reach the node later. The
//     second such answer drops the record;
//   - a node that cannot answer decides nothing: the record stays, the next
//     command asks again, and this one goes on.
//
// A coin the pool holds although a tree on the chain spends it (it was put
// back before the tree landed) is taken out of the pool. Only a failure to
// save the state is returned.
func (e *Engine) RecoverTrees(ctx context.Context) error {
	for _, p := range slices.Clone(e.St.PendingTrees) {
		id := p.Tree.Txid
		// A tree the home records has nothing left to settle. Recorded as
		// minted ahead, with its change taken, it keeps its record until
		// the switch adopts it; adopted, or with every output swept, its
		// record is done with.
		ahead := e.St.MintedAhead(id)
		if e.adopted(id) || ahead != nil && ahead.Remaining() == 0 {
			e.St.DropPendingTree(id)
			if err := e.St.Save(); err != nil {
				return err
			}
			continue
		}
		if ahead != nil {
			continue
		}
		r, err := e.trees.Recover(ctx, p.Tree, p.Coin)
		if err != nil {
			e.note("funding tree %s was signed by a run that stopped before recording it, and what became of it could not be settled now (%v): its record is kept and the next command asks again", id, err)
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		switch r.Outcome {
		case producer.TreeAdopted:
			e.St.DropPendingTree(id)
			e.unpool(p.Coin)
		case producer.TreeHeld:
			e.St.Ahead = producer.Index(e.St.Ahead, &r.Tree)
			e.unpool(p.Coin)
		case producer.CoinSpent:
			e.St.DropPendingTree(id)
			e.unpool(p.Coin)
		case producer.CoinReturned:
			if p.ReturnedOnce {
				e.St.DropPendingTree(id)
				e.note("funding tree %s: the node still does not know it, so its record is dropped", id)
			} else if rec := e.St.PendingTreeOf(id); rec != nil {
				rec.ReturnedOnce = true
				e.note("funding tree %s: its record is kept for one more command, in case it reaches the node after all", id)
			}
		}
		if err := e.St.Save(); err != nil {
			return err
		}
	}
	return nil
}

// adopted reports whether the home records txid as a tree it adopted.
func (e *Engine) adopted(txid string) bool {
	st := e.St
	return st.Tree != nil && st.Tree.Txid == txid ||
		slices.ContainsFunc(st.Trees, func(t funding.Tree) bool { return t.Txid == txid })
}

// unpool takes coin out of the pool when the pool holds it: the chain
// shows it spent. The pool has no removal, so coins are taken until this
// one comes out, and the others are put back.
func (e *Engine) unpool(coin bwallet.Output) {
	if RemoveCoin(e.Pool, coin) {
		e.note("coin %s is spent on the chain and is taken out of the pool", coin.Outpoint())
	}
}

// RemoveCoin takes coin out of pool, and reports whether the pool held it.
func RemoveCoin(pool *bwallet.Pool, coin bwallet.Output) bool {
	op := coin.Outpoint()
	if !slices.ContainsFunc(pool.Outputs(), func(o bwallet.Output) bool { return o.Outpoint() == op }) {
		return false
	}
	var keep []bwallet.Output
	removed := false
	for {
		o, err := pool.TakeAtLeast(math.MaxUint32, 1, []string{coin.TxID})
		if err != nil {
			break
		}
		if o.Outpoint() == op {
			removed = true
			break
		}
		keep = append(keep, o)
	}
	for _, o := range keep {
		_ = pool.Return(o)
	}
	return removed
}
