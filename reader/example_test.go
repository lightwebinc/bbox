package reader_test

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bbox/reader"
)

// The two questions a recipient asks of its office: one box, and one
// sender's envelopes.
func ExampleBoxQuery() {
	to := make([]byte, 33)
	to[0] = 0x02
	show := func(q reader.Query) {
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Println(keys)
	}
	show(reader.BoxQuery("example_abcdefghij", to, "inbox"))
	show(reader.FromQuery("example_abcdefghij", to, "02aa"))
	// Output:
	// [box office to]
	// [from office to]
}

// A recipient lists one box on every host, opens each envelope that
// verified, and hands a payment that passed every check to its wallet.
// Nothing a host answered is shown that did not verify, and a host that
// withholds what another answered is reported (Listing.Missing).
func ExampleClient_List() {
	ctx := context.Background()
	var (
		headers chaintracker.ChainTracker // the recipient's own headers
		w       wallet.Interface          // the recipient's wallet
		me      []byte                    // the recipient's identity key
	)
	c := &reader.Client{Hosts: []string{"https://host.example"}, Headers: headers, Timeout: 15 * time.Second}
	l, err := c.List(ctx, reader.BoxQuery("example_abcdefghij", me, "payment_inbox"), me)
	if err != nil {
		return
	}
	for _, r := range l.Refusals() {
		fmt.Println("refused", r.Host, r.Txid, r.Reason)
	}
	for _, it := range l.Items {
		m := reader.Open(ctx, w, "example", it, headers, time.Now())
		if m.Err != nil || m.Payment == nil || m.PayErr != nil {
			continue
		}
		args, err := m.Internalize("payment received", []string{"payment"})
		if err != nil {
			continue
		}
		if _, err := w.InternalizeAction(ctx, args, "example"); err == nil {
			fmt.Println("internalized", m.Paid(), "sat from", it.Txid)
		}
	}
}
