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

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/termsafe"

	"github.com/lightwebinc/bbox/internal/purse"
)

const payeeHelp = `usage: bbox payee key -out FILE
       bbox payee settle <payments.jsonl>

A host that prices a question is paid to its payee key (BBOX_PAYEE_KEY):
each payment is a BRC-29 output to a key derived from it, recorded by the
host in payments.jsonl in its state directory before the question is
answered, and not broadcast by the host. Until it is settled, the payer can
still spend the coins elsewhere: a payment is money only once settled.

key writes BBOX_PAYEE_KEY=<this home's identity private key> to FILE (mode
0600, never over an existing file), or with -out - to standard output, for
the host's environment: the home is then the payee's wallet.

settle takes every payment in the ledger that this home has not settled
into the home's wallet: each is checked to pay the key this identity
derives for its remittance and payer, verified against the headers,
broadcast through the settlement leg, waited for until it mines, and added
to the pool, and its txid is recorded in the home as settled. A payment the
network refuses (its inputs spent elsewhere) is reported and left
unsettled. Run it on a schedule: the sooner a payment is settled, the
shorter the window in which the payer can take it back.`

func cmdPayee(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("payee", payeeHelp)
	out := fs.String("out", "", "key: the file to write BBOX_PAYEE_KEY to")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 1 && pos[0] == "key":
		return payeeKey(g, *out)
	case len(pos) == 2 && pos[0] == "settle":
		return payeeSettle(ctx, g, pos[1])
	}
	return usage("payee key -out FILE | payee settle <payments.jsonl>")
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
func payeeSettle(ctx context.Context, g *global, path string) error {
	f, err := os.Open(path) //nolint:gosec // the operator's own path
	if err != nil {
		return err
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
			// A line cut short by a crash: its question was never answered.
			g.say("%s line %d: not a payment; skipped", path, n)
			continue
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
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
	var settled, already, failed int
	var sats uint64
	for _, l := range lines {
		if slices.Contains(h.st.Settled, l.Txid) {
			already++
			continue
		}
		if err := settleOne(ctx, g, p, l); err != nil {
			failed++
			g.say("payment %s (%d sat, %s): NOT SETTLED: %v", l.Txid, l.Satoshis, termsafe.Text(l.Class), err)
			continue
		}
		h.st.Settled = append(h.st.Settled, l.Txid)
		if err := h.st.Save(); err != nil {
			return err
		}
		settled++
		sats += l.Satoshis
		fmt.Fprintf(g.stdout, "settled %s: %d sat for %s from %s\n", l.Txid, l.Satoshis, termsafe.Text(l.Class), termsafe.Abbrev(l.SenderIdentityKey))
	}
	fmt.Fprintf(g.stdout, "%d payment(s) settled, %d sat; %d settled before; %d not settled; pool %d output(s), %d sat\n",
		settled, sats, already, failed, h.e.Pool.Count(), h.e.Pool.Balance())
	if failed > 0 {
		return refused("%d payment(s) not settled: until one is, its payer can spend the coins elsewhere", failed)
	}
	return nil
}

func settleOne(ctx context.Context, g *global, p *purse.Purse, l ledgerLine) error {
	beef, err := base64.StdEncoding.DecodeString(l.Beef)
	if err != nil {
		return fmt.Errorf("the ledger's BEEF is not base64: %w", err)
	}
	r, err := purse.Remittance(l.DerivationPrefix, l.DerivationSuffix, l.SenderIdentityKey)
	if err != nil {
		return err
	}
	_, err = p.InternalizeAction(ctx, wallet.InternalizeActionArgs{Tx: beef, Description: "bbox priced question " + l.Class,
		Labels: []string{"bbox", "payee"}, Outputs: []wallet.InternalizeOutput{{OutputIndex: l.OutputIndex,
			Protocol: wallet.InternalizeProtocolWalletPayment, PaymentRemittance: r}}}, g.cfg.Originator)
	return err
}
