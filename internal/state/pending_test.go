package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
)

// A state file written before the home recorded trees signed and not yet
// adopted loads as it is, and the records it gains are saved at mode 0600
// and read back.
func TestAStateWithoutTreeRecordsLoads(t *testing.T) {
	dir := t.TempDir()
	id := "02" + strings.Repeat("11", 32)
	old := `{
  "version": 1,
  "identity": "` + id + `",
  "tree": {"identityKey": "` + id + `", "txid": "` + strings.Repeat("22", 32) + `", "rawHex": "00", "sats": 1, "count": 8, "next": 3},
  "trees": [{"identityKey": "` + id + `", "txid": "` + strings.Repeat("22", 32) + `", "rawHex": "00", "sats": 1, "count": 8, "next": 3}],
  "taking": [{"txid": "` + strings.Repeat("33", 32) + `", "vout": 1, "satoshis": 5, "lockingScript": ""}]
}
`
	path := filepath.Join(dir, File)
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if st.Tree == nil || st.Tree.Next != 3 || len(st.Trees) != 1 || len(st.Taking) != 1 || len(st.PendingTrees) != 0 {
		t.Fatalf("an old state: %+v", st)
	}
	// A state with no record writes no such member.
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "pendingTrees") {
		t.Fatalf("a state with no record names them:\n%s", raw)
	}
	a := PendingTree{Tree: funding.Tree{Txid: strings.Repeat("44", 32), RawHex: "01", Count: 4, Sats: 1},
		Coin: bwallet.Output{TxID: strings.Repeat("55", 32), Vout: 2, Satoshis: 9}}
	st.KeepPendingTree(a)
	// A record is keyed by its tree: a second of the same tree replaces it.
	a.ReturnedOnce = true
	st.KeepPendingTree(a)
	st.KeepPendingTree(PendingTree{Tree: funding.Tree{Txid: strings.Repeat("66", 32)}})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the state file's mode: %v %v", fi.Mode(), err)
	}
	again, err := Load(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	got := again.PendingTreeOf(a.Tree.Txid)
	if len(again.PendingTrees) != 2 || got == nil || !got.ReturnedOnce || got.Coin.Outpoint() != a.Coin.Outpoint() || got.Tree.Count != 4 {
		t.Fatalf("the records read back: %+v", again.PendingTrees)
	}
	if !again.DropPendingTree(a.Tree.Txid) || again.DropPendingTree(a.Tree.Txid) || len(again.PendingTrees) != 1 {
		t.Fatalf("after dropping one: %+v", again.PendingTrees)
	}
	// A tree recorded as minted ahead is settled; its record stays.
	again.Ahead = []funding.Tree{{Txid: strings.Repeat("66", 32)}}
	if len(again.UnsettledTrees()) != 0 || len(again.PendingTrees) != 1 {
		t.Fatalf("a tree minted ahead: %+v", again.UnsettledTrees())
	}
}
