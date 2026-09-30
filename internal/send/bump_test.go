package send

import (
	"crypto/sha256"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

func leaf(s string) *chainhash.Hash {
	h := chainhash.Hash(sha256.Sum256([]byte(s)))
	return &h
}

// A node may answer a proof that carries more of its block than one
// transaction needs; the carrier's funding tree must carry only its own.
func TestMinimalCutsAProofToOneTransaction(t *testing.T) {
	tx := leaf("tree")
	yes := true
	h := []*chainhash.Hash{leaf("coinbase"), leaf("other"), tx, leaf("sibling")}
	// A compound path: the whole lowest level, nothing above it.
	full := transaction.NewMerklePath(900, [][]*transaction.PathElement{
		{{Offset: 0, Hash: h[0]}, {Offset: 1, Hash: h[1]}, {Offset: 2, Hash: tx, Txid: &yes}, {Offset: 3, Hash: h[3]}},
		{},
	})
	m, err := Minimal(full, tx)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Path) != 2 || len(m.Path[0]) != 2 || len(m.Path[1]) != 1 || m.Path[1][0].Offset != 0 {
		t.Fatalf("not minimal: %+v", m.Path)
	}
	if at, sib := m.Path[0][0], m.Path[0][1]; at.Offset != 2 || at.Txid == nil || !*at.Txid || *at.Hash != *tx || sib.Offset != 3 || sib.Txid != nil {
		t.Fatalf("the txid leaf and its sibling, in offset order: %+v %+v", at, sib)
	}
	want, _ := full.ComputeRoot(tx)
	got, _ := m.ComputeRoot(tx)
	if *want != *got {
		t.Fatal("another root")
	}
	// An odd last transaction: its sibling is the duplicate flag.
	dup := true
	odd := transaction.NewMerklePath(901, [][]*transaction.PathElement{
		{{Offset: 0, Hash: h[0]}, {Offset: 1, Hash: h[1]}, {Offset: 2, Hash: tx, Txid: &yes}, {Offset: 3, Duplicate: &dup}},
		{{Offset: 0, Hash: transaction.MerkleTreeParent(h[0], h[1])}},
	})
	m, err = Minimal(odd, tx)
	if err != nil || m.Path[0][1].Duplicate == nil || !*m.Path[0][1].Duplicate {
		t.Fatalf("%v %+v", err, m)
	}
	if _, err := Minimal(full, leaf("absent")); err == nil {
		t.Fatal("a proof of another transaction")
	}
}
