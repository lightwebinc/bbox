package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/lightwebinc/bcommon/payee"
	"github.com/lightwebinc/bcommon/purse"

	"github.com/lightwebinc/bbox/internal/limits"
)

const payeeHelp = `usage: bbox payee key -out FILE
       bbox payee settle <payments.jsonl>... [-in-flight N]

A host that prices a question is paid to its payee key (BBOX_PAYEE_KEY):
each payment is a BRC-29 output to a key derived from it, recorded by the
host in payments.jsonl in its state directory before the question is
answered, and not broadcast by the host. Until it is settled, the payer can
still spend the coins elsewhere: a payment is money only once settled.

key writes BBOX_PAYEE_KEY=<this home's identity private key> to FILE (mode
0600, never over an existing file), or with -out - to standard output, for
the host's environment: the home is then the payee's wallet.

settle takes every payment in the ledgers (one or more hosts' files; a
payment in two is settled once) that this home has not settled into the
home's wallet: each is checked to pay the key this identity derives for
its remittance and payer, verified against the headers, broadcast through
the settlement leg, waited for until it mines, and added to the pool, and
its txid is recorded in the home as settled. Every payment is broadcast
before any is waited for, so a run takes about one block however many
there are; -in-flight (default 16, at most 64) bounds how many are
broadcast and not yet mined at once. A payment the network refuses for
good (its payer spent the inputs elsewhere) is reported once, recorded,
and passed over by later runs. Run it on a schedule: the sooner a payment
is settled, the shorter the window in which the payer can take it back.`

func cmdPayee(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("payee", payeeHelp)
	out := fs.String("out", "", "key: the file to write BBOX_PAYEE_KEY to")
	inFlight := fs.Int("in-flight", limits.DefaultSettleInFlight, "settle: the most payments broadcast and not yet mined at once")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 1 && pos[0] == "key":
		return payeeKey(g, *out)
	case len(pos) >= 2 && pos[0] == "settle":
		if *inFlight < 1 || *inFlight > limits.MaxSettleInFlight {
			return usage("-in-flight %d is outside 1 to %d: payments broadcast and not yet mined at once (docs/limits.md)", *inFlight, limits.MaxSettleInFlight)
		}
		return payeeSettle(ctx, g, pos[1:], *inFlight)
	}
	return usage("payee key -out FILE | payee settle <payments.jsonl>...")
}

// payeeKey writes the home's identity private key as BBOX_PAYEE_KEY.
func payeeKey(g *global, out string) error {
	if out == "" {
		return usage("payee key writes a secret: name a file with -out (it is created at mode 0600), or -out - to print it")
	}
	k, err := payee.HomeKey(g.cfg.Home, "bbox")
	if err != nil {
		return err
	}
	env := payee.KeyEnv("bbox")
	if out == "-" {
		fmt.Fprint(g.stdout, payee.KeyLine(env, k))
		g.say("%s", payee.PrintedKeyNote)
		return nil
	}
	note, err := payee.CreateKeyFile(out, env, k)
	if errors.Is(err, payee.ErrKeyFileExists) {
		return usage("%s exists; payee key never writes over a file", out)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(g.stdout, note)
	return nil
}

// payeeSettle internalizes every payment in the ledgers this home has not.
func payeeSettle(ctx context.Context, g *global, paths []string, inFlight int) error {
	ps, err := payee.ReadLedgers(g.stderr, paths...)
	if err != nil {
		return err
	}
	h, err := g.openHome()
	if err != nil {
		return err
	}
	defer h.close()
	hc, err := g.headerClient()
	if err != nil {
		return err
	}
	p, err := g.purse(ctx, h, hc, 0)
	if err != nil {
		return err
	}
	rep, err := (&payee.Settler{App: "bbox", Payer: p, Record: payee.Saved(&h.st.Book, h.st.Save), Pool: h.e.Pool,
		InFlight: inFlight, Out: g.stdout, Warn: g.stderr}).Settle(ctx, ps)
	if err != nil {
		return err
	}
	if problem := rep.Problem(); problem != "" {
		return refused("%s", problem)
	}
	return nil
}

// payWords puts a refusal of the purse in this command's words. The library
// names no flag and no config key; the command that set them does.
func payWords(err error, maxSats uint64) error {
	switch {
	case errors.Is(err, purse.ErrOverMaxPay):
		return fmt.Errorf("%w: more than the %d this command may pay (-max-sats)", err, maxSats)
	case errors.Is(err, purse.ErrNoNode):
		return fmt.Errorf("%w (config key asset)", err)
	case errors.Is(err, purse.ErrNoSettler):
		return fmt.Errorf("%w (config key settle)", err)
	case errors.Is(err, purse.ErrNotMined):
		return fmt.Errorf("%w; run the command again to take it into the pool once it is", err)
	}
	return err
}
