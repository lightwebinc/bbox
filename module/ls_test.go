package module

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	bmodule "github.com/lightwebinc/bcommon/module"

	"github.com/lightwebinc/bbox/boxrec"
)

// ls_bbox on its own, fed what tm_bbox admits (ls_bbox.test.ts): every
// question class and its order and pages, the answer window, the one
// winner per funding outpoint, the evidence cap, arrival order, retraction,
// retention floors, and the outpoint rows that outlive every drop.

const (
	T0  = int64(1_800_000_000)
	day = int64(86400)
)

var (
	alice = newParty("alice")
	bob   = newParty("bob")
	carol = newParty("carol")
	dave  = newParty("dave")
)

type lsRig struct {
	t       *testing.T
	host    *fakeHost
	ls      *Service
	tm      *TopicManager
	clock   *clock
	journal Journal
}

type lsOpts struct {
	journal       Journal
	priced        []string
	now           int64
	retentionDays int
}

func newLsRig(t *testing.T, chain *testChain, o lsOpts) (*lsRig, error) {
	t.Helper()
	host := newFakeHost()
	cl := &clock{now: o.now}
	if cl.now == 0 {
		cl.now = T0
	}
	if o.journal == nil {
		o.journal = &MemoryJournal{}
	}
	priced := map[string]bool{}
	for _, p := range o.priced {
		priced[p] = true
	}
	ls, err := NewService(Options{Offices: []string{office}, Host: host, Journal: o.journal, Priced: priced, RetentionDays: o.retentionDays, Now: cl.time})
	if err != nil {
		return nil, err
	}
	tm, err := NewTopicManager(topic, host, chain, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &lsRig{t: t, host: host, ls: ls, tm: tm, clock: cl, journal: o.journal}, nil
}

func lsRigOf(t *testing.T, chain *testChain, o lsOpts) *lsRig {
	t.Helper()
	r, err := newLsRig(t, chain, o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// tell is the engine telling the lookup service of an admitted output.
func tell(t *testing.T, ls *Service, raw []byte, vout uint32) {
	t.Helper()
	_, _, txid, err := parseSubject(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := ls.OutputAdmittedByTopic(context.Background(), &bmodule.OutputAdmittedByTopic{Mode: bmodule.AdmitWholeTx, Outpoint: transaction.Outpoint{Txid: txid, Index: vout}, Topic: topic, AtomicBEEF: raw}); err != nil {
		t.Fatal(err)
	}
}

// feed is tm_bbox's verdict on tx, then each admitted output told to
// ls_bbox.
func (r *lsRig) feed(tx *transaction.Transaction) []uint32 {
	r.t.Helper()
	raw := atomicOf(r.t, tx)
	ai, err := r.tm.Admit(context.Background(), raw, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, i := range ai.OutputsToAdmit {
		tell(r.t, r.ls, raw, i)
	}
	return ai.OutputsToAdmit
}

func (r *lsRig) ask(q map[string]string) []string {
	r.t.Helper()
	got, err := lookupTxids(r.ls, nil, q)
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

func (r *lsRig) status(tx *transaction.Transaction) string { return r.ls.Status(cOf(tx)) }

func zero(t *testing.T, got []uint32) {
	t.Helper()
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("admitted %v, want [0]", got)
	}
}

func u64(n int64) uint64 { return uint64(n) } //nolint:gosec // test times are positive

func mintLetter(t *testing.T, tree *transaction.Transaction, vout uint32, from, to *party, box string, created, expires int64, lt uint32) *transaction.Transaction {
	t.Helper()
	return mintCarrier(t, tree, vout, from, envelopeRecord(t, letter{office: office, from: from, to: to, box: box, created: u64(created), expiry: u64(expires)}), lt)
}

func mintReceipt(t *testing.T, tree *transaction.Transaction, vout uint32, by *party, acks []*transaction.Transaction, created int64) *transaction.Transaction {
	t.Helper()
	cs := make([][32]byte, 0, len(acks))
	for _, a := range acks {
		cs = append(cs, commitment(a))
	}
	return mintCarrier(t, tree, vout, by, receiptRecord(t, office, by, cs, u64(created)), lockTime(0))
}

func after(created int64, tx *transaction.Transaction) string {
	return fmt.Sprintf("%d:%s", created, idOf(tx))
}

func TestEveryFreeClassAndHistory(t *testing.T) {
	chain := newTestChain(7000)
	r := lsRigOf(t, chain, lsOpts{})
	aTree := fundingTree(t, alice, 10, chain)
	cTree := fundingTree(t, carol, 2, chain)
	bTree := fundingTree(t, bob, 2, chain)
	e1 := mintLetter(t, aTree, 0, alice, bob, "inbox", T0-300, 0, lockTime(0))
	e2 := mintLetter(t, aTree, 1, alice, bob, "files", T0-200, 0, lockTime(0))
	e3 := mintLetter(t, cTree, 0, carol, bob, "inbox", T0-100, 0, lockTime(0))
	e4 := mintLetter(t, aTree, 2, alice, dave, "inbox", T0-50, 0, lockTime(0))
	expired := mintLetter(t, aTree, 3, alice, bob, "inbox", T0-400, T0-1, lockTime(0))
	old := mintLetter(t, aTree, 4, alice, bob, "inbox", T0-WindowBefore-1, 0, lockTime(0))
	ahead := mintLetter(t, aTree, 5, alice, bob, "inbox", T0+3601, 0, lockTime(0))
	for _, e := range []*transaction.Transaction{e1, e2, e3, e4, expired, old, ahead} {
		zero(t, r.feed(e))
	}
	to := bob.hex
	eq(t, r.ask(map[string]string{"office": office, "to": to}), ids(e1, e2, e3), "inbox")
	eq(t, r.ask(map[string]string{"office": office, "to": to, "box": "inbox"}), ids(e1, e3), "box")
	eq(t, r.ask(map[string]string{"office": office, "to": to, "from": alice.hex}), ids(e1, e2), "sender")
	eq(t, r.ask(map[string]string{"office": office, "to": to, "after": after(T0-300, e1)}), ids(e2, e3), "inbox-after")
	eq(t, r.ask(map[string]string{"office": office, "to": to, "box": "inbox", "after": after(T0-300, e1)}), ids(e3), "box-after")
	eq(t, r.ask(map[string]string{"office": office, "to": to, "from": alice.hex, "after": fmt.Sprintf("%d:%s", T0-300, strings.Repeat("0", 64))}), ids(e1, e2), "sender-after")
	eq(t, r.ask(map[string]string{"office": office, "to": dave.hex}), ids(e4), "inbox of another key")
	// Expired and out of the window: history; dated ahead: nothing yet.
	eq(t, r.ask(map[string]string{"office": office, "history": to}), ids(old, expired), "history")
	eq(t, r.ask(map[string]string{"office": office, "history": to, "after": after(T0-WindowBefore-1, old)}), ids(expired), "history-after")

	// Bob acknowledges e1 and e2: out of the inbox, into history, and the
	// receipt answers for each.
	rc := mintReceipt(t, bTree, 0, bob, []*transaction.Transaction{e1, e2}, T0)
	zero(t, r.feed(rc))
	eq(t, r.ask(map[string]string{"office": office, "to": to}), ids(e3), "inbox after the receipt")
	if s := r.status(e1); s != StatusAcknowledged {
		t.Fatal(s)
	}
	eq(t, r.ask(map[string]string{"office": office, "by": to, "receiptFor": idOf(e1)}), ids(rc), "receipt")
	eq(t, r.ask(map[string]string{"office": office, "by": to, "receiptFor": idOf(e3)}), nil, "receipt for another")
	eq(t, r.ask(map[string]string{"office": office, "by": alice.hex, "receiptFor": idOf(e1)}), nil, "receipt by another")
	eq(t, r.ask(map[string]string{"office": office, "history": to}), ids(old, expired, e1, e2), "history with the acknowledged")

	// Alice sweeps e1's funding output: retracted everywhere, and the sweep
	// answers for the outpoint.
	sw := mintSweep(t, aTree, []uint32{0}, alice, chain)
	zero(t, r.feed(sw))
	if s := r.status(e1); s != StatusRetracted {
		t.Fatal(s)
	}
	eq(t, r.ask(map[string]string{"office": office, "history": to}), ids(old, expired, e2), "history without the retracted")
	eq(t, r.ask(map[string]string{"office": office, "spent": idOf(aTree) + ".0"}), ids(sw), "sweep")
	eq(t, r.ask(map[string]string{"office": office, "spent": idOf(aTree) + ".1"}), nil, "no sweep")
	if n := r.host.count("bbox_lookups_total", map[string]string{"class": "history"}); n != 3 {
		t.Fatal(n)
	}
}

func TestPagesOf64(t *testing.T) {
	chain := newTestChain(7100)
	r := lsRigOf(t, chain, lsOpts{})
	tree := fundingTree(t, alice, 70, chain)
	type item struct {
		created int64
		txid    string
	}
	var all []item
	for i := 0; i < 70; i++ {
		created := T0 - 1000 + int64(i/3)
		e := mintLetter(t, tree, uint32(i), alice, bob, "inbox", created, 0, lockTime(0)) //nolint:gosec // test
		r.feed(e)
		all = append(all, item{created, idOf(e)})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].created != all[j].created {
			return all[i].created < all[j].created
		}
		return all[i].txid < all[j].txid
	})
	var want []string
	for _, x := range all {
		want = append(want, x.txid)
	}
	first := r.ask(map[string]string{"office": office, "to": bob.hex})
	eq(t, first, want[:64], "first page")
	rest := r.ask(map[string]string{"office": office, "to": bob.hex, "after": fmt.Sprintf("%d:%s", all[63].created, all[63].txid)})
	eq(t, rest, want[64:], "next page")
}

func TestQuestionsRefusedNeverAnsweredEmpty(t *testing.T) {
	chain := newTestChain(7200)
	r := lsRigOf(t, chain, lsOpts{priced: []string{"history", "history-after"}})
	var qv struct {
		Questions []struct {
			Name     string `json:"name"`
			Question string `json:"question"`
			Class    string `json:"class"`
		} `json:"questions"`
	}
	readVectors(t, "query-v1.json", &qv)
	refused := 0
	for _, x := range qv.Questions {
		q := json.RawMessage(x.Question)
		_, err := r.ls.Lookup(context.Background(), question(q))
		switch {
		case x.Class == "":
			if err == nil || !strings.Contains(err.Error(), "ls_bbox: refused") {
				t.Fatalf("%s: %v", x.Name, err)
			}
			refused++
		case strings.HasPrefix(x.Class, "history"):
			if err == nil || !strings.Contains(err.Error(), "priced on this host") {
				t.Fatalf("%s: %v", x.Name, err)
			}
			c, err := r.ls.Classify(boxrec.LookupService, q)
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.ls.Answer(c, true)
			if err != nil || len(got) != 0 {
				t.Fatalf("%s: %v %v", x.Name, got, err)
			}
		default:
			got, err := lookupTxids(r.ls, nil, q)
			if err != nil || len(got) != 0 {
				t.Fatalf("%s: %v %v", x.Name, got, err)
			}
		}
	}
	if refused < 10 {
		t.Fatalf("%d refused questions", refused)
	}
	for _, c := range []struct {
		q    *struct{ service, query string }
		want string
	}{
		{&struct{ service, query string }{boxrec.LookupService, fmt.Sprintf(`{"office":"other_office_abcdefghij","to":"%s"}`, bob.hex)}, "does not carry office"},
		{&struct{ service, query string }{"ls_log", fmt.Sprintf(`{"office":"%s","to":"%s"}`, office, bob.hex)}, "this service is ls_bbox"},
		{&struct{ service, query string }{boxrec.LookupService, fmt.Sprintf(`["%s"]`, office)}, "not an object"},
		{&struct{ service, query string }{boxrec.LookupService, `null`}, "not an object"},
		{&struct{ service, query string }{boxrec.LookupService, ``}, "not an object"},
	} {
		q := question(json.RawMessage(c.q.query))
		q.Service = c.q.service
		if c.q.query == "" {
			q.Query = nil
		}
		if _, err := r.ls.Lookup(context.Background(), q); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%v: %v", c.q, err)
		}
	}
	// Unpriced, the history classes answer through the engine.
	free := lsRigOf(t, chain, lsOpts{})
	eq(t, free.ask(map[string]string{"office": office, "history": bob.hex}), nil, "unpriced history")
}

func TestOneAnsweredCarrierPerOutpoint(t *testing.T) {
	chain := newTestChain(7300)
	r := lsRigOf(t, chain, lsOpts{})
	tree := fundingTree(t, alice, 1, chain)
	bTree := fundingTree(t, bob, 1, chain)
	cTree := fundingTree(t, carol, 1, chain)
	three := []*transaction.Transaction{
		mintLetter(t, tree, 0, alice, bob, "inbox", T0-30, 0, lockTime(0)),
		mintLetter(t, tree, 0, alice, bob, "inbox", T0-20, 0, lockTime(1)),
		mintLetter(t, tree, 0, alice, bob, "inbox", T0-10, 0, lockTime(2)),
	}
	for _, e := range three {
		zero(t, r.feed(e))
	}
	s := byC(three)
	eq(t, r.ask(map[string]string{"office": office, "to": bob.hex}), ids(s[0]), "only the lowest C is answered")
	statuses := func() string { return strings.Join([]string{r.status(s[0]), r.status(s[1]), r.status(s[2])}, ",") }
	if got := statuses(); got != "held,superseded,superseded" {
		t.Fatal(got)
	}
	// Carol cannot acknowledge envelopes to Bob.
	r.feed(mintReceipt(t, cTree, 0, carol, []*transaction.Transaction{s[2]}, T0))
	if got := statuses(); got != "held,superseded,superseded" {
		t.Fatal(got)
	}
	// Bob acknowledges the highest: it becomes the winner, so a lower C
	// cannot replace a read message.
	r.feed(mintReceipt(t, bTree, 0, bob, []*transaction.Transaction{s[2]}, T0))
	if got := statuses(); got != "superseded,superseded,acknowledged" {
		t.Fatal(got)
	}
	eq(t, r.ask(map[string]string{"office": office, "to": bob.hex}), nil, "inbox")
	eq(t, r.ask(map[string]string{"office": office, "history": bob.hex}), ids(s[2]), "history")
}

func TestEvidenceCap(t *testing.T) {
	chain := newTestChain(7400)
	r := lsRigOf(t, chain, lsOpts{})
	tree := fundingTree(t, alice, 2, chain)
	bTree := fundingTree(t, bob, 1, chain)
	var many []*transaction.Transaction
	for i := 0; i < 12; i++ {
		many = append(many, mintLetter(t, tree, 0, alice, bob, "inbox", T0-100, 0, lockTime(uint32(i)))) //nolint:gosec // test
	}
	for _, e := range many {
		r.feed(e)
	}
	if n := r.host.count("bbox_dropped_total", map[string]string{"why": "evidence"}); n != 3 {
		t.Fatal(n)
	}
	sorted := byC(many)
	for i, e := range sorted {
		if want := i >= 9; r.ls.Dropped(cOf(e)) != want {
			t.Fatalf("carrier %d: dropped %v, want %v (the highest commitments go)", i, !want, want)
		}
	}
	if n := r.ls.Counts().Envelopes; n != 9 {
		t.Fatal(n)
	}
	// With the winner acknowledged, 64 are kept.
	var more []*transaction.Transaction
	for i := 0; i < 12; i++ {
		more = append(more, mintLetter(t, tree, 1, alice, bob, "files", T0-100, 0, lockTime(uint32(i)))) //nolint:gosec // test
	}
	winner := byC(more)[0]
	r.feed(mintReceipt(t, bTree, 0, bob, []*transaction.Transaction{winner}, T0))
	// The acknowledged winner first: until it arrives the outpoint's winner
	// is not acknowledged, the cap is 8, and a ninth superseded carrier
	// arriving before it would be dropped for good.
	r.feed(winner)
	for _, e := range more {
		if e != winner {
			r.feed(e)
		}
	}
	if n := r.host.count("bbox_dropped_total", map[string]string{"why": "evidence"}); n != 3 {
		t.Fatal(n)
	}
	if s := r.status(winner); s != StatusAcknowledged {
		t.Fatal(s)
	}
}

// shuffle is a seeded shuffle, the TypeScript test's, so a failing order
// can be reproduced.
func shuffle(xs []*transaction.Transaction, seed int64) []*transaction.Transaction {
	out := append([]*transaction.Transaction(nil), xs...)
	s := seed
	for i := len(out) - 1; i > 0; i-- {
		s = (s*1103515245 + 12345) % 2147483648
		j := s % int64(i+1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func TestStatusesWhateverTheArrivalOrder(t *testing.T) {
	chain := newTestChain(7500)
	aTree := fundingTree(t, alice, 4, chain)
	bTree := fundingTree(t, bob, 2, chain)
	e1 := mintLetter(t, aTree, 0, alice, bob, "inbox", T0-50, 0, lockTime(0))
	e1b := mintLetter(t, aTree, 0, alice, bob, "inbox", T0-40, 0, lockTime(1))
	e2 := mintLetter(t, aTree, 1, alice, bob, "inbox", T0-30, 0, lockTime(0))
	e3 := mintLetter(t, aTree, 2, alice, bob, "files", T0-20, 0, lockTime(0))
	e4 := mintLetter(t, aTree, 3, alice, bob, "files", T0-10, 0, lockTime(0))
	rc := mintReceipt(t, bTree, 0, bob, []*transaction.Transaction{e1b, e2}, T0)
	sw := mintSweep(t, aTree, []uint32{2}, alice, chain)
	objects := []*transaction.Transaction{e1, e1b, e2, e3, e4, rc, sw}
	snapshot := func(r *lsRig) string {
		var st []string
		for _, x := range objects[:6] {
			st = append(st, r.status(x))
		}
		b, _ := json.Marshal(map[string]any{
			"statuses": st,
			"inbox":    r.ask(map[string]string{"office": office, "to": bob.hex}),
			"history":  r.ask(map[string]string{"office": office, "history": bob.hex}),
			"receipt":  r.ask(map[string]string{"office": office, "by": bob.hex, "receiptFor": idOf(e2)}),
			"sweep":    r.ask(map[string]string{"office": office, "spent": idOf(aTree) + ".2"}),
		})
		return string(b)
	}
	base := lsRigOf(t, chain, lsOpts{})
	for _, x := range objects {
		base.feed(x)
	}
	want := snapshot(base)
	var got struct{ Statuses []string }
	_ = json.Unmarshal([]byte(want), &got)
	if strings.Join(got.Statuses, ",") != "superseded,acknowledged,acknowledged,retracted,held,held" {
		t.Fatal(want)
	}
	rev := append([]*transaction.Transaction(nil), objects...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	orders := [][]*transaction.Transaction{rev, {rc, sw, e4, e3, e2, e1b, e1}}
	for seed := int64(1); seed <= 20; seed++ {
		orders = append(orders, shuffle(objects, seed))
	}
	for _, order := range orders {
		r := lsRigOf(t, chain, lsOpts{})
		var idx []string
		for _, x := range order {
			r.feed(x)
			for i, o := range objects {
				if o == x {
					idx = append(idx, fmt.Sprint(i))
				}
			}
		}
		if s := snapshot(r); s != want {
			t.Fatalf("order %s:\n got  %s\n want %s", strings.Join(idx, ","), s, want)
		}
	}
}

func TestRetractedReceiptAcknowledgesNothing(t *testing.T) {
	chain := newTestChain(7600)
	r := lsRigOf(t, chain, lsOpts{})
	aTree := fundingTree(t, alice, 1, chain)
	bTree := fundingTree(t, bob, 1, chain)
	e := mintLetter(t, aTree, 0, alice, bob, "inbox", T0-60, 0, lockTime(0))
	rc := mintReceipt(t, bTree, 0, bob, []*transaction.Transaction{e}, T0)
	r.feed(e)
	r.feed(rc)
	eq(t, r.ask(map[string]string{"office": office, "to": bob.hex}), nil, "acknowledged")
	r.feed(mintSweep(t, bTree, []uint32{0}, bob, chain))
	if r.status(rc) != StatusRetracted || r.status(e) != StatusHeld {
		t.Fatal(r.status(rc), r.status(e))
	}
	eq(t, r.ask(map[string]string{"office": office, "to": bob.hex}), ids(e), "open again")
	eq(t, r.ask(map[string]string{"office": office, "by": bob.hex, "receiptFor": idOf(e)}), nil, "no receipt")
}

func TestRetentionFloorsAndRowsOutliveDrops(t *testing.T) {
	chain := newTestChain(7700)
	journal := &MemoryJournal{}
	r := lsRigOf(t, chain, lsOpts{journal: journal})
	aTree := fundingTree(t, alice, 4, chain)
	bTree := fundingTree(t, bob, 1, chain)
	read := mintLetter(t, aTree, 0, alice, bob, "inbox", T0-60, 0, lockTime(0))
	unread := mintLetter(t, aTree, 1, alice, bob, "inbox", T0-60, 0, lockTime(0))
	rc := mintReceipt(t, bTree, 0, bob, []*transaction.Transaction{read}, T0+10*365*day)
	sw := mintSweep(t, aTree, []uint32{2}, alice, chain)
	for _, x := range []*transaction.Transaction{read, unread, rc, sw} {
		r.feed(x)
	}
	held := func(ls *Service) int { c := ls.Counts(); return c.Envelopes + c.Receipts + c.Sweeps }

	// A receipt's claimed created, ten years ahead, pins nothing: the floor
	// runs from first sight.
	r.clock.set(T0 + 31*day - 1)
	if n := r.ls.Prune(context.Background()); n != 0 {
		t.Fatalf("pruned %d a second before the floor", n)
	}
	if held(r.ls) != 4 {
		t.Fatal(held(r.ls))
	}
	hist := ids(read, unread)
	sort.Strings(hist)
	eq(t, r.ask(map[string]string{"office": office, "history": bob.hex}), hist, "history")
	r.clock.set(T0 + 31*day)
	if n := r.ls.Prune(context.Background()); n != 4 {
		t.Fatalf("pruned %d: the receipt, the sweep and both envelopes, which the window has left", n)
	}
	if held(r.ls) != 0 || r.host.count("bbox_dropped_total", map[string]string{"why": "retention"}) != 4 {
		t.Fatal(held(r.ls), r.host.counters)
	}

	// Postage is spent for the host's life: a new carrier on a dropped
	// winner's outpoint is superseded, one on a swept outpoint retracted,
	// and a receipt dropped by retention still makes its envelope the
	// winner.
	now := T0 + 31*day
	again := mintLetter(t, aTree, 0, alice, bob, "inbox", now-10, 0, lockTime(5))
	swept := mintLetter(t, aTree, 2, alice, bob, "inbox", now-10, 0, lockTime(0))
	fresh := mintLetter(t, aTree, 3, alice, bob, "inbox", now-10, 0, lockTime(0))
	for _, x := range []*transaction.Transaction{again, swept, fresh} {
		zero(t, r.feed(x))
	}
	if r.status(again) != StatusSuperseded || r.status(swept) != StatusRetracted {
		t.Fatal(r.status(again), r.status(swept))
	}
	eq(t, r.ask(map[string]string{"office": office, "to": bob.hex}), ids(fresh), "inbox")

	// The same from the journal alone, as after a restart.
	again2 := lsRigOf(t, chain, lsOpts{journal: journal, now: now})
	for _, x := range []*transaction.Transaction{again, swept, fresh} {
		tell(t, again2.ls, atomicOf(t, x), 0)
	}
	if again2.status(again) != StatusSuperseded || again2.status(swept) != StatusRetracted {
		t.Fatal(again2.status(again), again2.status(swept))
	}
	if s := again2.status(read); s != StatusSuperseded {
		t.Fatalf("the dropped winner closed its outpoint: %s", s)
	}
	eq(t, again2.ask(map[string]string{"office": office, "to": bob.hex}), ids(fresh), "inbox from the journal")

	// An open envelope is kept past any retention.
	open := lsRigOf(t, chain, lsOpts{now: T0})
	kept := mintLetter(t, fundingTree(t, carol, 1, chain), 0, carol, bob, "inbox", T0+20*day, 0, lockTime(0))
	open.feed(kept)
	open.clock.set(T0 + 40*day)
	if n := open.ls.Prune(context.Background()); n != 0 {
		t.Fatal(n)
	}
	eq(t, open.ask(map[string]string{"office": office, "to": bob.hex}), ids(kept), "kept")
}

func TestRetentionFloorAndFileJournal(t *testing.T) {
	if _, err := newLsRig(t, newTestChain(7800), lsOpts{retentionDays: 30}); err == nil || !strings.Contains(err.Error(), "floor") {
		t.Fatal(err)
	}
	dir := t.TempDir()
	j, err := NewFileJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	op := strings.Repeat("aa", 32) + ".0"
	if err := j.Append(Line{K: "s", Op: op, S: strings.Repeat("bb", 32), Office: office, H: i64(9), At: i64(1)}); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	body, _ := os.ReadFile(j.Path)
	if err := os.WriteFile(j.Path, append(body, []byte(`{"k":"s","op"`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	k, _ := NewFileJournal(dir)
	if lines, err := k.Load(); err != nil || len(lines) != 1 {
		t.Fatalf("the torn line is ignored: %v %v", lines, err)
	}
	if err := k.Append(Line{K: "d", Op: op, C: strings.Repeat("cc", 32), W: boolp(true)}); err != nil {
		t.Fatal(err)
	}
	_ = k.Close()
	if lines, err := (&FileJournal{Path: filepath.Join(dir, JournalFile)}).Load(); err != nil || len(lines) != 2 {
		t.Fatalf("and the next line starts on its own: %v %v", lines, err)
	}
	body, _ = os.ReadFile(j.Path)
	if err := os.WriteFile(j.Path, append([]byte("garbage\n"), body...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&FileJournal{Path: filepath.Join(dir, JournalFile)}).Load(); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatal(err)
	}
	if err := os.WriteFile(j.Path, []byte(`{"k":"c","op":"x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&FileJournal{Path: filepath.Join(dir, JournalFile)}).Load(); err == nil || !strings.Contains(err.Error(), "not an outpoint row") {
		t.Fatal(err)
	}
}

// parseSubject is an Atomic BEEF's subject and txid.
func parseSubject(raw []byte) (*transaction.Beef, *transaction.Transaction, [32]byte, error) {
	beef, txid, err := transaction.NewBeefFromAtomicBytes(raw)
	if err != nil {
		return nil, nil, [32]byte{}, err
	}
	tx := beef.FindAtomicTransactionByHash(txid)
	if tx == nil {
		return nil, nil, [32]byte{}, errors.New("no subject")
	}
	return beef, tx, *txid, nil
}

var _ = time.Second
