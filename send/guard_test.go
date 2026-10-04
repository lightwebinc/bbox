package send

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"

	"github.com/lightwebinc/bbox/internal/state"
)

// A tree minted ahead becomes the current one only once the home has
// recorded it: a switch that could not be saved is an error, and nothing
// is spent from a tree the home does not know is current.
func TestATreeSwitchThatCannotBeSavedIsAnError(t *testing.T) {
	w, err := bwallet.Create(t.TempDir(), Profile)
	if err != nil {
		t.Fatal(err)
	}
	signer := w.Signer()
	// A home that does not exist: nothing can be saved in it.
	st, err := state.Load(filepath.Join(t.TempDir(), "gone"), signer.IdentityHex())
	if err != nil {
		t.Fatal(err)
	}
	st.Ahead = []funding.Tree{{IdentityKeyHex: signer.IdentityHex(), Txid: strings.Repeat("22", 32), Count: 8}}
	e := &Engine{St: st, Signer: signer}
	if err := e.promoteAhead(); err == nil || !strings.Contains(err.Error(), "switching to funding tree") {
		t.Fatalf("a switch that could not be saved: %v", err)
	}
}

// The journal records the pool with every save while a command runs, and
// nothing once it ended.
func TestTheJournalRecordsThePoolWhileACommandRuns(t *testing.T) {
	dir := t.TempDir()
	w, err := bwallet.Create(dir, Profile)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Load(dir, w.Signer().IdentityHex())
	if err != nil {
		t.Fatal(err)
	}
	coin := bwallet.Output{TxID: strings.Repeat("11", 32), Vout: 1, Satoshis: 5}
	if _, err := w.Pool.Add(coin); err != nil {
		t.Fatal(err)
	}
	Journal(st, w.Pool, nil)
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if len(st.Taking) != 1 || st.Taking[0].Outpoint() != coin.Outpoint() {
		t.Fatalf("the journal while a command runs: %+v", st.Taking)
	}
	Unjournal(st)
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if again, err := state.Load(dir, w.Signer().IdentityHex()); err != nil || len(again.Taking) != 0 {
		t.Fatalf("the journal after the command: %v %+v", err, again.Taking)
	}
}

// Adopting a tree drops its record as a tree signed and not yet adopted,
// in the save that records it as adopted, and leaves every other record.
func TestAdoptDropsTheTreesRecord(t *testing.T) {
	dir := t.TempDir()
	w, err := bwallet.Create(dir, Profile)
	if err != nil {
		t.Fatal(err)
	}
	id := w.Signer().IdentityHex()
	st, err := state.Load(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{St: st, Signer: w.Signer()}
	one := funding.Tree{IdentityKeyHex: id, Txid: strings.Repeat("22", 32), Count: 8}
	two := funding.Tree{IdentityKeyHex: id, Txid: strings.Repeat("33", 32), Count: 8}
	coin := bwallet.Output{TxID: strings.Repeat("11", 32), Vout: 1, Satoshis: 5}
	// The record is on disk before the hook returns.
	if err := e.prepareTree(one, coin); err != nil {
		t.Fatal(err)
	}
	if err := e.prepareTree(two, coin); err != nil {
		t.Fatal(err)
	}
	if saved, err := state.Load(dir, id); err != nil || len(saved.PendingTrees) != 2 || saved.PendingTrees[0].Coin.Outpoint() != coin.Outpoint() {
		t.Fatalf("the records the hook saved: %v %+v", err, saved)
	}
	if err := (treeState{e}).Adopt(one); err != nil {
		t.Fatal(err)
	}
	saved, err := state.Load(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Tree == nil || saved.Tree.Txid != one.Txid || len(saved.PendingTrees) != 1 || saved.PendingTrees[0].Tree.Txid != two.Txid {
		t.Fatalf("after adopting one: tree %+v, records %+v", saved.Tree, saved.PendingTrees)
	}
}

// A record that cannot be saved aborts the mint, and is not kept in memory.
func TestARecordThatCannotBeSavedAbortsTheMint(t *testing.T) {
	w, err := bwallet.Create(t.TempDir(), Profile)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Load(filepath.Join(t.TempDir(), "gone"), w.Signer().IdentityHex())
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{St: st, Signer: w.Signer()}
	if err := e.prepareTree(funding.Tree{Txid: strings.Repeat("22", 32)}, bwallet.Output{}); err == nil || len(st.PendingTrees) != 0 {
		t.Fatalf("a record that could not be saved: %v %+v", err, st.PendingTrees)
	}
}
