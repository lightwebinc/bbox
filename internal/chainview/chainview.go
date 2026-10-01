// Package chainview answers, from the network's own words and the node's
// view of outputs, whether a transaction can still mine: what a settlement
// leg's error means, and which input another transaction spent, in words
// over bcommon's view of the node (nodeapi.Asset.SpentElsewhere). A sender's
// sweep and a payee's payment both need it, since a leg that accepted a
// transaction (arcade has answered ACCEPTED_BY_NETWORK for a transaction
// whose input was already spent and mined) or that answers nothing (the tcp
// ingress) cannot be taken at its word.
package chainview

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

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

// SpentElsewhere names the first input of tx that the node shows spent by
// another transaction, in words, or "" when none is: then tx can still
// mine as far as its inputs go. An input the node cannot answer for is
// passed over. The node's view is bcommon's (nodeapi.Asset.SpentElsewhere);
// this only words it.
func SpentElsewhere(ctx context.Context, a *nodeapi.Asset, tx *transaction.Transaction) string {
	var se *nodeapi.SpentError
	if errors.As(a.SpentElsewhere(ctx, tx), &se) {
		return fmt.Sprintf("input %d (%s) is spent by %s", se.Input, se.Outpoint, se.By)
	}
	return ""
}
