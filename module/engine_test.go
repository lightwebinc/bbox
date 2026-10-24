package module

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	bmodule "github.com/lightwebinc/bcommon/module"
)

// The module inside the engine's admission path (engine.test.ts): a
// stand-in for the Go engine's ordinary path over an in-memory store, with
// the engine's SPV before any manager runs: what the engine records for a
// refusal, the suppression and out-of-order attacks spec section 8.1
// guards against, every golden transaction, answers hydrated from storage,
// and restore from the engine's own storage in any order. A Go host that
// mounts the module runs it in its real engine in its own tests.

func vectorEngine(t *testing.T, o engineOpts) (*engine, *txVectors) {
	t.Helper()
	v := loadTxVectors(t)
	if o.chain == nil {
		o.chain = newTestChain(9000).withVectors(t, v)
	}
	o.spv = true
	o.config = baseConfig(v.Office)
	return newEngine(t, o), v
}

func TestSuppressionAttack(t *testing.T) {
	v := loadTxVectors(t)
	note := v.byName(t, "carrier-envelope-note")
	// The same txid, its funding tree unproven: SPV passes, the beef rule refuses.
	worse := v.byName(t, "carrier-beef-tree-unproven")
	if worse.Txid != note.Txid {
		t.Fatal("not the same txid")
	}
	naive, _ := vectorEngine(t, engineOpts{naive: true})
	eq(t, u32str(naive.submit(worse.beef(t), "")), nil, "naive: nothing admitted")
	if !naive.storage.isApplied(note.Txid, topic) {
		t.Fatal("the refusal is recorded as applied")
	}
	before := naive.calls
	eq(t, u32str(naive.submit(note.beef(t), "")), nil, "naive: the genuine carrier")
	if naive.calls != before {
		t.Fatal("the genuine carrier is a duplicate the manager never sees")
	}
	real, _ := vectorEngine(t, engineOpts{})
	if got := real.submit(worse.beef(t), ""); got != nil {
		t.Fatal("raised: nothing admitted", got)
	}
	if real.storage.isApplied(note.Txid, topic) {
		t.Fatal("nothing recorded")
	}
	if n := real.host.count("bbox_refused_total", map[string]string{"reason": "beef"}); n != 1 {
		t.Fatal(n)
	}
	zero(t, real.submit(note.beef(t), ""))
}

func u32str(a []uint32) []string {
	if a == nil {
		return nil
	}
	out := []string{}
	for _, x := range a {
		out = append(out, string(rune('0'+x)))
	}
	return out
}

func TestOutOfOrder(t *testing.T) {
	e, v := vectorEngine(t, engineOpts{})
	tree := v.byName(t, "funding-tree")
	sweep := v.byName(t, "sweep")
	unmined := v.byName(t, "sweep-unmined")
	spend := v.byName(t, "spend-tombstone")
	zero(t, e.submit(tree.beef(t), ""))
	// The unmined sweep spends the held output 0 of the tree: judged as a
	// sweep, refused unmined.
	if got := e.submit(unmined.beef(t), ""); got != nil {
		t.Fatal(got)
	}
	if e.storage.isApplied(unmined.Txid, topic) {
		t.Fatal("recorded")
	}
	if o := e.storage.find(tree.Txid, 0, topic); o == nil || o.Spent {
		t.Fatal("the held output is marked spent")
	}
	if e.storage.isApplied(sweep.Txid, topic) {
		t.Fatal("the sweep before it arrives")
	}
	// A spend of the tombstone before the sweep arrives: nothing held,
	// refused not-bbox.
	if got := e.submit(spend.beef(t), ""); got != nil {
		t.Fatal(got)
	}
	if e.storage.isApplied(spend.Txid, topic) {
		t.Fatal("recorded")
	}
	zero(t, e.submit(sweep.beef(t), ""))
	if o := e.storage.find(tree.Txid, 0, topic); o == nil || !o.Spent {
		t.Fatal("the sweep spends the held output")
	}
	if got := e.submit(spend.beef(t), ""); got == nil || len(got) != 0 {
		t.Fatal("offered again, the spend is accepted for what it retains", got)
	}
	if !e.storage.isApplied(spend.Txid, topic) {
		t.Fatal("not recorded")
	}
	if o := e.storage.find(sweep.Txid, 0, topic); o == nil || !o.Spent {
		t.Fatal("the tombstone stays, spent")
	}
}

