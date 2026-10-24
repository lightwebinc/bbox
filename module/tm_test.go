package module

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	bmodule "github.com/lightwebinc/bcommon/module"

	"github.com/lightwebinc/bbox/boxrec"
)

// tm_bbox on its own (tm_bbox.test.ts): every golden transaction admitted
// or refused through the topic manager exactly as the vectors say, every
// refusal record of refusal-v1.json refused inside a real carrier for the
// reason the vectors give at admission, a BEEF over the host's bound, and
// every refusal label a host counts shown to be reachable.

// verdict is a manager's answer: the instructions, or the reason it
// raised, added to seen.
func verdict(t *testing.T, tm *TopicManager, raw []byte, held []int, seen map[string]bool) (*[2][]uint32, string) {
	t.Helper()
	ai, err := tm.Admit(context.Background(), raw, held)
	if err != nil {
		var r *Refused
		if !errors.As(err, &r) {
			t.Fatalf("not a refusal: %v", err)
		}
		if seen != nil {
			seen[r.Reason] = true
		}
		return nil, r.Reason
	}
	return &[2][]uint32{ai.OutputsToAdmit, ai.CoinsToRetain}, ""
}

func u32s(a []uint32) string {
	var b strings.Builder
	for i, x := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(string(rune('0' + x)))
	}
	return b.String()
}

// everyVector runs every transaction vector through the manager.
func everyVector(t *testing.T, seen map[string]bool) *fakeHost {
	t.Helper()
	v := loadTxVectors(t)
	host := newFakeHost()
	tm, err := NewTopicManager(v.Topic, host, newTestChain(1).withVectors(t, v), 0)
	if err != nil {
		t.Fatal(err)
	}
	if tm.Office != v.Office {
		t.Fatal(tm.Office)
	}
	admitted, refused := 0, 0
	for i := range v.Transactions {
		x := &v.Transactions[i]
		got, reason := verdict(t, tm, x.beef(t), x.held(), seen)
		if x.Verdict == "admit" {
			if got == nil || u32s(got[0]) != u32s(x.Admits.OutputsToAdmit) || u32s(got[1]) != u32s(x.Admits.CoinsToRetain) {
				t.Fatalf("%s: got %v (%s)", x.Name, got, reason)
			}
			admitted++
		} else {
			if reason != x.Reason {
				t.Fatalf("%s: refused %q, want %q", x.Name, reason, x.Reason)
			}
			refused++
		}
	}
	if admitted+refused != len(v.Transactions) || admitted < 9 || refused < 45 {
		t.Fatalf("%d admitted, %d refused", admitted, refused)
	}
	return host
}

