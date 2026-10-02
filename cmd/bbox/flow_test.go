package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
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
	// ack -all leaves a payment to internalize; acknowledged by txid, no
	// host answers it any more, and internalize checks the carrier kept.
	tx2 := h.send("alice", bob, "another", "-pay", "3000")
	plain := h.send("alice", bob, "no money")
	h.must("bob", "", "read")
	r = h.must("bob", "", "ack", "-all")
	if !strings.Contains(r.stderr, tx2+" carries a payment of 3000 sat not yet taken") || !strings.Contains(r.stdout, "acknowledged 1 envelope(s)") {
		t.Fatalf("ack -all:\n%s\n%s", r.stdout, r.stderr)
	}
	_ = plain
	h.must("bob", "", "ack", tx2)
	r = h.must("bob", "", "internalize", tx2)
	if !strings.Contains(r.stderr, "using the carrier kept when it was read") || !strings.Contains(r.stdout, "internalized 3000 sat") {
		t.Fatalf("internalize after ack:\n%s\n%s", r.stdout, r.stderr)
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

// A host gives each session a budget of signed responses and refuses a
// request over it 429 without a signature. The command waits as the host
// says and asks once more, while it has paid nothing; refused twice, it
// says so. A refusal of the request that carried the payment is not asked
// again: that would take a second payment, and the first left this process,
// so it is kept as made and its coin is not reused.
func TestHistoryWaitsOutALimitedSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.identity("payee")
	h.newOffice("bob")
	h.startPaid(t, "payee", 7)
	tx := h.send("alice", bob, "read me later")
	h.must("bob", "", "read", tx)
	h.must("bob", "", "ack", tx)
	// The question is refused: asked again, it is answered 402, paid and answered.
	h.paid.Limit(1)
	r := h.must("bob", "", "history")
	if !strings.Contains(r.stdout, "paid 7 sat") || !strings.Contains(r.stdout, tx) || !strings.Contains(r.stderr, "waiting 1s and asking once more") || len(h.paid.Payments) != 1 {
		t.Fatalf("history, the question limited: %d payments\n%s\n%s", len(h.paid.Payments), r.stdout, r.stderr)
	}
	// The paid request is refused: the payment left this process, so it is
	// kept as made, broadcast by its payer, and not paid a second time.
	sent := h.chain.Sent
	h.paid.Limit(2)
	r = h.run("bob", "", "history")
	h.want(r, exitIncomplete, "refused the paid request for its session's budget (429)")
	if !strings.Contains(r.stderr, "it is kept as made and its coins are not reused") || strings.Contains(r.stderr, "asking once more") ||
		len(h.paid.Payments) != 1 || h.chain.Sent != sent+1 {
		t.Fatalf("history, the payment limited: %d payments at the host, %d sent\n%s\n%s", len(h.paid.Payments), h.chain.Sent-sent, r.stdout, r.stderr)
	}
	// Refused twice: the command stops, and nothing was paid.
	h.paid.Limit(1, 2)
	h.want(h.run("bob", "", "history"), exitIncomplete, "(429) twice; ask again later")
	if len(h.paid.Payments) != 1 {
		t.Fatalf("payments %d", len(h.paid.Payments))
	}
	// And the wallet still pays, on a coin the payment it kept did not
	// spend.
	h.paid.Limit()
	r = h.must("bob", "", "history")
	if !strings.Contains(r.stdout, "paid 7 sat") || len(h.paid.Payments) != 2 {
		t.Fatalf("history after: %d payments\n%s\n%s", len(h.paid.Payments), r.stdout, r.stderr)
	}
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
	// One host of the two takes it: published under a quorum of one.
	h.b.Drop = func(*transaction.Transaction) bool { return true }
	h.env["BBOX_QUORUM"] = "one"
	tx := h.send("alice", bob, "only at a")
	delete(h.env, "BBOX_QUORUM")
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

func TestHistoryHostIsAnOrigin(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("bob")
	h.newOffice("bob")
	h.want(h.run("bob", "", "history", "-at", "https://terms.example.com/bbox"), exitUsage, "is not an origin")
	h.want(h.run("bob", "", "terms", "https://terms.example.com/bbox"), exitUsage, "is not an origin")
	h.want(h.run("bob", "", "terms", "terms.example.com"), exitUsage, "is not an origin")
}

// In unicast, a host that holds an envelope its recipient acknowledged
// answers a resend with nothing admitted, and no box question answers the
// envelope; the receipt naming it confirms that the host took it.
func TestUnicastAcknowledgedResendIsConfirmed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	h.env["BBOX_MODE"] = "unicast"
	delete(h.env, "BBOX_FACADE")
	// b takes nothing: the envelope reaches a only and stays persisted.
	h.b.Drop = func(*transaction.Transaction) bool { return true }
	r := h.run("alice", "", "send", bob, "-m", "one", "-rate", "20", "-tree-count", "4")
	h.want(r, exitUsage, "1 of 2 host(s) took it and the quorum is 2")
	m := regexp.MustCompile(`outbox      envelope ([0-9a-f]{64})`).FindStringSubmatch(h.must("alice", "", "doctor").stdout)
	if m == nil {
		t.Fatal("the envelope is not persisted")
	}
	tx := m[1]
	// bob reads and acknowledges it at a alone.
	h.must("bob", "", "-hosts", h.aS.URL, "read", tx)
	h.must("bob", "", "-hosts", h.aS.URL, "ack", tx)
	if h.a.Status(tx) != "acknowledged" {
		t.Fatalf("status at a: %s", h.a.Status(tx))
	}
	// b is back: the resend is a duplicate at a, confirmed by the receipt.
	h.b.Drop = nil
	r = h.must("alice", "", "send", bob, "-m", "two", "-rate", "20", "-tree-count", "4")
	if !strings.Contains(r.stdout, "host "+h.aS.URL+": took 2 object(s), missed 0") || !h.b.Holds(tx) {
		t.Fatalf("resend:\n%s\n%s", r.stdout, r.stderr)
	}
}

