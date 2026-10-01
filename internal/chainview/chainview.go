// Package chainview answers, from the network's own words and the node's
// view of outputs, whether a transaction can still mine: what a settlement
// leg's error means, and which transaction spent an output. A sender's
// sweep and a payee's payment both need it, since a leg that accepted a
// transaction (arcade has answered ACCEPTED_BY_NETWORK for a transaction
// whose input was already spent and mined) or that answers nothing (the tcp
// ingress) cannot be taken at its word.
package chainview

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

	"github.com/lightwebinc/bcommon/nodeapi"
)

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

// ErrUnknownTx is a transaction the node does not know.
var ErrUnknownTx = errors.New("the node does not know the transaction")

// Spender reads the node's UTXO view of txid (/api/v1/utxos/<txid>/json)
// and names the transaction that spent output vout, or "" while it is
// unspent.
func Spender(ctx context.Context, a *nodeapi.Asset, txid string, vout uint32) (string, error) {
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
		return "", ErrUnknownTx
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

// SpentElsewhere names the first input of tx that the node shows spent by
// another transaction, in words, or "" when none is: then tx can still
// mine as far as its inputs go. An input the node cannot answer for is
// passed over.
func SpentElsewhere(ctx context.Context, a *nodeapi.Asset, tx *transaction.Transaction) string {
	txid := tx.TxID().String()
	for i, in := range tx.Inputs {
		src := in.SourceTXID.String()
		by, err := Spender(ctx, a, src, in.SourceTxOutIndex)
		if err == nil && by != "" && by != txid {
			return fmt.Sprintf("input %d (%s.%d) is spent by %s", i, src, in.SourceTxOutIndex, by)
		}
	}
	return ""
}
