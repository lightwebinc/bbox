package main

import (
	"context"
	"encoding/hex"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"

	"github.com/lightwebinc/bbox/internal/config"
	"github.com/lightwebinc/bbox/send"
)

// pay builds a payment from the home from's pool to the home to's fund
// address, as the user's own wallet would, and sends it to the chain. It
// returns the payment and the template that signs for from's fund key.
func (h *harness) pay(from, to string, sats uint64) (*transaction.Transaction, transaction.UnlockingScriptTemplate) {
	h.t.Helper()
	ctx := context.Background()
	w, err := bwallet.Open(filepath.Join(h.dir, from), send.Profile)
	if err != nil {
		h.t.Fatal(err)
	}
	r, err := bwallet.Open(filepath.Join(h.dir, to), send.Profile)
	if err != nil {
		h.t.Fatal(err)
	}
	fund, err := r.Signer().FundScript()
	if err != nil {
		h.t.Fatal(err)
	}
	s := w.Signer()
	p := &producer.Payer{Pool: w.Pool, Tip: h.chain.Height(), Keys: map[string]*bwallet.Signer{s.IdentityHex(): s}}
	in, err := p.Take(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.spend(in.Tx, in.Vout, in.Unlocker, fund, sats), in.Unlocker
}

// spend sends a transaction paying sats to lock from output vout of src.
func (h *harness) spend(src *transaction.Transaction, vout uint32, u transaction.UnlockingScriptTemplate, lock *script.Script, sats uint64) *transaction.Transaction {
	h.t.Helper()
	tx := transaction.NewTransaction()
	tx.AddInputFromTx(src, vout, u)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: lock})
	if err := tx.Sign(); err != nil {
		h.t.Fatal(err)
	}
	if err := h.chain.Send(tx); err != nil {
		h.t.Fatal(err)
	}
	return tx
}

// mined gives tx its proof from the chain.
func (h *harness) mined(tx *transaction.Transaction) {
	h.t.Helper()
	h.chain.Mine()
	mp, _, ok := h.chain.Proof(tx.TxID().String())
	if !ok {
		h.t.Fatal("not mined")
	}
	tx.MerklePath = mp
}

func beefHex(t *testing.T, tx *transaction.Transaction) string {
	t.Helper()
	b, err := tx.BEEF()
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// TestFundImports: a home is funded from a payment sent from another
// wallet, by txid from the chain view or as the BEEF the wallet hands over;
// an unmined BEEF is held until it mines, or refused with -mined-only; a
// payment to someone else pays nothing.
func TestFundImports(t *testing.T) {
	h := newHarness(t)
	alice := h.identity("alice")
	h.must("bob", "", "init")

	// A mined payment, by txid and then again as BEEF: once only.
	paid, _ := h.pay("alice", "bob", 5000)
	h.mined(paid)
	r := h.must("bob", "", "fund", "-txid", paid.TxID().String())
	if !strings.Contains(r.stdout, "imported 1 of 1 output(s)") || !strings.Contains(r.stdout, "5000 sat, mined at height") {
		t.Fatalf("fund -txid:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("bob", beefHex(t, paid)+"\n", "fund", "-beef", "-")
	if !strings.Contains(r.stdout, "imported 0 of 1 output(s)") || !strings.Contains(r.stderr, "in the pool already") {
		t.Fatalf("fund -beef of a payment already imported:\n%s\n%s", r.stdout, r.stderr)
	}

	// Someone else's payment pays nothing to this home.
	h.want(h.run("alice", "", "fund", "-txid", paid.TxID().String()), exitUsage, "pays nothing to the fund address")

	// An unmined payment whose parent is proven: refused with -mined-only,
	// else held until it mines.
	parent, u := h.pay("alice", "alice", 20000)
	h.mined(parent)
	bob, err := bwallet.Open(filepath.Join(h.dir, "bob"), send.Profile)
	if err != nil {
		t.Fatal(err)
	}
	fund, err := bob.Signer().FundScript()
	if err != nil {
		t.Fatal(err)
	}
	h.chain.SetHold(true)
	held := h.spend(parent, 0, u, fund, 7000)
	h.want(h.run("bob", "", "fund", "-txid", held.TxID().String()), exitUsage, "has not mined yet")
	h.want(h.run("bob", beefHex(t, held), "fund", "-beef", "-", "-mined-only"), exitUsage, "has not mined yet")
	r = h.must("bob", beefHex(t, held), "fund", "-beef", "-")
	if !strings.Contains(r.stdout, "7000 sat, not mined yet: held until it mines") {
		t.Fatalf("fund -beef unmined:\n%s\n%s", r.stdout, r.stderr)
	}
	if r := h.must("bob", "", "doctor"); !strings.Contains(r.stdout, "change from 1 transaction(s) held until mined") {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
	// Once it mines, the next command that spends collects its proof.
	h.chain.SetHold(false)
	h.chain.Mine()
	h.newOffice("bob")
	h.send("bob", alice, "funded with no coinbase")
	if r := h.must("bob", "", "doctor"); strings.Contains(r.stdout, "held until mined") {
		t.Fatalf("doctor after the payment mined:\n%s", r.stdout)
	}

	// A BEEF that does not parse is refused before anything is added.
	h.want(h.run("bob", "beef", "fund", "-beef", "-"), exitUsage, "payment in the BEEF")
}

// TestNoNodeLegs: on main and test a publisher needs no node: WhatsOnChain
// headers and chain view, GorillaPool's arcade with its verdict held to the
// chain view's spends, and the network's fee rate.
func TestNoNodeLegs(t *testing.T) {
	for _, n := range []string{"main", "test"} {
		cfg, err := config.Defaults().Apply(map[string]string{"network": n, "facade": "https://host.example"})
		if err != nil {
			t.Fatal(err)
		}
		g := &global{cfg: cfg, stdout: io.Discard, stderr: io.Discard}
		l, err := g.legs()
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		want := map[string]string{"main": publish.ArcadeMainnet, "test": publish.ArcadeTestnet}[n]
		if l.Arcade == nil || l.Arcade.Base != want || l.Arcade.Spends == nil || l.Chain == nil || l.Headers == nil {
			t.Fatalf("%s: %+v", n, l)
		}
		f, err := g.fees(context.Background())
		if err != nil || f.Rate != mint.DefaultFees.Rate {
			t.Fatalf("%s: fees %+v %v", n, f, err)
		}
	}
	cfg, _ := config.Defaults().Apply(map[string]string{"network": "regtest", "facade": "https://host.example", "header_url": "https://bridge.example"})
	if _, err := (&global{cfg: cfg}).legs(); err == nil || !strings.Contains(err.Error(), "asset:<node URL>") {
		t.Fatalf("regtest with no chain view: %v", err)
	}
}
