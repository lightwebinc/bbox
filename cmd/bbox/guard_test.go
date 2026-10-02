package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bbox/internal/boxrec"
	"github.com/lightwebinc/bbox/internal/send"
	"github.com/lightwebinc/bbox/internal/state"
	"github.com/lightwebinc/bbox/internal/testchain"
)

// isSweep reports a sweep: a funding-shaped tombstone and at most a change
// output, where a funding tree has an output a carrier.
func isSweep(tx *transaction.Transaction) bool {
	return len(tx.Outputs) > 0 && len(tx.Outputs) <= 2 && len(tx.Inputs) >= 2 && boxrec.IsFundingShape(*tx.Outputs[0].LockingScript)
}

// isCarrier reports an envelope or receipt carrier.
func isCarrier(tx *transaction.Transaction) bool {
	return len(tx.Outputs) == 1 && boxrec.ClaimOf(*tx.Outputs[0].LockingScript) != boxrec.KindNone
}

// copyHome copies a home, as a second device of one identity would hold it.
func copyHome(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		if d.Name() == "lock" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// editState rewrites a home's state file as JSON.
func (h *harness) editState(name string, edit func(st map[string]any)) {
	h.t.Helper()
	path := filepath.Join(h.dir, name, state.File)
	raw, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		h.t.Fatal(err)
	}
	edit(st)
	out, _ := json.Marshal(st)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// lastSent is the txid of the last envelope a home persisted.
func (h *harness) lastSent(name string) string {
	h.t.Helper()
	var txid string
	h.editState(name, func(st map[string]any) {
		sent := st["sent"].([]any)
		txid = sent[len(sent)-1].(map[string]any)["txid"].(string)
	})
	return txid
}

// A sweep is marked failed only when the node names another transaction
// as the spender of one of its inputs. A leg's refusal alone leaves it in
// flight: the next drop sends the same bytes again.
func TestASweepIsKeptUnlessAnotherSpends(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	gone := h.send("alice", bob, "take back")
	refused := false
	// The sweep is refused once by the leg, with every input unspent.
	h.chain.Refuse = func(tx *transaction.Transaction) string {
		if refused || !isSweep(tx) {
			return ""
		}
		refused = true
		return "txn-mempool-conflict"
	}
	r := h.run("alice", "", "drop", gone)
	if !refused || r.code != exitUsage || strings.Contains(r.stdout+r.stderr, "marked failed") || !strings.Contains(r.stderr, "it is persisted; the next drop sends it again") {
		t.Fatalf("a sweep the leg refused once: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if r := h.must("alice", "", "doctor"); !strings.Contains(r.stdout, "in flight") || strings.Contains(r.stdout, "FAILED") {
		t.Fatalf("the sweep is not in flight:\n%s", r.stdout)
	}
	r = h.must("alice", "", "drop", gone)
	if !strings.Contains(r.stdout, "retracted 1 funding output(s)") {
		t.Fatalf("the sweep was not finished by the next drop:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, host := range []*testchain.Host{h.a, h.b} {
		if host.Status(gone) != "retracted" {
			t.Fatalf("status %s", host.Status(gone))
		}
	}
	if r := h.must("alice", "", "doctor"); !strings.Contains(r.stdout, "1 sweep(s)") {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
}

// After a reorganisation a proof kept from before may name a block that is
// no longer in the best chain. A reader handed such a proof by a host, and
// a sender holding one in its home, each take the funding tree's current
// proof from the node and verify that against their own headers.
func TestStaleProofsAreReplaced(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	tx := h.send("alice", bob, "one")
	// The hosts answer every carrier under a proof of another block.
	h.a.Stale, h.b.Stale = true, true
	if r := h.must("bob", "", "read", tx); !strings.Contains(r.stdout, "one") || strings.Contains(r.stderr, "REFUSED") {
		t.Fatalf("a reader given stale proofs:\n%s\n%s", r.stdout, r.stderr)
	}
	// With no chain source there is nothing to replace them with: nothing
	// verifies, and nothing is shown.
	asset := h.env["BBOX_ASSET"]
	delete(h.env, "BBOX_ASSET")
	r := h.run("bob", "", "list")
	h.want(r, exitRefused, "a host answered what does not verify")
	if strings.Contains(r.stdout, tx) {
		t.Fatalf("an envelope under a stale proof was shown:\n%s", r.stdout)
	}
	h.env["BBOX_ASSET"] = asset
	h.a.Stale, h.b.Stale = false, false

	// The sender's home: the proof it kept of its funding tree names
	// another block.
	shift := func(m map[string]any) {
		mp, err := transaction.NewMerklePathFromHex(m["bumpHex"].(string))
		if err != nil {
			t.Fatal(err)
		}
		mp.BlockHeight += 3
		m["bumpHex"] = mp.Hex()
	}
	h.editState("alice", func(st map[string]any) {
		shift(st["tree"].(map[string]any))
		for _, tr := range st["trees"].([]any) {
			shift(tr.(map[string]any))
		}
	})
	r = h.must("alice", "", "send", bob, "-m", "two", "-rate", "20", "-tree-count", "4")
	if !strings.Contains(r.stderr, "the node's current proof") {
		t.Fatalf("a send from a home with a stale proof:\n%s\n%s", r.stdout, r.stderr)
	}
	two := sentLine.FindStringSubmatch(r.stdout)[1]
	if r := h.must("bob", "", "read", two); !strings.Contains(r.stdout, "two") || strings.Contains(r.stderr, "DISAGREES") {
		t.Fatalf("after the send:\n%s\n%s", r.stdout, r.stderr)
	}
	// The home is healed: the next send says nothing of proofs.
	if r := h.must("alice", "", "send", bob, "-m", "three", "-rate", "20", "-tree-count", "4"); strings.Contains(r.stderr, "the node's current proof") {
		t.Fatalf("the home kept its stale proof:\n%s", r.stderr)
	}
}

// acked sends n envelopes to bob and has bob read and acknowledge them, so
// they are history, and returns their txids.
func (h *harness) acked(bob string, n int) []string {
	h.t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		ids = append(ids, h.send("alice", bob, "read me later"))
		// Each a second apart: a cursor is <created>:<txid>.
		time.Sleep(1100 * time.Millisecond)
	}
	h.must("bob", "", "read")
	h.must("bob", "", "ack", "-all")
	return ids
}

// One question is paid for once, and a payment that left this process is
// never given back to the pool: a host that keeps answering 402 is handed
// one payment and no more, and a host that took a payment and did not
// answer leaves the payer's next payment on other coins, so both settle.
func TestOnePaymentAQuestionAndNoRefundOnceSent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.identity("payee")
	h.newOffice("bob")
	h.startPaid(t, "payee", 7)
	h.acked(bob, 1)
	// A host that asks again and again.
	h.paid.Again = true
	before := h.chain.Sent
	r := h.run("bob", "", "history")
	h.want(r, exitRefused, "asked for a payment again after one was sent")
	if strings.Count(r.stderr, "was sent to") != 1 {
		t.Fatalf("a host that keeps answering 402:\n%s\n%s", r.stdout, r.stderr)
	}
	// Exactly one payment of this home's reached the settlement leg: the
	// one it made, kept as made.
	if n := h.chain.Sent - before; n != 1 {
		t.Fatalf("%d transactions were sent for one question", n)
	}
	h.paid.Again = false
	// A host that takes the payment and does not answer.
	h.paid.FailPaid = true
	r = h.run("bob", "", "history")
	if r.code != exitUsage || !strings.Contains(r.stderr, "it left this process, so it is kept as made") || !strings.Contains(r.stderr, "status 500") {
		t.Fatalf("a host that took a payment and answered 500: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	h.paid.FailPaid = false
	// The next payment spends other coin, and the question is answered.
	r = h.must("bob", "", "history")
	if !strings.Contains(r.stdout, "paid 7 sat to ") || !strings.Contains(r.stdout, "from ") {
		t.Fatalf("the history after:\n%s\n%s", r.stdout, r.stderr)
	}
	// Both payments the host recorded settle: neither was made on a coin
	// that another spent.
	ledger := filepath.Join(h.dir, "payments.jsonl")
	if err := os.WriteFile(ledger, h.paid.Ledger(), 0o600); err != nil {
		t.Fatal(err)
	}
	r = h.must("payee", "", "payee", "settle", ledger)
	if !strings.Contains(r.stdout, "2 payment(s) settled, 14 sat") || strings.Contains(r.stdout+r.stderr, "REFUSED") {
		t.Fatalf("payee settle:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A payment is recorded in the home before it is sent: when it reaches the
// host, the payer's state already holds it.
func TestAPaymentIsRecordedBeforeItIsSent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.identity("payee")
	h.newOffice("bob")
	h.startPaid(t, "payee", 7)
	h.acked(bob, 1)
	recorded := -1
	h.paid.OnPayment = func() {
		raw, err := os.ReadFile(filepath.Join(h.dir, "bob", state.File))
		if err != nil {
			recorded = 0
			return
		}
		var st struct {
			Payments []json.RawMessage `json:"payments"`
		}
		_ = json.Unmarshal(raw, &st)
		recorded = len(st.Payments)
	}
	if r := h.must("bob", "", "history"); !strings.Contains(r.stdout, "paid 7 sat") {
		t.Fatalf("history:\n%s\n%s", r.stdout, r.stderr)
	}
	if recorded != 1 {
		t.Fatalf("when the payment reached the host the payer's home recorded %d payment(s)", recorded)
	}
}

// What a command that asks several priced questions pays is bounded in
// all, whatever a host's pages and prices add up to.
func TestABudgetBoundsEveryPage(t *testing.T) {
	b := &budget{per: 20, total: 50}
	for i, want := range []uint64{20, 20, 10} {
		got, err := b.next()
		if err != nil || got != want {
			t.Fatalf("question %d may be paid %d, want %d: %v", i, got, want, err)
		}
		b.paid(got)
	}
	if _, err := b.next(); err == nil || !strings.Contains(err.Error(), "has paid 50 sat") {
		t.Fatalf("past the budget: %v", err)
	}
}

// history -all walks the history page after page, one payment a page, and
// stops paying at its budget: the pages are the host's, their sum is the
// command's.
func TestHistoryWalksPagesWithinItsBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.identity("payee")
	h.newOffice("bob")
	h.startPaid(t, "payee", 7)
	ids := h.acked(bob, 3)
	// The host answers pages of 2 envelopes: the 3 are two pages.
	h.a.HistoryPage = 2
	r := h.must("bob", "", "history")
	if strings.Count(r.stdout, "paid 7 sat") != 1 || !strings.Contains(r.stdout, ids[1]) || strings.Contains(r.stdout, ids[2]) || !strings.Contains(r.stderr, "a full page: the next is bbox history -after") {
		t.Fatalf("one page:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("bob", "", "history", "-all")
	if strings.Count(r.stdout, "paid 7 sat") != 2 || !strings.Contains(r.stdout, ids[2]) || !strings.Contains(r.stderr, "3 envelope(s)") {
		t.Fatalf("every page:\n%s\n%s", r.stdout, r.stderr)
	}
	// A budget of one page and a half pays one page, and says the history
	// was not read to its end.
	paid := len(h.paid.Payments)
	r = h.run("bob", "", "history", "-all", "-budget", "10")
	h.want(r, exitIncomplete, "the history is not read to its end")
	if strings.Count(r.stdout, "paid 7 sat") != 1 || len(h.paid.Payments) != paid+1 {
		t.Fatalf("a budget of 10 at 7 a page: %d payment(s)\n%s\n%s", len(h.paid.Payments)-paid, r.stdout, r.stderr)
	}
	// A budget below the first page's price pays nothing.
	paid = len(h.paid.Payments)
	r = h.run("bob", "", "history", "-budget", "5")
	if r.code == exitOK || strings.Contains(r.stdout, "paid ") || len(h.paid.Payments) != paid || !strings.Contains(r.stderr, "-budget") {
		t.Fatalf("a budget below the price: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
}

// On the plane a carrier counts as published only once the hosts named
// answer it by lookup: the facade's "nothing admitted" is a duplicate and a
// refusal alike. An envelope fewer hosts than the quorum hold stays in the
// outbox.
func TestAnUnconfirmedCarrierIsNotPublished(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	h.a.Drop, h.b.Drop = isCarrier, isCarrier
	r := h.run("alice", "", "send", bob, "-m", "one", "-rate", "20", "-tree-count", "4")
	if r.code != exitUsage || !strings.Contains(r.stderr, "0 of 2 host(s) named answer it") || !strings.Contains(r.stderr, "it is persisted") {
		t.Fatalf("an envelope no host took: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	one := h.lastSent("alice")
	if r := h.must("alice", "", "doctor"); !strings.Contains(r.stdout, "outbox      envelope "+one) {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
	// One host of two, with a quorum of all: still not published. With a
	// quorum of one it is, before anything new is built.
	h.b.Drop = nil
	h.want(h.run("alice", "", "send", bob, "-m", "two", "-rate", "20", "-tree-count", "4"), exitUsage, "1 of 2 host(s) named answer it and 2 must")
	h.env["BBOX_QUORUM"] = "one"
	r = h.must("alice", "", "send", bob, "-m", "two", "-rate", "20", "-tree-count", "4")
	if !strings.Contains(r.stderr, "publishing what a previous run persisted") {
		t.Fatalf("with a quorum of one:\n%s\n%s", r.stdout, r.stderr)
	}
	if r := h.must("alice", "", "doctor"); strings.Contains(r.stdout, "outbox      ") {
		t.Fatalf("doctor after:\n%s", r.stdout)
	}
	if !h.b.Holds(one) || h.a.Holds(one) {
		t.Fatal("the envelope is not where the plane left it")
	}
}

// Work an earlier command left unfinished does not stop a drop.
func TestDropIsNotBlockedByOldWork(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["BBOX_MODE"] = "unicast"
	delete(h.env, "BBOX_FACADE")
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	gone := h.send("alice", bob, "take back")
	// An envelope one host never takes: it stays in the outbox.
	h.b.Drop = isCarrier
	h.want(h.run("alice", "", "send", bob, "-m", "stuck", "-rate", "20", "-tree-count", "4"), exitUsage, "it is persisted")
	r := h.must("alice", "", "drop", gone)
	if !strings.Contains(r.stdout, "retracted 1 funding output(s)") || !strings.Contains(r.stderr, "is left for the next command") {
		t.Fatalf("drop behind an envelope a host does not take:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, host := range []*testchain.Host{h.a, h.b} {
		if host.Status(gone) != "retracted" {
			t.Fatalf("status %s", host.Status(gone))
		}
	}
}

// An output of a tree that another copy of the home already swept does not
// make the sweep of the rest fail for good: it is left out, and the
// transaction that spends it is taken as its sweep.
func TestDropPassesOverASpentOutput(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	one := h.send("alice", bob, "one")
	two := h.send("alice", bob, "two")
	copyHome(t, filepath.Join(h.dir, "alice"), filepath.Join(h.dir, "alice2"))
	h.must("alice2", "", "drop", one)
	r := h.must("alice", "", "drop", one, two)
	if !strings.Contains(r.stdout, "retracted 1 funding output(s)") || !strings.Contains(r.stderr, "already spent by") || !strings.Contains(r.stderr, "taken as their sweep") {
		t.Fatalf("drop with an output already swept:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, host := range []*testchain.Host{h.a, h.b} {
		for _, id := range []string{one, two} {
			if host.Status(id) != "retracted" {
				t.Fatalf("%s is %s at a host", id, host.Status(id))
			}
		}
	}
	// Both are recorded as retracted: dropping them again does nothing.
	r = h.must("alice", "", "drop", one, two)
	if strings.Count(r.stderr, "is retracted already") != 2 {
		t.Fatalf("drop again:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A sweep that cannot be sent does not keep an envelope a previous run
// persisted from the hosts: each piece of unfinished work is tried.
func TestStuckWorkDoesNotStopOtherWork(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["BBOX_MODE"] = "unicast"
	delete(h.env, "BBOX_FACADE")
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	gone := h.send("alice", bob, "take back")
	// An envelope host b does not take, then a sweep the node is too busy
	// to take: the home holds both unfinished.
	h.b.Drop = isCarrier
	h.want(h.run("alice", "", "send", bob, "-m", "stuck", "-rate", "20", "-tree-count", "4"), exitUsage, "it is persisted")
	stuck := h.lastSent("alice")
	h.chain.Busy = isSweep
	h.want(h.run("alice", "", "drop", gone), exitUsage, "it is persisted; the next drop sends it again")
	// Host b is back: the sweep still cannot be sent, and the envelope is
	// published all the same.
	h.b.Drop = nil
	r := h.run("alice", "", "send", bob, "-m", "next", "-rate", "20", "-tree-count", "4")
	if r.code != exitUsage || !strings.Contains(r.stderr, "the next drop sends it again") {
		t.Fatalf("the sweep is still stuck: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if !h.b.Holds(stuck) {
		t.Fatalf("the envelope was not published by the run whose sweep was stuck:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A coin a run took from the pool and never recorded a spend of (the run
// stopped in between) is found by the next command and put back, when the
// node shows it unspent.
func TestACoinARunTookAndNeverSpentGoesBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	h.send("alice", bob, "one")
	// The home as a run that stopped left it: the state records the pool
	// with a coin, and the pool's own file no longer holds it.
	statePath := filepath.Join(h.dir, "alice", state.File)
	poolPath := filepath.Join(h.dir, "alice", "wallet.json")
	var pool struct {
		Outputs []json.RawMessage `json:"outputs"`
	}
	raw, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatal(err)
	}
	var whole map[string]json.RawMessage
	if err := json.Unmarshal(raw, &whole); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &pool); err != nil || len(pool.Outputs) < 2 {
		t.Fatalf("the pool: %d coins, %v", len(pool.Outputs), err)
	}
	h.editState("alice", func(st map[string]any) { st["taking"] = pool.Outputs })
	before := len(pool.Outputs)
	whole["outputs"], _ = json.Marshal(pool.Outputs[1:])
	out, _ := json.Marshal(whole)
	if err := os.WriteFile(poolPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
	r := h.must("alice", "", "send", bob, "-m", "two", "-rate", "20", "-tree-count", "4")
	if !strings.Contains(r.stderr, "is back in the pool: a run stopped between taking it and recording what spent it") {
		t.Fatalf("the next command:\n%s\n%s", r.stdout, r.stderr)
	}
	raw, _ = os.ReadFile(poolPath)
	if err := json.Unmarshal(raw, &pool); err != nil || len(pool.Outputs) < before {
		t.Fatalf("the pool after: %d coins, %d before the run that stopped, %v", len(pool.Outputs), before, err)
	}
	// A command that ended leaves nothing recorded as being taken.
	raw, _ = os.ReadFile(statePath)
	if strings.Contains(string(raw), `"taking"`) {
		t.Fatal("the journal outlived the command")
	}
}

// The home's lock is taken before its wallet is opened: the coin pool is
// read whole and written back whole, so a pool read before the lock could
// put back coins another command spent. A command that finds the home in
// use says so before it has read anything of the wallet.
func TestTheLockComesBeforeTheWallet(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.identity("payee")
	h.newOffice("bob")
	h.startPaid(t, "payee", 7)
	gone := h.send("alice", bob, "one")
	home := filepath.Join(h.dir, "alice")
	unlock, err := state.Lock(filepath.Join(home, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	// A wallet file that does not read: a command that opened the wallet
	// before the lock would trip on it instead of on the lock.
	pool := filepath.Join(home, "wallet.json")
	raw, err := os.ReadFile(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pool, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	txid := strings.Repeat("ab", 32)
	for _, args := range [][]string{{"fund"}, {"fund", "-txid", txid}, {"office", "new", "other"}, {"init"}, {"send", bob, "-m", "x"},
		{"drop", gone}, {"ack", "-all"}, {"read"}, {"internalize", txid}, {"history"}, {"payee", "settle", pool}} {
		r := h.run("alice", "", args...)
		if r.code != exitUsage || !strings.Contains(r.stderr, "another bbox command is using the home") {
			t.Errorf("%v with the home locked: exit %d\n%s", args, r.code, r.stderr)
		}
	}
	if err := os.WriteFile(pool, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	unlock()
	h.send("alice", bob, "two")
}

// sendRefs seals and publishes an envelope with references, as tooling
// that does not check a locator would, and returns its txid.
func (h *harness) sendRefs(name, to string, refs ...string) string {
	h.t.Helper()
	ctx := context.Background()
	g, _, errb := h.global(name)
	hm, err := g.openHome()
	if err != nil {
		h.t.Fatal(err)
	}
	defer hm.close()
	eng, err := g.engine(ctx, hm, 4)
	if err != nil {
		h.t.Fatalf("%v\n%s", err, errb)
	}
	defer func() { _ = eng.Close(ctx) }()
	parsed, err := parseRefs(refs)
	if err != nil {
		h.t.Fatal(err)
	}
	doc := &boxrec.Object{}
	doc.Set(boxrec.PlainRefs, parsed)
	plain, err := boxrec.Canonical(doc)
	if err != nil {
		h.t.Fatal(err)
	}
	key, _ := hex.DecodeString(to)
	sent, err := eng.Envelope(ctx, send.Letter{Office: h.office, To: key, Box: "inbox", Created: uint64(time.Now().Unix()), Plaintext: plain}) //nolint:gosec // a clock after 1970
	if err != nil {
		h.t.Fatalf("%v\n%s", err, errb)
	}
	return sent.Txid
}

// A reference's locator is the sender's text: one with a line break in it
// writes no line of its own when the message is read, and one with a
// character a terminal acts on reaches no terminal. This tooling refuses to
// send one.
func TestAHostileLocatorForgesNoLine(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	digest := strings.Repeat("ab", 32)
	h.want(h.run("alice", "", "send", bob, "-ref", "https://example.com/a\npayment  5000 sat in "+digest+","+digest+",4"), exitUsage, "the locator holds a line break")
	// Another tooling would not refuse it.
	tx := h.sendRefs("alice", bob, "https://example.com/a\npayment  5000 sat in "+digest+": acceptable\r\nenvelope "+digest+"\u0085from     x,"+digest+",4")
	r := h.must("bob", "", "read", tx)
	for _, line := range strings.Split(r.stdout+"\n"+r.stderr, "\n") {
		if strings.HasPrefix(line, "payment  ") || (strings.HasPrefix(line, "envelope ") && !strings.Contains(line, tx)) || strings.ContainsAny(line, "\r\u0085") {
			t.Fatalf("the locator wrote a line of its own, or reached the terminal:\n%s\n%s", r.stdout, r.stderr)
		}
	}
	if !strings.Contains(r.stdout, "ref      https://example.com/a payment  5000 sat") {
		t.Fatalf("the reference is not shown on its one line:\n%s", r.stdout)
	}
}

// A note is filtered for the terminal whole: a long one loses nothing, and
// one that quotes what a node or a host wrote carries no control character.
func TestANoteIsFilteredAndWhole(t *testing.T) {
	long := strings.Repeat("a refusal with two transaction ids in it; ", 20) + "drop again to build a new sweep"
	if got := plain(long); got != long {
		t.Fatalf("a note of %d bytes was cut to %d", len(long), len(got))
	}
	if got := plain("spent by \x1b[2J\x1b]0;owned\x07evil\nnext"); strings.ContainsAny(got, "\x1b\x07") || !strings.HasSuffix(got, "\nnext") {
		t.Fatalf("%q", got)
	}
}
