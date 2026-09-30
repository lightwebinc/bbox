package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bbox/internal/testchain"
	"strings"
	"testing"
	"time"
)

func TestPlaneSendListReadAck(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	alice := h.identity("alice")
	bob := h.identity("bob")
	_ = alice
	h.newOffice("bob")
	tx1 := h.send("alice", bob, "hello \x1b[31mbob\x1b]0;owned\x07")
	r := h.must("bob", "", "list")
	if !strings.Contains(r.stdout, tx1) || !strings.Contains(r.stdout, "["+h.aS.URL+", "+h.bS.URL+"]") {
		t.Fatalf("list:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("bob", "", "read")
	if !strings.Contains(r.stdout, "hello bob") || strings.ContainsAny(r.stdout, "\x1b\x07") {
		t.Fatalf("read:\n%q\n%s", r.stdout, r.stderr)
	}
	r = h.must("bob", "", "ack", tx1)
	if !strings.Contains(r.stdout, "acknowledged 1 envelope(s)") {
		t.Fatalf("ack:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("bob", "", "list")
	if strings.Contains(r.stdout, tx1) {
		t.Fatalf("list after ack:\n%s", r.stdout)
	}
}

func TestPaymentInsideAnEnvelopeIsInternalized(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	tx := h.send("alice", bob, "a coffee", "-pay", "5000")
	r := h.must("bob", "", "read", tx)
	if !strings.Contains(r.stdout, "payment  5000 sat in ") || !strings.Contains(r.stdout, "bbox internalize "+tx) {
		t.Fatalf("read:\n%s\n%s", r.stdout, r.stderr)
	}
	pay := regexp.MustCompile(`payment  5000 sat in ([0-9a-f]{64})`).FindStringSubmatch(r.stdout)[1]
	if h.chain.Tx(pay) != nil {
		t.Fatal("the sender broadcast the payment")
	}
	r = h.must("bob", "", "internalize", tx)
	if !strings.Contains(r.stdout, "internalized 5000 sat from") || !strings.Contains(r.stdout, "payment "+pay) || !strings.Contains(r.stdout, "acknowledged "+tx) {
		t.Fatalf("internalize:\n%s\n%s", r.stdout, r.stderr)
	}
	if !h.chain.Mined(pay) {
		t.Fatal("the recipient's wallet did not broadcast the payment")
	}
	r = h.must("bob", "", "internalize", tx)
	if !strings.Contains(r.stdout, "internalized already") {
		t.Fatalf("internalize again:\n%s", r.stdout)
	}
	r = h.must("bob", "", "doctor")
	if !strings.Contains(r.stdout, "identity    "+bob) {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
}

func TestHistoryPaysThe402(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	payee := h.identity("payee")
	h.newOffice("bob")
	h.startPaid(t, "payee", 7)
	_ = payee
	tx := h.send("alice", bob, "read me later")
	h.must("bob", "", "read", tx)
	h.must("bob", "", "ack", tx)
	r := h.must("bob", "", "terms")
	if !strings.Contains(r.stdout, "history        7 sat a question") {
		t.Fatalf("terms:\n%s", r.stdout)
	}
	r = h.must("bob", "", "history")
	if !strings.Contains(r.stdout, "paid 7 sat") || !strings.Contains(r.stdout, tx) {
		t.Fatalf("history:\n%s\n%s", r.stdout, r.stderr)
	}
	h.want(h.run("bob", "", "history", "-max-sats", "6"), exitUsage, "more than the 6 this command may pay")
	if len(h.paid.Payments) != 1 {
		t.Fatalf("payments %d", len(h.paid.Payments))
	}
	// The payee settles: the payment is broadcast and pooled once, and a
	// second run finds it settled.
	ledger := filepath.Join(h.dir, "payments.jsonl")
	if err := os.WriteFile(ledger, h.paid.Ledger(), 0o600); err != nil {
		t.Fatal(err)
	}
	r = h.must("payee", "", "payee", "settle", ledger)
	if !strings.Contains(r.stdout, "1 payment(s) settled, 7 sat") {
		t.Fatalf("settle:\n%s\n%s", r.stdout, r.stderr)
	}
	if !h.chain.Mined(h.paid.Payments[0].Txid) {
		t.Fatal("the settled payment is not mined")
	}
	r = h.must("payee", "", "payee", "settle", ledger)
	if !strings.Contains(r.stdout, "0 payment(s) settled, 0 sat; 1 settled before") {
		t.Fatalf("settle again:\n%s", r.stdout)
	}
	// Another home is not the payee: nothing it derives is paid.
	h.identity("other")
	h.want(h.run("other", "", "payee", "settle", ledger), exitRefused, "does not pay the key this identity derives")
}

func TestDropRetractsAtEveryHost(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	keep := h.send("alice", bob, "keep")
	gone := h.send("alice", bob, "take back")
	r := h.must("alice", "", "drop", gone)
	if !strings.Contains(r.stdout, "retracted 1 funding output(s)") || !strings.Contains(r.stderr, "not erasure") {
		t.Fatalf("drop:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("bob", "", "list")
	if !strings.Contains(r.stdout, keep) || strings.Contains(r.stdout, gone) {
		t.Fatalf("list after drop:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, host := range []*testchain.Host{h.a, h.b} {
		if host.Status(gone) != "retracted" || host.Status(keep) != "held" {
			t.Fatalf("statuses %s %s", host.Status(gone), host.Status(keep))
		}
	}
	r = h.must("alice", "", "drop", gone)
	if !strings.Contains(r.stderr, "retracted already") {
		t.Fatalf("drop again:\n%s", r.stderr)
	}
}

func TestUnicastQuorumAndResume(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	h.env["BBOX_MODE"] = "unicast"
	delete(h.env, "BBOX_FACADE")
	tx1 := h.send("alice", bob, "one")
	if !h.a.Holds(tx1) || !h.b.Holds(tx1) {
		t.Fatal("unicast: not at both hosts")
	}
	// b is down: quorum all refuses, the carrier stays persisted.
	h.b.SetDown(true)
	r := h.run("alice", "", "send", bob, "-m", "two", "-rate", "20")
	h.want(r, exitUsage, "1 of 2 host(s) took it and the quorum is 2")
	r = h.must("alice", "", "doctor")
	if !strings.Contains(r.stdout, "outbox      envelope") {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
	// b is back: the next send publishes the persisted carrier first.
	h.b.SetDown(false)
	r = h.must("alice", "", "send", bob, "-m", "three", "-rate", "20")
	if !strings.Contains(r.stderr, "publishing what a previous run persisted") {
		t.Fatalf("resume:\n%s\n%s", r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "host "+h.bS.URL+": took 2 object(s), missed 0") {
		t.Fatalf("tally:\n%s", r.stdout)
	}
	r = h.must("bob", "", "list")
	if strings.Count(r.stdout, "\n") != 3 {
		t.Fatalf("list:\n%s\n%s", r.stdout, r.stderr)
	}
	// A duplicate is confirmed by lookup, not counted as a refusal.
	h.must("alice", "", "-quorum", "one", "doctor")
}

func TestHostsThatDisagreeAreReported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	h.b.Drop = func(*transaction.Transaction) bool { return true }
	tx := h.send("alice", bob, "only at a")
	h.b.Drop = nil
	r := h.run("bob", "", "list")
	h.want(r, exitIncomplete, "host "+h.bS.URL+": DISAGREES")
	if !strings.Contains(r.stdout, tx) {
		t.Fatalf("an envelope one host withholds is shown from the others:\n%s", r.stdout)
	}
}

func TestLimitsAndUsage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	var refs []string
	for range 33 {
		refs = append(refs, "-ref", "https://example.com/x,"+strings.Repeat("ab", 32)+",10")
	}
	h.want(h.run("alice", "", append([]string{"send", bob, "-m", "x"}, refs...)...), exitUsage, "over the limit of 32")
	h.want(h.run("alice", "", "send", bob, "-m", "x", "-tree-count", "1001"), exitUsage, "-tree-count 1001 is over the limit of 1000")
	h.want(h.run("alice", "", "send", "02"+strings.Repeat("ff", 32), "-m", "x"), exitUsage, "is not an identity key")
	h.want(h.run("alice", "", "send", bob, "Inbox", "-m", "x"), exitUsage, "box name grammar")
	h.want(h.run("alice", "", "send", bob, "-m", "x", "-pay", "10", "-expires", "1h"), exitUsage, "at least 2h")
	h.want(h.run("alice", "", "send", bob), exitUsage, "nothing to send")
	h.want(h.run("bob", "", "ack", strings.Repeat("ab", 32)), exitUsage, "read it first")
	h.want(h.run("alice", "", "office", "new", "Bad"), exitUsage, "office name")
	h.env["BBOX_NETWORK"] = "main"
	h.want(h.run("alice", "", "fund", "-blocks", "1"), exitUsage, "on network main, import a payment with fund -txid")
}

func TestAHostThatEditsAnEnvelopeIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	tx := h.send("alice", bob, "the real text")
	// Host b edits one byte of the sealed content it serves.
	h.b.Tamper = func(beef []byte) []byte {
		i := bytes.Index(beef, []byte(`"signature":"`))
		beef[i+14] ^= 1
		return beef
	}
	r := h.run("bob", "", "read")
	h.want(r, exitRefused, "host "+h.bS.URL+": REFUSED "+tx)
	if !strings.Contains(r.stdout, "the real text") || !strings.Contains(r.stdout, "hosts    "+h.aS.URL+"\n") {
		t.Fatalf("the envelope from the honest host:\n%s", r.stdout)
	}
}

// A tree minted ahead never holds a command up: it is recorded as soon as
// the settlement leg takes it, and a later run switches to it, waiting for
// its block only when a carrier needs it.
func TestMintAheadDoesNotWaitForABlock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	h.send("alice", bob, "one")
	// From here nothing mines until the chain is told to.
	h.chain.SetHold(true)
	r := h.must("alice", "", "send", bob, "-m", "two", "-rate", "20", "-tree-count", "4")
	if !strings.Contains(r.stderr, "has 2 output(s) left, so the next is minted ahead") {
		t.Fatalf("no mint ahead:\n%s", r.stderr)
	}
	h.send("alice", bob, "three")
	h.send("alice", bob, "four")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for h.chain.Waiting() == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		h.chain.Mine()
	}()
	r = h.must("alice", "", "send", bob, "-m", "five", "-rate", "20", "-tree-count", "4")
	<-done
	if !strings.Contains(r.stderr, "minted ahead by an earlier run") {
		t.Fatalf("no switch to the tree minted ahead:\n%s", r.stderr)
	}
	h.chain.SetHold(false)
	r = h.must("bob", "", "list")
	if strings.Count(r.stdout, "\n") != 5 {
		t.Fatalf("list:\n%s\n%s", r.stdout, r.stderr)
	}
}
