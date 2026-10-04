package limits

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/mint"

	"github.com/lightwebinc/bbox/boxrec"
)

// fragment is the object bytes one plane packet carries at the fabric's
// 1280-byte MTU floor, less IPv6, UDP and fragment headers (docs/limits.md).
const fragment = 1128

// depth is the Merkle path depth of the funding tree's block: 2^16 is a
// block of about 65,000 transactions.
const depth = 16

// measure mints a real carrier for an envelope record whose content is
// contentLen bytes, from a mined funding tree of count outputs whose BUMP
// has depth levels, and returns the Atomic BEEF length a sender publishes.
func measure(t *testing.T, contentLen, count int) int {
	t.Helper()
	ctx := context.Background()
	k, _ := ec.PrivateKeyFromBytes(bytes.Repeat([]byte{0x42}, 32))
	w, err := wallet.NewCompletedProtoWallet(k)
	if err != nil {
		t.Fatal(err)
	}
	p := carrier.Params{Derivation: boxrec.EnvelopeDerivation, FundingTag: boxrec.TagFunding,
		ValidatePayload: func([]byte) error { return nil }}
	addr, _ := script.NewAddressFromPublicKey(k.PubKey(), false)
	payTo, _ := p2pkh.Lock(addr)
	coin := transaction.NewTransaction()
	nowhere := chainhash.Hash{0x11}
	coin.AddInput(&transaction.TransactionInput{SourceTXID: &nowhere, UnlockingScript: &script.Script{}, SequenceNumber: transaction.MaxTxInSequenceNum})
	coin.AddOutput(&transaction.TransactionOutput{Satoshis: 10_000_000, LockingScript: payTo})
	payer, _ := p2pkh.Unlock(k, nil)
	lock, err := carrier.FundingLock(ctx, w, "", p)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := mint.FundingTree(lock, count, 1000, mint.Input{Tx: coin, Vout: 0, Unlocker: payer}, payTo, mint.DefaultFees)
	if err != nil {
		t.Fatal(err)
	}
	// A path of depth levels: at each level the sibling, and at level 0
	// the tree itself.
	isTxid := true
	path := make([][]*transaction.PathElement, depth)
	for l := 0; l < depth; l++ {
		sib := chainhash.Hash{byte(l + 1)}
		path[l] = []*transaction.PathElement{{Offset: 1, Hash: &sib}}
	}
	path[0] = append([]*transaction.PathElement{{Offset: 0, Hash: tree.TxID(), Txid: &isTxid}}, path[0]...)
	tree.MerklePath = transaction.NewMerklePath(900_000, path)

	e := &boxrec.Envelope{Office: "example_office_qzxkvbmwtr", To: k.PubKey().Compressed(), Box: "inbox",
		From: k.PubKey().Compressed(), Created: 1767225600, Content: bytes.Repeat([]byte{'x'}, contentLen)}
	rec, err := e.Encode()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := carrier.Mint(ctx, w, "", p, rec, tree, 0)
	if err != nil {
		t.Fatal(err)
	}
	beef, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	return len(beef)
}

// TestCarrierSizes measures envelope carriers on the plane and holds the
// defaults of docs/limits.md to them: with the default 32-output tree, an
// envelope at the full 16 KiB content bound is at most 17 packets, and one
// at the plane's warning threshold of 6 KiB at most 8.
func TestCarrierSizes(t *testing.T) {
	type row struct{ content, tree int }
	var lines []string
	for _, r := range []row{{1024, 32}, {4096, 32}, {6144, 32}, {6144, 100}, {12288, 32}, {16384, 32}, {16384, 100}, {16384, 250}} {
		n := measure(t, r.content, r.tree)
		pk := (n + fragment - 1) / fragment
		lines = append(lines, fmt.Sprintf("content %5d tree %3d: carrier %6d bytes, %2d packets", r.content, r.tree, n, pk))
		switch {
		case r.content == 16384 && r.tree == 32 && pk > 17:
			t.Errorf("full envelope is %d packets", pk)
		case r.content == 6144 && r.tree == 32 && pk > 8:
			t.Errorf("an envelope at the plane warning threshold is %d packets", pk)
		}
	}
	for _, l := range lines {
		t.Log(l)
	}
}
