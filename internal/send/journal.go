package send

import (
	"context"
	"encoding/hex"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/nodeapi"

	"github.com/lightwebinc/bbox/internal/state"
)

// The pool keeps its coins in a file of its own, and a coin leaves that
// file the moment it is taken: before the transaction that spends it is
// built, and so before the home's state records that transaction. A run
// that stops in between leaves the coin in neither file. The journal closes
// that window from the state's side: while a command that spends is
// running, every save of the state records the pool as it stands
// (state.Taking), so the record of a coin is always made before the coin is
// taken. A command that ends clears it. The next command to start after one
// that did not end reads it (Reconcile).

// Journal makes every save of st record the pool, with the coins carried
// over from a reconciliation that could not settle them.
func Journal(st *state.State, pool *bwallet.Pool, carried []bwallet.Output) {
	st.OnSave(func() { st.Taking = append(pool.Outputs(), carried...) })
}

// Unjournal ends the journal: the command ended, so the pool's file and the
// state agree, and nothing is recorded as being taken.
func Unjournal(st *state.State) {
	st.OnSave(nil)
	st.Taking = nil
}

// Reconcile settles what a run that stopped left in the journal. A coin the
// journal holds that the pool no longer does was taken; when no transaction
// the home records spends it, the run stopped before it recorded one. The
// node is asked: a coin it shows unspent goes back in the pool; one it
// shows spent is spent by a transaction this home did not record, and is
// reported; one it cannot answer for is carried over to the next command.
func Reconcile(ctx context.Context, st *state.State, pool *bwallet.Pool, asset *nodeapi.Asset, note func(string, ...any)) []bwallet.Output {
	if len(st.Taking) == 0 {
		return nil
	}
	held := map[string]bool{}
	for _, o := range pool.Outputs() {
		held[o.Outpoint()] = true
	}
	spent := recordedSpends(st)
	var carried []bwallet.Output
	for _, o := range st.Taking {
		op := o.Outpoint()
		if held[op] || spent[op] {
			continue
		}
		held[op] = true
		if asset == nil {
			carried = append(carried, o)
			continue
		}
		by, err := asset.Spender(ctx, o.TxID, o.Vout)
		switch {
		case err != nil:
			note("coin %s was taken from the pool by a run that stopped before it recorded what spent it, and the node could not say whether it is spent (%v): it is looked at again by the next command", op, err)
			carried = append(carried, o)
		case by != "":
			note("coin %s was taken from the pool by a run that stopped before it recorded what spent it, and it is spent by %s, a transaction this home does not record", op, short(by))
		default:
			if err := pool.Return(o); err != nil {
				note("coin %s could not be put back in the pool: %v", op, err)
				carried = append(carried, o)
				continue
			}
			note("coin %s (%d sat) is back in the pool: a run stopped between taking it and recording what spent it, and the node shows it unspent", op, o.Satoshis)
		}
	}
	return carried
}

// recordedSpends are the outpoints spent by every transaction the home
// records: its funding trees, its sweeps and its payments. A carrier spends
// a funding output, never a coin.
func recordedSpends(st *state.State) map[string]bool {
	out := map[string]bool{}
	add := func(tx *transaction.Transaction) {
		if tx == nil {
			return
		}
		for _, in := range tx.Inputs {
			if in.SourceTXID != nil {
				out[(&bwallet.Output{TxID: in.SourceTXID.String(), Vout: in.SourceTxOutIndex}).Outpoint()] = true
			}
		}
	}
	raw := func(h string) {
		if b, err := hex.DecodeString(h); err == nil && len(b) > 0 {
			if tx, err := guard.ParseTransaction(b, guard.DefaultBound); err == nil {
				add(tx)
			}
		}
	}
	if st.Tree != nil {
		raw(st.Tree.RawHex)
	}
	for i := range st.Trees {
		raw(st.Trees[i].RawHex)
	}
	for i := range st.Ahead {
		raw(st.Ahead[i].RawHex)
	}
	for i := range st.Sweeps {
		raw(st.Sweeps[i].RawHex)
	}
	for i := range st.Payments {
		if tx, err := funding.Rebuild("", "", st.Payments[i].Beef); err == nil {
			add(tx)
		}
	}
	return out
}
