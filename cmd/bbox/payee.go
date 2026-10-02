package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/purse"
	"github.com/lightwebinc/bcommon/termsafe"

	"github.com/lightwebinc/bbox/internal/limits"
	"github.com/lightwebinc/bbox/internal/state"
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
	raw, err := os.ReadFile(filepath.Join(g.cfg.Home, "identity.json"))
	if err != nil {
		return fmt.Errorf("open the home %s: %w (run `bbox init`)", g.cfg.Home, err)
	}
	var id struct {
		WIF string `json:"wif"`
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		return fmt.Errorf("identity.json: %w", err)
	}
	k, err := ec.PrivateKeyFromWif(id.WIF)
	if err != nil {
		return fmt.Errorf("identity.json: %w", err)
	}
	line := fmt.Sprintf("BBOX_PAYEE_KEY=%s\n", hex.EncodeToString(k.Serialize()))
	if out == "-" {
		fmt.Fprint(g.stdout, line)
		g.say("the line above is a private key: keep it out of logs and shells' history")
		return nil
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator's own path
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return usage("%s exists; payee key never writes over a file", out)
		}
		return err
	}
	if _, err := fmt.Fprint(f, line); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "wrote BBOX_PAYEE_KEY to %s for payee %s\n", out, hex.EncodeToString(k.PubKey().Compressed()))
	return nil
}

// ledgerLine is one line of a host's payments.jsonl.
type ledgerLine struct {
	Txid              string `json:"txid"`
	Beef              string `json:"beef"`
	OutputIndex       uint32 `json:"outputIndex"`
	Satoshis          uint64 `json:"satoshis"`
	DerivationPrefix  string `json:"derivationPrefix"`
	DerivationSuffix  string `json:"derivationSuffix"`
	SenderIdentityKey string `json:"senderIdentityKey"`
	Class             string `json:"class"`
}

// payeeSettle internalizes every payment in the ledger this home has not.
func payeeSettle(ctx context.Context, g *global, paths []string, inFlight int) error {
	var lines []ledgerLine
	seen := map[string]bool{}
	for _, path := range paths {
		ls, err := readLedger(g, path)
		if err != nil {
			return err
		}
		for _, l := range ls {
			// The same payment in two ledgers (a copy, or two hosts sharing
			// one) is settled once.
			if !seen[l.Txid] {
				seen[l.Txid] = true
				lines = append(lines, l)
			}
		}
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
	var settled, already, failed, unsettleable, refusedNow int
	var sats uint64
	notSettled := func(l ledgerLine, err error) {
		failed++
		g.say("payment %s (%d sat, %s): NOT SETTLED: %v", l.Txid, l.Satoshis, termsafe.Text(l.Class), err)
	}
	// Every payment is checked first, then broadcast, and the proofs are
	// awaited together: at most inFlight are broadcast and not yet mined
	// at once, so a run takes about one block, not one block a payment.
	type job struct {
		l  ledgerLine
		in *purse.Incoming
	}
	var jobs []job
	for _, l := range lines {
		if slices.Contains(h.st.Settled, l.Txid) {
			already++
			continue
		}
		if h.st.IsUnsettleable(l.Txid) {
			unsettleable++
			continue
		}
		in, err := checkPayment(ctx, g, p, l)
		if err != nil {
			notSettled(l, err)
			continue
		}
		jobs = append(jobs, job{l, in})
	}
	type result struct {
		job
		err error
	}
	results := make(chan result)
	sem := make(chan struct{}, inFlight)
	go func() {
		var wg sync.WaitGroup
		for _, j := range jobs {
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				err := p.Broadcast(ctx, j.in)
				if err == nil {
					err = p.Await(ctx, j.in)
				}
				results <- result{j, err}
			}()
		}
		wg.Wait()
		close(results)
	}()
	for r := range results {
		err := r.err
		if err == nil {
			err = p.Take(r.in)
		}
		var re *purse.RefusedError
		if errors.As(err, &re) {
			// It will never mine: reported once, recorded, and passed over
			// by every later run.
			refusedNow++
			g.say("payment %s (%d sat, %s): REFUSED, NEVER SETTLES: %s; the payer took the coins back after the question was answered", r.l.Txid, r.l.Satoshis, termsafe.Text(r.l.Class), termsafe.Text(re.Why))
			h.st.Unsettleable = append(h.st.Unsettleable, state.Unsettleable{Txid: r.l.Txid, Why: re.Why})
			if err := h.st.Save(); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			notSettled(r.l, payWords(err, 0))
			continue
		}
		h.st.Settled = append(h.st.Settled, r.l.Txid)
		if err := h.st.Save(); err != nil {
			return err
		}
		settled++
		sats += r.l.Satoshis
		fmt.Fprintf(g.stdout, "settled %s: %d sat for %s from %s\n", r.l.Txid, r.l.Satoshis, termsafe.Text(r.l.Class), termsafe.Abbrev(r.l.SenderIdentityKey))
	}
	fmt.Fprintf(g.stdout, "%d payment(s) settled, %d sat; %d settled before; %d not settled; %d refused (%d before); pool %d output(s), %d sat\n",
		settled, sats, already, failed, refusedNow, unsettleable, h.e.Pool.Count(), h.e.Pool.Balance())
	switch {
	case refusedNow > 0:
		return refused("%d payment(s) refused by the network: their payers spent the coins elsewhere, and they will never settle", refusedNow)
	case failed > 0:
		return refused("%d payment(s) not settled: until one is, its payer can spend the coins elsewhere", failed)
	}
	return nil
}

// readLedger reads a host's payments.jsonl, passing over a line that does
// not parse: one cut short by a crash, whose question was never answered.
func readLedger(g *global, path string) ([]ledgerLine, error) {
	f, err := os.Open(path) //nolint:gosec // the operator's own path
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []ledgerLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	n := 0
	for sc.Scan() {
		n++
		t := strings.TrimSpace(sc.Text())
		if t == "" {
			continue
		}
		var l ledgerLine
		if err := json.Unmarshal([]byte(t), &l); err != nil || l.Txid == "" {
			g.say("%s line %d: not a payment; skipped", path, n)
			continue
		}
		lines = append(lines, l)
	}
	return lines, sc.Err()
}

func checkPayment(ctx context.Context, g *global, p *purse.Purse, l ledgerLine) (*purse.Incoming, error) {
	beef, err := base64.StdEncoding.DecodeString(l.Beef)
	if err != nil {
		return nil, fmt.Errorf("the ledger's BEEF is not base64: %w", err)
	}
	r, err := purse.Remittance(l.DerivationPrefix, l.DerivationSuffix, l.SenderIdentityKey)
	if err != nil {
		return nil, err
	}
	in, err := p.Check(ctx, wallet.InternalizeActionArgs{Tx: beef, Description: "bbox priced question " + l.Class,
		Labels: []string{"bbox", "payee"}, Outputs: []wallet.InternalizeOutput{{OutputIndex: l.OutputIndex,
			Protocol: wallet.InternalizeProtocolWalletPayment, PaymentRemittance: r}}})
	if err != nil {
		return nil, err
	}
	if in.Txid != l.Txid {
		return nil, fmt.Errorf("the ledger names %s and its BEEF holds %s", l.Txid, in.Txid)
	}
	return in, nil
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
