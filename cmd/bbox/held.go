package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/heldpay"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/purse"
)

// heldWait is how long a paid question waits for a payment the host holds
// for confirmation to mine before it sends the payment again.
const heldWait = 10 * time.Minute

// heldWhy is the host's reason when it held a payment, "" when it did not.
func heldWhy(t *heldpay.Tap) string {
	why, held := t.Held()
	if !held {
		return ""
	}
	if why == "" {
		why = "held for confirmation"
	}
	return why
}

// sendHeldAgain waits, up to heldWait, for tx, a payment the host holds for
// confirmation, to mine, and then asks again with the same payment, which
// buys the answer. When it does not mine in time, or there is no chain view
// to watch it on, the held answer stands.
func (g *global) sendHeldAgain(ctx context.Context, p *purse.Purse, tx *transaction.Transaction, tap *heldpay.Tap, resp *http.Response, ferr error,
	again func(payment string) (*http.Response, error)) (*http.Response, error) {
	var chain nodeapi.Chain = p.Chain
	if chain == nil && p.Asset != nil {
		chain = p.Asset
	}
	if chain == nil || tap.Payment() == "" {
		return resp, ferr
	}
	g.say("the host holds payment %s for confirmation; waiting up to %s for it to mine, then sending it again", tx.TxID(), heldWait)
	// A host with no leg to broadcast on holds the payment unsent: it is
	// this home's own transaction, so it is broadcast from here too (a node
	// that has it already answers so, which is no failure).
	if p.Settler != nil {
		if err := p.Settler.Submit(ctx, tx); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already") {
			g.say("payment %s: not broadcast from here (%v)", tx.TxID(), err)
		}
	}
	if err := heldpay.Mined(ctx, chain, tx, heldWait, p.Poll); err != nil {
		g.say("payment %s did not mine in time (%v): the host keeps it held", tx.TxID(), err)
		return resp, ferr
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	tap.Reset()
	return again(tap.Payment())
}