func TestEveryVectorThroughTheEngine(t *testing.T) {
	e, v := vectorEngine(t, engineOpts{})
	admitted := map[string]bool{}
	for _, x := range v.Transactions {
		if x.Verdict == "admit" {
			admitted[x.Txid] = true
		}
	}
	engineRefused := 0
	for i := range v.Transactions {
		x := &v.Transactions[i]
		got, err := e.admit(x.beef(t), topic)
		if err != nil {
			var r *Refused
			if !asRefused(err, &r) {
				// The engine's own SPV refuses before any manager runs.
				if x.Verdict != "refuse" {
					t.Fatalf("%s: %v", x.Name, err)
				}
				engineRefused++
			}
		}
		switch {
		case x.Verdict == "admit":
			if !e.storage.isApplied(x.Txid, topic) {
				t.Fatalf("%s: not recorded", x.Name)
			}
		case !admitted[x.Txid]:
			if len(got) != 0 || e.storage.isApplied(x.Txid, topic) {
				t.Fatalf("%s: admitted %v", x.Name, got)
			}
		}
	}
	t.Logf("%d vectors refused by the engine's SPV before the manager", engineRefused)
	cm := func(name string) string { return v.byName(t, name).Admits.Commitment }
	for name, want := range map[string]string{
		"carrier-envelope-note": StatusRetracted, "carrier-locktime-max": StatusRetracted,
		"carrier-envelope-payment": StatusHeld, "carrier-receipt": StatusHeld,
	} {
		if s := e.ls.Status(cm(name)); s != want {
			t.Fatalf("%s: %s, want %s", name, s, want)
		}
	}
	eq(t, e.answer(map[string]string{"office": v.Office, "by": v.RecipientIdentityKey, "receiptFor": v.byName(t, "carrier-envelope-refs").Txid}),
		[]string{v.byName(t, "carrier-receipt").Txid}, "receipt")
	eq(t, e.answer(map[string]string{"office": v.Office, "spent": v.SweepRetractsOutpoints[1]}), []string{v.byName(t, "sweep").Txid}, "sweep")
	// The vectors were created long before now: out of the window, so
	// history answers the payment envelope.
	eq(t, e.answer(map[string]string{"office": v.Office, "history": v.RecipientIdentityKey}), []string{v.byName(t, "carrier-envelope-payment").Txid}, "history")
}

func asRefused(err error, r **Refused) bool {
	x, ok := err.(*Refused)
	if ok {
		*r = x
	}
	return ok
}

