package module

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	bmodule "github.com/lightwebinc/bcommon/module"

	"github.com/lightwebinc/bbox/boxrec"
)

// Defects a review of a sibling host's index proved against it, each
// pinned here with the sequence that shows it in this one
// (hardening.test.ts): what one topic must not take from another, what
// must not wait in a queue, and what a restore in flight must not be
// handed.

func now() int64 { return time.Now().Unix() }

// equivocations are n envelopes from alice to bob on one funding output,
// lowest commitment first.
func equivocations(t *testing.T, chain *testChain, n int) []*transaction.Transaction {
	t.Helper()
	tree := fundingTree(t, alice, 1, chain)
	var many []*transaction.Transaction
	for i := 0; i < n; i++ {
		rec := envelopeRecord(t, letter{office: office, from: alice, to: bob, box: "inbox", created: u64(now() - 60)})
		many = append(many, mintCarrier(t, tree, 0, alice, rec, lockTime(uint32(i)))) //nolint:gosec // test
	}
	return byC(many)
}

func TestSweepInTwoOffices(t *testing.T) {
	chain := newTestChain(12000)
	s := newEngine(t, engineOpts{chain: chain})
	tree := fundingTree(t, alice, 2, chain)
	a := mintCarrier(t, tree, 0, alice, envelopeRecord(t, letter{office: office, from: alice, to: bob, box: "inbox", created: u64(now() - 60)}), lockTime(0))
	// The second office's carrier on the same tree: the funding key does not name an office.
	b := mintCarrier(t, tree, 1, alice, envelopeRecord(t, letter{office: office2, from: alice, to: bob, box: "inbox", created: u64(now() - 60)}), lockTime(0))
	zero(t, s.submitTx(a, topic))
	zero(t, s.submitTx(b, topic2))
	sw := mintSweep(t, tree, []uint32{0, 1}, alice, chain)
	zero(t, s.submitTx(sw, topic))
	zero(t, s.submitTx(sw, topic2))
	asked := func(ls *Service, o string, vout uint32) []string {
		got, err := lookupTxids(ls, nil, map[string]string{"office": o, "spent": idOf(tree) + "." + string(rune('0'+vout))})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if s.ls.Status(cOf(a)) != StatusRetracted || s.ls.Status(cOf(b)) != StatusRetracted {
		t.Fatal(s.ls.Status(cOf(a)), s.ls.Status(cOf(b)))
	}
	eq(t, asked(s.ls, office, 0), ids(sw), "office 1")
	eq(t, asked(s.ls, office2, 1), ids(sw), "the office whose carrier it retracts")
	if s.ls.Counts().Sweeps != 1 {
		t.Fatal(s.ls.Counts())
	}
	offices := map[string]bool{}
	for _, l := range s.journal.Lines {
		if l.K == "s" {
			offices[l.Office] = true
		}
	}
	if len(offices) != 2 || !offices[office] || !offices[office2] {
		t.Fatal("the journal names the sweep in each office", offices)
	}
	// Its tombstone is spent in both topics: a restore is handed neither
	// row, and reads both back by name.
	for _, tp := range []string{topic, topic2} {
		s.storage.markSpent(idOf(sw), 0, tp)
	}
	handed := append(s.storage.utxos(topic), s.storage.utxos(topic2)...)
	for _, o := range handed {
		if o.Outpoint.Txid.String() == idOf(sw) {
			t.Fatal("a spent tombstone handed to restore")
		}
	}
	again, err := Mount(newFakeHost(), baseConfig(), []string{topic, topic2}, s.journal, chain, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.LS.Restore(context.Background(), handed, s.storage); err != nil {
		t.Fatal(err)
	}
	eq(t, asked(again.LS, office, 0), ids(sw), "restored, office 1")
	eq(t, asked(again.LS, office2, 1), ids(sw), "restored, office 2")
	// Dropped from one topic, it still answers in the other.
	if err := s.ls.OutputNoLongerRetainedInHistory(context.Background(), &transaction.Outpoint{Txid: *sw.TxID(), Index: 0}, topic); err != nil {
		t.Fatal(err)
	}
	eq(t, asked(s.ls, office, 0), nil, "no longer retained in office 1")
	eq(t, asked(s.ls, office2, 1), ids(sw), "still answered in office 2")
	if s.ls.Counts().Sweeps != 1 {
		t.Fatal(s.ls.Counts())
	}
	// Retention deletes it from every topic that still holds it.
	late, err := NewService(Options{Offices: []string{office, office2}, Host: newFakeHost(), Journal: s.journal, Now: func() time.Time { return time.Now().Add(32 * 24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := late.Restore(context.Background(), handed, s.storage); err != nil {
		t.Fatal(err)
	}
	late.Prune(context.Background())
	if s.storage.find(idOf(sw), 0, topic) != nil || s.storage.find(idOf(sw), 0, topic2) != nil {
		t.Fatal("retention left the tombstone in storage")
	}
}

func TestProtoMemberIsRefused(t *testing.T) {
	s := newEngine(t, engineOpts{chain: newTestChain(12500)})
	q := json.RawMessage(`{"office":"` + office + `","to":"` + bob.hex + `","__proto__":"x"}`)
	var raw map[string]any
	_ = json.Unmarshal(q, &raw)
	if _, err := boxrec.ParseQuery(raw); boxrec.Reason(err) != "query" {
		t.Fatal(err)
	}
	if _, err := s.ls.Lookup(context.Background(), question(q)); err == nil || !strings.Contains(err.Error(), "refused: query") {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(`{"office":"`+office+`","to":"`+bob.hex+`"}`), &raw)
	delete(raw, "__proto__")
	if q, err := boxrec.ParseQuery(raw); err != nil || q.Class.Name != "inbox" {
		t.Fatal(q, err)
	}
}

func TestDropsDeletedAtOnceAndNoQueueWithoutDelete(t *testing.T) {
	chain := newTestChain(13000)
	s := newEngine(t, engineOpts{chain: chain})
	many := equivocations(t, chain, 11)
	for _, x := range many {
		zero(t, s.submitTx(x, topic))
	}
	// Nine kept, two dropped, with no retention pass in between.
	if c := s.ls.Counts(); c.Envelopes != 9 || len(s.storage.utxos(topic)) != 9 || c.PendingDeletes != 0 {
		t.Fatal(c, len(s.storage.utxos(topic)))
	}
	for _, x := range many[9:] {
		if s.storage.find(idOf(x), 0, topic) != nil {
			t.Fatal("a dropped carrier is still in storage")
		}
	}
	// A store with no DeleteOutput keeps what was dropped; the module does
	// not keep a list of it.
	chain2 := newTestChain(13500)
	more := equivocations(t, chain2, 31)
	host := newFakeHost()
	m, err := Mount(host, baseConfig(), []string{topic, topic2}, &MemoryJournal{}, chain2, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.LS.Restore(context.Background(), nil, cannotDelete{newMemStorage()}); err != nil {
		t.Fatal(err)
	}
	for _, x := range more {
		raw := atomicOf(t, x)
		if err := m.LS.OutputAdmittedByTopic(context.Background(), &bmodule.OutputAdmittedByTopic{Mode: bmodule.AdmitWholeTx, Topic: topic, AtomicBEEF: raw, Outpoint: transaction.Outpoint{Txid: *x.TxID()}}); err != nil {
			t.Fatal(err)
		}
	}
	m.LS.Prune(context.Background())
	if c := m.LS.Counts(); c.Envelopes != 9 || c.PendingDeletes != 0 {
		t.Fatal(c)
	}
	if n := host.logged("cannot delete dropped outputs"); n != 1 {
		t.Fatalf("said %d times, want once", n)
	}
}

func TestAdmissionWaitsForARestoreInFlight(t *testing.T) {
	chain := newTestChain(14000)
	s := newEngine(t, engineOpts{chain: chain})
	many := equivocations(t, chain, 10)
	for _, x := range many[1:] {
		zero(t, s.submitTx(x, topic))
	}
	journal := &MemoryJournal{Lines: append([]Line(nil), s.journal.Lines...)}
	m, err := Mount(newFakeHost(), baseConfig(), []string{topic, topic2}, journal, chain, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ls := m.LS
	// While the restore reads its first row, the lowest commitment of all is
	// admitted on the full row.
	admitted := make(chan error, 1)
	started := make(chan struct{})
	seenAtOnce := "not asked"
	s.storage.meanwhile = func() {
		go func() {
			close(started)
			admitted <- ls.OutputAdmittedByTopic(context.Background(), &bmodule.OutputAdmittedByTopic{Mode: bmodule.AdmitWholeTx, Topic: topic, AtomicBEEF: atomicOf(t, many[0]), Outpoint: transaction.Outpoint{Txid: *many[0].TxID()}})
		}()
		<-started
		time.Sleep(20 * time.Millisecond)
		select {
		case <-admitted:
			seenAtOnce = "admitted during the restore"
		default:
			seenAtOnce = ""
		}
	}
	// Rows handed without their BEEF are read again from storage.
	rows := s.storage.utxos(topic)
	for i := range rows {
		rows[i].Beef = nil
	}
	if _, err := ls.Restore(context.Background(), rows, s.storage); err != nil {
		t.Fatal(err)
	}
	if seenAtOnce != "" {
		t.Fatalf("the admission came during the restore and %q", seenAtOnce)
	}
	if err := <-admitted; err != nil {
		t.Fatal(err)
	}
	if ls.Status(cOf(many[0])) != StatusHeld || ls.Status(cOf(many[1])) != StatusSuperseded {
		t.Fatal("the lowest ever admitted is the winner", ls.Status(cOf(many[0])), ls.Status(cOf(many[1])))
	}
	if !ls.Dropped(cOf(many[9])) {
		t.Fatal("the row keeps eight superseded: the highest went")
	}
	if ls.Counts().Envelopes != 9 {
		t.Fatal(ls.Counts())
	}
}
