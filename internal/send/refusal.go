package send

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/termsafe"

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

var (
	arcadeCode = regexp.MustCompile(`arcade answered (\d{3})`)
	rpcCode    = regexp.MustCompile(`rpc error (-?\d+)`)
)

// RefusedAnswer reports whether a settlement leg's error is the network's
// definitive refusal of these bytes, and in what words:
//
//   - arcade's verdict REJECTED or DOUBLE_SPEND_ATTEMPTED (bcommon words it
//     "arcade refused" or "the network refused");
//   - arcade's HTTP 422, or 460 to 475, its documented validation refusals
//     (malformed, inputs, fee, a conflicting transaction);
//   - a node's RPC error -25 (RPC_VERIFY_ERROR) or -26
//     (RPC_VERIFY_REJECTED), except a full mempool or a chain too long,
//     which pass.
//
// Everything else is transient: a leg that cannot be reached, a 5xx, a
// 429, no verdict yet.
func RefusedAnswer(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	msg := err.Error()
	low := strings.ToLower(msg)
	if strings.Contains(low, "arcade refused") || strings.Contains(low, "the network refused") {
		return msg, true
	}
	if m := arcadeCode.FindStringSubmatch(msg); m != nil {
		c, _ := strconv.Atoi(m[1])
		if c == http.StatusUnprocessableEntity || (c >= 460 && c <= 475) {
			return msg, true
		}
		return "", false
	}
	if m := rpcCode.FindStringSubmatch(msg); m != nil {
		if m[1] != "-25" && m[1] != "-26" {
			return "", false
		}
		if strings.Contains(low, "mempool full") || strings.Contains(low, "too-long-mempool-chain") {
			return "", false
		}
		return msg, true
	}
	return "", false
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
	if why, ok := RefusedAnswer(err); ok {
		return why
	}
	if a := e.Legs.Arcade; a != nil {
		if st, serr := a.Status(ctx, txid); serr == nil && st.Refused() {
			return "arcade reports " + st.Why()
		}
	}
	for i, in := range tx.Inputs {
		src := in.SourceTXID.String()
		by, serr := e.spender(ctx, src, in.SourceTxOutIndex)
		if serr == nil && by != "" && by != txid {
			return fmt.Sprintf("input %d (%s.%d) is spent by %s", i, src, in.SourceTxOutIndex, by)
		}
	}
	return ""
}

// failSweep marks sw failed for why, and gives its fee coin back to the
// pool if the node shows that coin unspent.
func (e *Engine) failSweep(ctx context.Context, sw *state.Sweep, why string) error {
	sw.Failed = why
	coin := "its fee coin was not recorded (a sweep built by an earlier version); `bbox doctor` shows the pool"
	if f := sw.Fee; f != nil {
		by, err := e.spender(ctx, f.TxID, f.Vout)
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

// errUnknownTx is a transaction the node does not know.
var errUnknownTx = errors.New("the node does not know the transaction")

// spender reads the node's UTXO view of txid (/api/v1/utxos/<txid>/json)
// and names the transaction that spent output vout, or "" while it is
// unspent.
func (e *Engine) spender(ctx context.Context, txid string, vout uint32) (string, error) {
	a := e.Legs.Asset
	if a == nil {
		return "", errors.New("no node is configured")
	}
	c := a.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.Base, "/")+"/api/v1/utxos/"+txid+"/json", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", errUnknownTx
	default:
		return "", fmt.Errorf("the node's UTXO view of %s: status %d", txid, resp.StatusCode)
	}
	var outs []struct {
		Vout     uint32 `json:"vout"`
		Status   string `json:"status"`
		Spending *struct {
			TxID string `json:"txId"`
		} `json:"spendingData"`
	}
	if err := json.Unmarshal(body, &outs); err != nil {
		return "", fmt.Errorf("the node's UTXO view of %s: %w", txid, err)
	}
	for _, o := range outs {
		if o.Vout != vout {
			continue
		}
		if strings.EqualFold(o.Status, "SPENT") {
			if o.Spending == nil || o.Spending.TxID == "" {
				return "", fmt.Errorf("the node's UTXO view of %s says output %d is spent and names no spender", txid, vout)
			}
			return o.Spending.TxID, nil
		}
		return "", nil
	}
	return "", fmt.Errorf("the node's UTXO view of %s has no output %d", txid, vout)
}