func TestRestoreFromEngineStorageInAnyOrder(t *testing.T) {
	n := time.Now().Unix()
	s, v := vectorEngine(t, engineOpts{})
	aTree := fundingTree(t, alice, 4, s.chain)
	bTree := fundingTree(t, bob, 1, s.chain)
	seal := func(vout uint32, box string, created int64, lt uint32) *transaction.Transaction {
		return mintCarrier(t, aTree, vout, alice, envelopeRecord(t, letter{office: v.Office, from: alice, to: bob, box: box, created: u64(created)}), lt)
	}
	e1 := seal(0, "inbox", n-300, lockTime(0))
	e1b := seal(0, "inbox", n-290, lockTime(1))
	e2 := seal(1, "files", n-200, lockTime(0))
	e3 := seal(2, "inbox", n-100, lockTime(0))
	rc := mintCarrier(t, bTree, 0, bob, receiptRecord(t, v.Office, bob, [][32]byte{commitment(e2)}, u64(n)), lockTime(0))
	sw := mintSweep(t, aTree, []uint32{2}, alice, s.chain)
	for _, x := range []*transaction.Transaction{rc, sw, e3, e2, e1b, e1} {
		zero(t, s.submitTx(x, ""))
	}
	zero(t, s.submit(v.byName(t, "funding-tree").beef(t), ""))
	zero(t, s.submit(v.byName(t, "sweep").beef(t), ""))
	if got := s.submit(v.byName(t, "spend-tombstone").beef(t), ""); got == nil || len(got) != 0 {
		t.Fatal(got)
	}
	questions := []map[string]string{
		{"office": v.Office, "to": bob.hex},
		{"office": v.Office, "to": bob.hex, "box": "inbox"},
		{"office": v.Office, "to": bob.hex, "from": alice.hex},
		{"office": v.Office, "history": bob.hex},
		{"office": v.Office, "by": bob.hex, "receiptFor": idOf(e2)},
		{"office": v.Office, "spent": idOf(aTree) + ".2"},
		{"office": v.Office, "spent": v.SweepRetractsOutpoints[0]},
	}
	asked := func(ls *Service) []string {
		var out []string
		for _, q := range questions {
			got, err := lookupTxids(ls, nil, q)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(got)
			out = append(out, string(b))
		}
		return out
	}
	live := asked(s.ls)
	lowest := byC([]*transaction.Transaction{e1, e1b})[0]
	if live[0] != `["`+idOf(lowest)+`"]` || live[3] != `["`+idOf(e2)+`"]` || live[6] != `["`+v.byName(t, "sweep").Txid+`"]` {
		t.Fatal("live answers", live)
	}
	statuses := func(ls *Service) []string {
		var out []string
		for _, x := range []*transaction.Transaction{e1, e1b, e2, e3, rc} {
			out = append(out, ls.Status(cOf(x)))
		}
		return out
	}
	want := statuses(s.ls)
	eq(t, want[2:], []string{StatusAcknowledged, StatusRetracted, StatusHeld}, "statuses")

	rows := s.storage.utxos(topic)
	for _, o := range rows {
		if o.Outpoint.Txid.String() == v.byName(t, "sweep").Txid {
			t.Fatal("the spent tombstone is among the unspent rows")
		}
	}
	reversed := make([]bmodule.Output, len(rows))
	for i := range rows {
		reversed[len(rows)-1-i] = rows[i]
	}
	bare := make([]bmodule.Output, len(rows))
	for i := range rows {
		bare[i] = rows[i]
		bare[i].Beef = nil
	}
	for i, order := range [][]bmodule.Output{rows, reversed, bare, nil} {
		again, err := Mount(newFakeHost(), baseConfig(v.Office), []string{topic}, s.journal, s.chain, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		n, err := again.LS.Restore(context.Background(), order, s.storage)
		if err != nil || n == 0 {
			t.Fatalf("order %d: %d %v", i, n, err)
		}
		eq(t, statuses(again.LS), want, "statuses after a restore")
		eq(t, asked(again.LS), live, "answers after a restore")
	}
}

func TestAnswersHydratedAndDroppedNeverRestored(t *testing.T) {
	n := time.Now().Unix()
	s, v := vectorEngine(t, engineOpts{})
	tree := fundingTree(t, alice, 1, s.chain)
	var many []*transaction.Transaction
	for i := 0; i < 10; i++ {
		rec := envelopeRecord(t, letter{office: v.Office, from: alice, to: bob, box: "inbox", created: u64(n - 60)})
		many = append(many, mintCarrier(t, tree, 0, alice, rec, lockTime(uint32(i)))) //nolint:gosec // test
	}
	for _, x := range many {
		zero(t, s.submitTx(x, ""))
	}
	sorted := byC(many)
	eq(t, s.answer(map[string]string{"office": v.Office, "to": bob.hex}), ids(sorted[0]), "the engine hydrates the one answered carrier")
	// The tenth carrier, the ninth superseded, was dropped: gone from
	// storage once flushed.
	s.ls.Prune(context.Background())
	for _, x := range sorted[9:] {
		if s.storage.find(idOf(x), 0, topic) != nil {
			t.Fatal("a dropped carrier is in storage")
		}
	}
	for _, x := range sorted[:9] {
		if s.storage.find(idOf(x), 0, topic) == nil {
			t.Fatal("a kept carrier is gone")
		}
	}
	// Offered again, a dropped carrier is a duplicate the engine never
	// shows the manager.
	if got := s.submitTx(sorted[9], ""); got == nil || len(got) != 0 {
		t.Fatal(got)
	}
	again, err := Mount(newFakeHost(), baseConfig(v.Office), []string{topic}, s.journal, s.chain, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.LS.Restore(context.Background(), s.storage.utxos(topic), s.storage); err != nil {
		t.Fatal(err)
	}
	if again.LS.Counts().Envelopes != 9 || !again.LS.Dropped(cOf(sorted[9])) {
		t.Fatal(again.LS.Counts())
	}
}