// everyRefusalRecord carries each refusal record in an otherwise valid
// carrier.
func everyRefusalRecord(t *testing.T, seen map[string]bool) {
	t.Helper()
	var rv struct {
		Refusals []struct {
			Name            string `json:"name"`
			Record          string `json:"record"`
			AdmissionClaims string `json:"admissionClaims"`
			AdmissionReason string `json:"admissionReason"`
		} `json:"refusals"`
	}
	readVectors(t, "refusal-v1.json", &rv)
	v := loadTxVectors(t)
	chain := newTestChain(6000)
	// The vectors' sender: 32 bytes of 0x42, a public test key.
	sender := partyFromKey("vector sender", bytes.Repeat([]byte{0x42}, 32))
	tree := fundingTree(t, sender, len(rv.Refusals), chain)
	tm, err := NewTopicManager(v.Topic, newFakeHost(), chain, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range rv.Refusals {
		rec, err := hex.DecodeString(r.Record)
		if err != nil {
			t.Fatal(err)
		}
		k := mintCarrier(t, tree, uint32(i), sender, rec, lockTime(0)) //nolint:gosec // a vector index
		_, got := verdict(t, tm, atomicOf(t, k), nil, seen)
		want := r.AdmissionReason
		if r.AdmissionClaims == "none" {
			want = "not-bbox"
		}
		if got != want {
			t.Fatalf("%s: refused %q, want %q", r.Name, got, want)
		}
	}
}

func noteRecord(t *testing.T) []byte {
	t.Helper()
	v := loadTxVectors(t)
	b, err := hex.DecodeString(v.byName(t, "carrier-envelope-note").Admits.Record)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// overTheBound is a carrier whose BEEF is over the host's bound.
func overTheBound(t *testing.T, seen map[string]bool) {
	t.Helper()
	v := loadTxVectors(t)
	chain := newTestChain(6100)
	owner := newParty("big tree owner")
	// 6000 funding outputs of 49 bytes and more: the carrier's BEEF is over 256 KiB.
	tree := fundingTree(t, owner, 6000, chain)
	beef := atomicOf(t, mintCarrier(t, tree, 0, owner, noteRecord(t), lockTime(0)))
	if len(beef) <= boxrec.DefaultMaxBEEF {
		t.Fatal(len(beef))
	}
	tm, _ := NewTopicManager(v.Topic, newFakeHost(), chain, 0)
	if _, r := verdict(t, tm, beef, nil, seen); r != "beef" {
		t.Fatal(r)
	}
	// A host that names a larger bound reads it, and refuses it for what it
	// is: the note's record names the vectors' sender, not this owner.
	roomy, err := NewTopicManager(v.Topic, newFakeHost(), chain, len(beef))
	if err != nil {
		t.Fatal(err)
	}
	if _, r := verdict(t, roomy, beef, nil, seen); r != "lock" {
		t.Fatal(r)
	}
	// Bytes that are no BEEF at all.
	if _, r := verdict(t, tm, []byte{1, 2, 3}, nil, seen); r != "beef" {
		t.Fatal(r)
	}
}

// notBboxAndMineable is a plain payment and a carrier that could be mined.
func notBboxAndMineable(t *testing.T, seen map[string]bool) {
	t.Helper()
	v := loadTxVectors(t)
	chain := newTestChain(6200)
	payer := newParty("payer")
	addr, _ := script.NewAddressFromPublicKey(payer.key.PubKey(), true)
	payLock, _ := p2pkh.Lock(addr)
	coin := chain.mine(t, fromNowhere(&transaction.TransactionOutput{Satoshis: 5000, LockingScript: payLock}))
	tm, _ := NewTopicManager(v.Topic, newFakeHost(), chain, 0)
	unlock, _ := p2pkh.Unlock(payer.key, nil)
	plain := transaction.NewTransaction()
	plain.AddInputFromTx(coin, 0, unlock)
	payeeAddr, _ := script.NewAddressFromPublicKey(newParty("payee").key.PubKey(), true)
	payeeLock, _ := p2pkh.Lock(payeeAddr)
	plain.AddOutput(&transaction.TransactionOutput{Satoshis: 4000, LockingScript: payeeLock})
	if err := plain.Sign(); err != nil {
		t.Fatal(err)
	}
	if _, r := verdict(t, tm, atomicOf(t, plain), nil, seen); r != "not-bbox" {
		t.Fatal(r)
	}
	owner := newParty("mineable owner")
	mineable := mintCarrier(t, fundingTree(t, owner, 1, chain), 0, owner, noteRecord(t), lockTime(0)-1)
	if _, r := verdict(t, tm, atomicOf(t, mineable), nil, seen); r != "mineable" {
		t.Fatal(r)
	}
}

func TestEveryTransactionVector(t *testing.T) {
	host := everyVector(t, map[string]bool{})
	// Five envelope admissions, one of them a txid admitted before in
	// another BEEF: counted once, and once as a repeat.
	for _, c := range []struct {
		name, kind string
		want       float64
	}{{"bbox_admitted_total", "envelope", 4}, {"bbox_admitted_repeats_total", "envelope", 1}, {"bbox_admitted_total", "sweep", 2}} {
		if n := host.count(c.name, map[string]string{"kind": c.kind}); n != c.want {
			t.Fatalf("%s{%s} = %v, want %v", c.name, c.kind, n, c.want)
		}
	}
	if n := host.count("bbox_refused_total", map[string]string{"reason": "unlock"}); n != 6 {
		t.Fatal(n)
	}
}

func TestEveryRefusalRecordInACarrier(t *testing.T) { everyRefusalRecord(t, nil) }

func TestBEEFOverTheBound(t *testing.T) { overTheBound(t, nil) }

func TestNotBboxAndMineable(t *testing.T) { notBboxAndMineable(t, nil) }

func TestEveryRefusalLabelIsReachable(t *testing.T) {
	seen := map[string]bool{}
	everyVector(t, seen)
	everyRefusalRecord(t, seen)
	overTheBound(t, seen)
	notBboxAndMineable(t, seen)
	var missing []string
	for _, r := range Reasons {
		if !seen[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("never seen: %v", missing)
	}
}

func TestManagerNeedsNoInputsAndDocumentsItself(t *testing.T) {
	v := loadTxVectors(t)
	tm, err := NewTopicManager(v.Topic, newFakeHost(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(tm).(bmodule.InputIdentifier); ok {
		t.Fatal("the manager names inputs")
	}
	if !strings.Contains(tm.GetDocumentation(), "raised") {
		t.Fatal(tm.GetDocumentation())
	}
	if md := tm.GetMetaData(); md.Name != v.Topic || md.Description != "Admits the bbox office "+v.Office+"." {
		t.Fatal(md)
	}
	if _, err := NewTopicManager("tm_log_x_abcdefghij", newFakeHost(), nil, 0); err == nil {
		t.Fatal("not a bbox topic")
	}
	if _, err := NewTopicManager(v.Topic, newFakeHost(), nil, 1000); err == nil {
		t.Fatal("a bound under the floor")
	}
	// The engine's seam: the manager decides on the submission's bytes.
	if _, err := tm.IdentifyAdmissibleOutputs(context.Background(), nil, nil, nil); !errors.Is(err, ErrNoSubmission) {
		t.Fatal(err)
	}
}

func TestAdmissionCountedOncePerTransaction(t *testing.T) {
	v := loadTxVectors(t)
	host := newFakeHost()
	tm, _ := NewTopicManager(v.Topic, host, newTestChain(1).withVectors(t, v), 0)
	var sweep *txVector
	for i := range v.Transactions {
		if x := &v.Transactions[i]; x.Verdict == "admit" && strings.Contains(x.Name, "sweep") {
			sweep = x
			break
		}
	}
	if sweep == nil {
		t.Fatal("a sweep vector")
	}
	for i := 0; i < 3; i++ {
		verdict(t, tm, sweep.beef(t), sweep.held(), nil)
	}
	if host.count("bbox_admitted_total", map[string]string{"kind": "sweep"}) != 1 || host.count("bbox_admitted_repeats_total", map[string]string{"kind": "sweep"}) != 2 {
		t.Fatal(host.counters)
	}
}