// Every transaction the command settles goes through a leg that takes
// Extended Format only, as a fabric ingress and arcade do: a funding tree,
// a payment the recipient internalizes, and a sweep, whose inputs are
// given their sources again after it is rebuilt from its persisted bytes.
func TestSettlementLegTakesExtendedFormat(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	h.chain.Ingress(l)
	h.env["BBOX_SETTLE"] = "tcp:" + l.Addr().String()
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	tx := h.send("alice", bob, "paid", "-pay", "2000")
	h.must("bob", "", "read", tx)
	r := h.must("bob", "", "internalize", tx)
	if !strings.Contains(r.stdout, "internalized 2000 sat") {
		t.Fatalf("internalize:\n%s\n%s", r.stdout, r.stderr)
	}
	gone := h.send("alice", bob, "take back")
	r = h.must("alice", "", "drop", gone)
	if !strings.Contains(r.stdout, "retracted 1 funding output(s)") {
		t.Fatalf("drop:\n%s\n%s", r.stdout, r.stderr)
	}
	if n := h.chain.IngressRefused(); n != 0 {
		t.Fatalf("the ingress refused %d submission(s)", n)
	}
}

// A drop whose sweep the settlement leg did not take leaves it in flight;
// the same drop asked again finishes that sweep and sweeps nothing twice.
func TestDropAgainFinishesTheSweepInFlight(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	gone := h.send("alice", bob, "take back")
	h.chain.Busy = func(*transaction.Transaction) bool { return true }
	h.want(h.run("alice", "", "drop", gone), exitUsage, "it is persisted; the next drop sends it again")
	r := h.must("alice", "", "doctor")
	if !strings.Contains(r.stdout, "in flight") || strings.Contains(r.stdout, "FAILED") {
		t.Fatalf("a transient failure must leave the sweep in flight:\n%s", r.stdout)
	}
	h.chain.Busy = nil
	r = h.must("alice", "", "drop", gone)
	if !strings.Contains(r.stdout, "retracted 1 funding output(s)") || strings.Count(r.stdout, "retracted ") != 1 {
		t.Fatalf("drop again:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, host := range []*testchain.Host{h.a, h.b} {
		if host.Status(gone) != "retracted" {
			t.Fatalf("status %s", host.Status(gone))
		}
	}
	r = h.must("alice", "", "doctor")
	if strings.Contains(r.stdout, "in flight") || !strings.Contains(r.stdout, "1 sweep(s)") {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
}

// A sweep an input of which another transaction took is marked failed, and
// it no longer blocks the drop, which builds a new sweep on another coin
// and retracts. Through a node's RPC and through arcade.
func TestRefusedSweepFailsAndFreesTheNextDrop(t *testing.T) {
	t.Parallel()
	for _, leg := range []string{"rpc", "arcade"} {
		t.Run(leg, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if leg == "arcade" {
				h.env["BBOX_SETTLE"] = "arcade:" + h.chainS.URL + "/arcade"
			}
			h.identity("alice")
			bob := h.identity("bob")
			h.newOffice("bob")
			gone := h.send("alice", bob, "take back")
			tree := h.sentTree("alice", gone)
			other := strings.Repeat("ab", 32)
			// As the sweep is sent, another transaction takes its fee coin.
			h.mu.Lock()
			h.onSend = func(tx *transaction.Transaction) {
				for _, in := range tx.Inputs {
					if in.SourceTXID.String() != tree {
						h.chain.SpendElsewhere(in.SourceTXID.String(), in.SourceTxOutIndex, other)
					}
				}
			}
			h.mu.Unlock()
			r := h.must("alice", "", "drop", gone)
			if !strings.Contains(r.stderr, "refused by the network") || !strings.Contains(r.stderr, "is spent by "+other) ||
				!strings.Contains(r.stderr, "was not given back") || !strings.Contains(r.stdout, "retracted 1 funding output(s)") {
				t.Fatalf("drop:\n%s\n%s", r.stdout, r.stderr)
			}
			for _, host := range []*testchain.Host{h.a, h.b} {
				if host.Status(gone) != "retracted" {
					t.Fatalf("status %s", host.Status(gone))
				}
			}
			r = h.must("alice", "", "doctor")
			if !strings.Contains(r.stdout, "FAILED") || strings.Contains(r.stdout, "in flight") || !strings.Contains(r.stdout, "2 sweep(s)") {
				t.Fatalf("doctor:\n%s", r.stdout)
			}
		})
	}
}

// sentTree is the funding tree an envelope a home sent spends.
func (h *harness) sentTree(name, txid string) string {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.dir, name, "state.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	var st struct {
		Sent []struct {
			Txid string `json:"txid"`
			Tree string `json:"tree"`
		} `json:"sent"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		h.t.Fatal(err)
	}
	for _, s := range st.Sent {
		if s.Txid == txid {
			return s.Tree
		}
	}
	h.t.Fatalf("%s did not send %s", name, txid)
	return ""
}

// A leg that answers nothing (the tcp ingress) cannot say a sweep was
// refused; the node's view of its inputs can. An input spent by another
// transaction fails the sweep before any wait for a block.
func TestSweepWithAnInputSpentElsewhereFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	h.chain.Ingress(l)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	gone := h.send("alice", bob, "take back")
	// Every coin of the home is spent by another transaction: whichever
	// pays a sweep's fee, the sweep cannot mine.
	raw, err := os.ReadFile(filepath.Join(h.dir, "alice", "wallet.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pool struct {
		Outputs []struct {
			Txid string `json:"txid"`
			Vout uint32 `json:"vout"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(raw, &pool); err != nil || len(pool.Outputs) == 0 {
		t.Fatalf("pool: %v %+v", err, pool)
	}
	other := strings.Repeat("ab", 32)
	for _, o := range pool.Outputs {
		h.chain.SpendElsewhere(o.Txid, o.Vout, other)
	}
	h.env["BBOX_SETTLE"] = "tcp:" + l.Addr().String()
	h.want(h.run("alice", "", "drop", gone), exitRefused, "is spent by "+other)
	if n := h.chain.IngressRefused(); n != 3 {
		t.Fatalf("the ingress refused %d submission(s), want the three sweeps tried", n)
	}
	r := h.must("alice", "", "doctor")
	if !strings.Contains(r.stdout, "FAILED") || strings.Contains(r.stdout, "in flight") {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
	for _, host := range []*testchain.Host{h.a, h.b} {
		if host.Status(gone) == "retracted" {
			t.Fatal("a failed sweep retracted the envelope")
		}
	}
}

// payee settle broadcasts every payment before it waits for any, so the
// run takes one block, not one a payment; -in-flight bounds how many are
// broadcast and not yet mined at once.
func TestSettleBroadcastsThenWaitsTogether(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		inFlight string
		most     int
	}{{"16", 3}, {"1", 1}} {
		t.Run("in-flight "+c.inFlight, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.identity("bob")
			h.identity("payee")
			h.newOffice("bob")
			h.startPaid(t, "payee", 7)
			h.must("bob", "", "fund", "-blocks", "4")
			for range 3 {
				h.must("bob", "", "history")
			}
			if len(h.paid.Payments) != 3 {
				t.Fatalf("payments %d", len(h.paid.Payments))
			}
			ledger := filepath.Join(h.dir, "payments.jsonl")
			if err := os.WriteFile(ledger, h.paid.Ledger(), 0o600); err != nil {
				t.Fatal(err)
			}
			// The same ledger twice, as two hosts' files: each payment once.
			copyOf := filepath.Join(h.dir, "payments-b.jsonl")
			if err := os.WriteFile(copyOf, h.paid.Ledger(), 0o600); err != nil {
				t.Fatal(err)
			}
			h.chain.SetHold(true)
			done := make(chan result, 1)
			go func() { done <- h.run("payee", "", "payee", "settle", ledger, copyOf, "-in-flight", c.inFlight) }()
			deadline := time.After(30 * time.Second)
			most := 0
			for {
				select {
				case r := <-done:
					if r.code != 0 || !strings.Contains(r.stdout, "3 payment(s) settled, 21 sat") {
						t.Fatalf("settle: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
					}
					if most != c.most {
						t.Fatalf("at most %d payment(s) were broadcast and unmined at once, want %d", most, c.most)
					}
					return
				case <-deadline:
					t.Fatalf("settle did not finish; at most %d broadcast at once", most)
				case <-time.After(20 * time.Millisecond):
				}
				n := h.chain.Waiting()
				most = max(most, n)
				if n > c.most {
					t.Fatalf("%d payments broadcast and unmined, more than %d", n, c.most)
				}
				// Mine only once as many as may be are waiting: settling one
				// a block would never get there.
				if n == c.most || (n > 0 && n == 3-len(h.minedPayments())) {
					time.Sleep(100 * time.Millisecond)
					h.chain.Mine()
				}
			}
		})
	}
}

func (h *harness) minedPayments() []string {
	var out []string
	for _, p := range h.paid.Payments {
		if h.chain.Mined(p.Txid) {
			out = append(out, p.Txid)
		}
	}
	return out
}

// An envelope that reached one host only: a reader sees the hosts
// disagree, and list -fill copies it across, after which they agree.
func TestListFillCopiesAMissingEnvelopeAcross(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	h.env["BBOX_MODE"] = "unicast"
	delete(h.env, "BBOX_FACADE")
	r := h.must("alice", "", "-hosts", h.aS.URL, "send", bob, "-m", "only at a")
	tx := regexp.MustCompile(`(?m)^sent    ([0-9a-f]{64})`).FindStringSubmatch(r.stdout)
	if tx == nil {
		t.Fatalf("send:\n%s", r.stdout)
	}
	if !h.a.Holds(tx[1]) || h.b.Holds(tx[1]) {
		t.Fatal("the envelope must be at a only")
	}
	h.want(h.run("bob", "", "list"), exitIncomplete, "host "+h.bS.URL+": DISAGREES")
	r = h.must("bob", "", "list", "-fill")
	if !strings.Contains(r.stdout, "host "+h.bS.URL+": filled 1 of 1 envelope(s) it lacked") || !h.b.Holds(tx[1]) {
		t.Fatalf("list -fill:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("bob", "", "list")
	if !strings.Contains(r.stdout, tx[1]) || strings.Contains(r.stderr, "DISAGREES") {
		t.Fatalf("list after fill:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A payment whose payer spent its input elsewhere before the payee settled
// never mines: settle says so at once (not after a wait for a block),
// records it, and passes it over on every later run. Through a node's RPC,
// which refuses it, and through the tcp ingress, which answers nothing.
func TestSettleReportsADoubleSpentPaymentOnce(t *testing.T) {
	t.Parallel()
	for _, leg := range []string{"rpc", "tcp"} {
		t.Run(leg, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.identity("bob")
			h.identity("payee")
			h.newOffice("bob")
			h.startPaid(t, "payee", 7)
			h.must("bob", "", "history")
			if len(h.paid.Payments) != 1 {
				t.Fatalf("payments %d", len(h.paid.Payments))
			}
			ledger := filepath.Join(h.dir, "payments.jsonl")
			if err := os.WriteFile(ledger, h.paid.Ledger(), 0o600); err != nil {
				t.Fatal(err)
			}
			var line struct {
				Beef string `json:"beef"`
			}
			if err := json.Unmarshal(bytes.SplitN(h.paid.Ledger(), []byte("\n"), 2)[0], &line); err != nil {
				t.Fatal(err)
			}
			raw, err := base64.StdEncoding.DecodeString(line.Beef)
			if err != nil {
				t.Fatal(err)
			}
			p, err := transaction.NewTransactionFromBEEF(raw)
			if err != nil {
				t.Fatal(err)
			}
			other := strings.Repeat("cd", 32)
			h.chain.SpendElsewhere(p.Inputs[0].SourceTXID.String(), p.Inputs[0].SourceTxOutIndex, other)
			if leg == "tcp" {
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { l.Close() })
				h.chain.Ingress(l)
				h.env["BBOX_SETTLE"] = "tcp:" + l.Addr().String()
			}
			start := time.Now()
			r := h.run("payee", "", "payee", "settle", ledger)
			h.want(r, exitRefused, "REFUSED, NEVER SETTLES")
			why := map[string]string{"rpc": "rpc error -26: missing or spent input", "tcp": "is spent by " + other}[leg]
			if !strings.Contains(r.stderr, why) || !strings.Contains(r.stdout, "0 not settled; 1 refused (0 before)") {
				t.Fatalf("settle:\n%s\n%s", r.stdout, r.stderr)
			}
			if d := time.Since(start); d > 20*time.Second {
				t.Fatalf("settle took %s: it waited for a block that never comes", d)
			}
			r = h.must("payee", "", "payee", "settle", ledger)
			if !strings.Contains(r.stdout, "0 not settled; 0 refused (1 before)") {
				t.Fatalf("settle again:\n%s\n%s", r.stdout, r.stderr)
			}
		})
	}
}

// A drop whose only coins are change from a transaction not yet mined (a
// tree minted ahead took the last proven coin) waits for that change to
// mine and then sweeps, rather than failing for want of a fee.
func TestDropWaitsForChangeToMine(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	bob := h.identity("bob")
	h.newOffice("bob")
	h.identity("alice")
	first := h.send("alice", bob, "one", "-tree-count", "4")
	// The second send leaves two outputs and mints the next tree ahead
	// from the one coin, and that tree is held unmined.
	h.chain.SetHold(true)
	h.send("alice", bob, "two", "-tree-count", "4")
	if h.chain.Waiting() == 0 {
		t.Fatal("no tree was minted ahead")
	}
	// Every other coin is gone: what is left is that tree's change.
	wf := filepath.Join(h.dir, "alice", "wallet.json")
	raw, err := os.ReadFile(wf)
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Outputs []map[string]any `json:"outputs"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	var left []map[string]any
	for _, o := range w.Outputs {
		if o["unproven"] == true {
			left = append(left, o)
		}
	}
	if len(left) != 1 {
		t.Fatalf("%d unproven coins, want the ahead tree's change", len(left))
	}
	w.Outputs = left
	if raw, err = json.Marshal(w); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wf, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	h.want(h.run("alice", "", "doctor"), 0, "pool        1 output(s)")
	go func() {
		time.Sleep(3 * time.Second)
		h.chain.SetHold(false)
		h.chain.Mine()
	}()
	r := h.must("alice", "", "drop", first)
	if !strings.Contains(r.stderr, "waiting for one") || !strings.Contains(r.stdout, "retracted 1 funding output(s)") {
		t.Fatalf("drop:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A tree minted ahead by one run is the successor for the next: a later run
// that spends near the end of the tree does not mint yet another.
func TestOneTreeMintedAheadAcrossRuns(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	ahead := func() int {
		raw, err := os.ReadFile(filepath.Join(h.dir, "alice", "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			Ahead []json.RawMessage `json:"ahead"`
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		return len(st.Ahead)
	}
	// Eight outputs, minted ahead at four left: the fourth send starts the
	// next tree, and the fifth to seventh, each a run of its own, must not.
	for i := 1; i <= 7; i++ {
		h.send("alice", bob, fmt.Sprintf("n%d", i), "-tree-count", "8")
		if i >= 4 && ahead() != 1 {
			t.Fatalf("after send %d: %d tree(s) minted ahead, want 1", i, ahead())
		}
	}
	// The eighth uses the last output; the ninth switches to the tree
	// minted ahead, with no mint of its own.
	h.send("alice", bob, "n8", "-tree-count", "8")
	h.send("alice", bob, "n9", "-tree-count", "8")
	if n := ahead(); n != 0 {
		t.Fatalf("after switching: %d tree(s) still minted ahead", n)
	}
}
