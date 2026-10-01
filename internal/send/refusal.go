package send

import (
	"context"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/termsafe"

	"github.com/lightwebinc/bbox/internal/chainview"
	"github.com/lightwebinc/bbox/internal/state"
)

// A sweep the network will not mine is not in flight any more: waiting for
// it, or sending the same bytes again, can never finish it, and while it
// stayed in flight no later drop could run. Only a definitive answer marks
// it failed; anything else (a leg that cannot be reached, an answer that
// says busy, no verdict yet) leaves it in flight to be tried again.

// SweepRefusedError is a sweep the network refused. The sweep is marked
// failed in the home, its fee coin is back in the pool when the node shows
// it unspent, and the outputs it named may be swept again.
type SweepRefusedError struct {
	Txid string
	// Why is the refusal, as the evidence said it.
	Why string
	// Coin is what became of the fee coin.
	Coin string
}

func (e *SweepRefusedError) Error() string {
	return fmt.Sprintf("sweep %s refused by the network: %s; it is marked failed and retracts nothing; %s; drop again to build a new sweep", e.Txid, termsafe.Text(e.Why), termsafe.Text(e.Coin))
}

// refusal is why the network will never mine sweep tx, or "" while it
// still might. err is the settlement leg's answer, if any. A sweep that has
// mined is never refused. Then, in order: the leg's answer; arcade's
// verdict, when arcade is the leg; and the node's view of each input, one
// of which spent by another transaction means this one cannot mine.
func (e *Engine) refusal(ctx context.Context, tx *transaction.Transaction, err error) string {
	txid := tx.TxID().String()
	if _, _, perr := e.Legs.Asset.Proof(ctx, txid); perr == nil {
		return ""
	}
	if why, ok := chainview.RefusedAnswer(err); ok {
		return why
	}
	if a := e.Legs.Arcade; a != nil {
		if st, serr := a.Status(ctx, txid); serr == nil && st.Refused() {
			return "arcade reports " + st.Why()
		}
	}
	return chainview.SpentElsewhere(ctx, e.Legs.Asset, tx)
}

// failSweep marks sw failed for why, and gives its fee coin back to the
// pool if the node shows that coin unspent.
func (e *Engine) failSweep(ctx context.Context, sw *state.Sweep, why string) error {
	sw.Failed = why
	coin := "its fee coin was not recorded (a sweep built by an earlier version); `bbox doctor` shows the pool"
	if f := sw.Fee; f != nil {
		by, err := chainview.Spender(ctx, e.Legs.Asset, f.TxID, f.Vout)
		switch {
		case err != nil:
			coin = fmt.Sprintf("its fee coin %s was not given back: the node could not say whether it is spent (%v)", f.Outpoint(), err)
		case by != "":
			coin = fmt.Sprintf("its fee coin %s is spent by %s and was not given back", f.Outpoint(), by)
		default:
			if err := e.Pool.Return(*f); err != nil {
				coin = fmt.Sprintf("its fee coin %s could not be given back: %v", f.Outpoint(), err)
			} else {
				coin = fmt.Sprintf("its fee coin %s (%d sat) is back in the pool", f.Outpoint(), f.Satoshis)
			}
		}
	}
	if err := e.St.Save(); err != nil {
		return err
	}
	return &SweepRefusedError{Txid: sw.Txid, Why: why, Coin: coin}
}
