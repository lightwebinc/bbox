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
