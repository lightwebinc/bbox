package send

import (
	"errors"
	"fmt"
	"sort"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// Minimal is mp cut to the proof of txid alone (spec section 8.1 rule 2): at
// the lowest level txid, flagged as the txid, and its sibling; above that the
// one sibling each level needs, computed from the leaves below when mp holds
// only those. A node may answer a proof that carries more of its block than
// one transaction needs, and a host refuses a carrier whose funding tree's
// proof does. The result proves the same root.
func Minimal(mp *transaction.MerklePath, txid *chainhash.Hash) (*transaction.MerklePath, error) {
	if mp == nil || len(mp.Path) == 0 {
		return nil, errors.New("send: no proof")
	}
	index := transaction.IndexedPath(make([]map[uint64]*transaction.PathElement, len(mp.Path)))
	var at *transaction.PathElement
	for l, leaves := range mp.Path {
		index[l] = map[uint64]*transaction.PathElement{}
		for _, e := range leaves {
			index[l][e.Offset] = e
			if l == 0 && e.Hash != nil && *e.Hash == *txid {
				at = e
			}
		}
	}
	if at == nil {
		return nil, fmt.Errorf("send: the proof does not hold %s", txid)
	}
	isTxid := true
	path := make([][]*transaction.PathElement, len(mp.Path))
	path[0] = []*transaction.PathElement{{Offset: at.Offset, Hash: txid, Txid: &isTxid}}
	for l := range mp.Path {
		off := at.Offset>>uint(l) ^ 1
		e := index.GetOffsetLeaf(l, off)
		if e == nil {
			return nil, fmt.Errorf("send: the proof lacks the sibling at level %d", l)
		}
		leaf := &transaction.PathElement{Offset: off}
		if e.Duplicate != nil && *e.Duplicate {
			dup := true
			leaf.Duplicate = &dup
		} else {
			h := *e.Hash
			leaf.Hash = &h
		}
		path[l] = append(path[l], leaf)
	}
	sort.Slice(path[0], func(i, j int) bool { return path[0][i].Offset < path[0][j].Offset })
	out := transaction.NewMerklePath(mp.BlockHeight, path)
	want, err := mp.ComputeRoot(txid)
	if err != nil {
		return nil, err
	}
	got, err := out.ComputeRoot(txid)
	if err != nil || *got != *want {
		return nil, fmt.Errorf("send: the minimal proof of %s does not reach the block's root", txid)
	}
	return out, nil
}
