package boxrec

import (
	"bytes"
	"context"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/carrier"
)

// TestPushDropScriptMatchesSDK: the rebuilt script is byte for byte what
// bcommon pushdrop writes through the wallet, for a field on each
// push-opcode boundary and past 65535 bytes, signed and unsigned.
func TestPushDropScriptMatchesSDK(t *testing.T) {
	priv, _ := ec.PrivateKeyFromBytes(bytes.Repeat([]byte{0x42}, 32))
	w, err := wallet.NewCompletedProtoWallet(priv)
	if err != nil {
		t.Fatal(err)
	}
	key, err := EnvelopeDerivation.ExpectedLockingKey(priv.PubKey())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{3, 75, 76, 255, 256, 65535, 65536, 70000} {
		f := bytes.Repeat([]byte{0xa5}, n)
		for _, sign := range []bool{false, true} {
			s, err := EnvelopeDerivation.Lock(context.Background(), w, "x", [][]byte{f}, sign)
			if err != nil {
				t.Fatal(err)
			}
			var sig []byte
			if sign {
				fields, ok := pushFields(*s)
				if !ok || len(fields) != 2 {
					t.Fatal("read back")
				}
				sig = fields[1]
			}
			if !bytes.Equal(*s, PushDropScript(key, [][]byte{f}, sig)) {
				t.Errorf("field of %d bytes, signed %v: rebuilt script differs", n, sign)
			}
		}
	}
	lock, err := carrier.FundingLock(context.Background(), w, "x", Params())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(*lock, FundingScript(key)) || !IsFundingShape(*lock) {
		t.Fatal("bcommon's funding lock is not the funding script")
	}
	if k, ok := carrier.DecodeFunding(lock, TagFunding); !ok || !k.IsEqual(key) {
		t.Fatal("bcommon does not read the funding script back")
	}
}

// TestIsFundingShape: only the exact funding script under a canonical key.
func TestIsFundingShape(t *testing.T) {
	priv, _ := ec.PrivateKeyFromBytes(bytes.Repeat([]byte{0x42}, 32))
	f := FundingScript(priv.PubKey())
	if !IsFundingShape(f) {
		t.Fatal("funding script refused")
	}
	alias := append([]byte{0x02}, bytes.Repeat([]byte{0xff}, 32)...)
	bad := [][]byte{
		f[:len(f)-1],
		append(append([]byte{}, f...), script.OpNOP),
		append(append(append([]byte{}, f[:34]...), 0xac, 0x03, 'b', 'b', 0x03), 0x75),
		append(append([]byte{0x21}, alias...), f[34:]...),
	}
	for i, s := range bad {
		if IsFundingShape(s) {
			t.Errorf("case %d: %x taken as funding", i, s)
		}
	}
}

// TestStrictSignatureMatchesBcommon: a signature is strict here exactly
// when bcommon's CheckUnlocking takes it with the sighash byte after it.
func TestStrictSignatureMatchesBcommon(t *testing.T) {
	priv, _ := ec.PrivateKeyFromBytes(bytes.Repeat([]byte{0x42}, 32))
	sig, _ := priv.Sign(bytes.Repeat([]byte{1}, 32))
	der := sig.Serialize()
	cases := [][]byte{der, append([]byte{}, der[:len(der)-1]...), append([]byte{0x31}, der[1:]...), {0x30, 0x06, 0x02, 0x01, 0x00, 0x02, 0x01, 0x01}}
	for i, d := range cases {
		tx := transaction.NewTransaction()
		u := script.Script(append([]byte{byte(len(d) + 1)}, append(append([]byte{}, d...), 0x41)...))
		tx.AddInput(&transaction.TransactionInput{SourceTXID: &chainhash.Hash{}, UnlockingScript: &u})
		if StrictSignature(d) != (carrier.CheckUnlocking(tx) == nil) {
			t.Errorf("case %d: strict %v, bcommon %v", i, StrictSignature(d), carrier.CheckUnlocking(tx))
		}
	}
}

// TestMinimalPath: a two-leaf proof is minimal; a sibling at the wrong
// offset, a leaf too many, or a second txid is not.
func TestMinimalPath(t *testing.T) {
	txid, sib := chainhash.Hash{1}, chainhash.Hash{2}
	yes := true
	mp := func(levels ...[]*transaction.PathElement) *transaction.MerklePath {
		return transaction.NewMerklePath(1, levels)
	}
	at := &transaction.PathElement{Offset: 2, Hash: &txid, Txid: &yes}
	cases := []struct {
		name string
		mp   *transaction.MerklePath
		ok   bool
	}{
		{"minimal", mp([]*transaction.PathElement{at, {Offset: 3, Hash: &sib}}, []*transaction.PathElement{{Offset: 0, Hash: &sib}}), true},
		{"duplicate sibling", mp([]*transaction.PathElement{at, {Offset: 3, Duplicate: &yes}}, []*transaction.PathElement{{Offset: 0, Hash: &sib}}), true},
		{"wrong offset", mp([]*transaction.PathElement{at, {Offset: 1, Hash: &sib}}, []*transaction.PathElement{{Offset: 0, Hash: &sib}}), false},
		{"extra leaf", mp([]*transaction.PathElement{{Offset: 0, Hash: &sib}, at, {Offset: 3, Hash: &sib}}, []*transaction.PathElement{{Offset: 0, Hash: &sib}}), false},
		{"extra upper leaf", mp([]*transaction.PathElement{at, {Offset: 3, Hash: &sib}}, []*transaction.PathElement{{Offset: 0, Hash: &sib}, {Offset: 1, Hash: &sib}}), false},
		{"sibling flagged txid", mp([]*transaction.PathElement{at, {Offset: 3, Hash: &sib, Txid: &yes}}, []*transaction.PathElement{{Offset: 0, Hash: &sib}}), false},
		{"another txid", mp([]*transaction.PathElement{{Offset: 2, Hash: &sib, Txid: &yes}, {Offset: 3, Hash: &txid}}, []*transaction.PathElement{{Offset: 0, Hash: &sib}}), false},
	}
	for _, c := range cases {
		if got := minimalPath(c.mp, txid); got != c.ok {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// TestAdmitHostAndCoins: a host office that breaks the grammar is a
// configuration error, a previous coin outside the inputs is an error, and
// bytes that are no BEEF are refused as beef, never a crash.
func TestAdmitHostAndCoins(t *testing.T) {
	ctx := context.Background()
	if _, err := Admit(ctx, nil, nil, Host{Office: "example_office"}); err == nil || Reason(err) != "other" {
		t.Fatalf("a host without a suffix admitted: %v", err)
	}
	h := Host{Office: "example_office_qzxkvbmwtr"}
	for _, b := range [][]byte{nil, {0x01, 0x00, 0xbe, 0xef}, bytes.Repeat([]byte{0xff}, 64)} {
		if _, err := Admit(ctx, b, nil, h); Reason(err) != "beef" {
			t.Errorf("%x: %v", b, err)
		}
	}
	parent := transaction.NewTransaction()
	parent.AddInput(&transaction.TransactionInput{SourceTXID: &chainhash.Hash{7}, UnlockingScript: &script.Script{}})
	parent.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &script.Script{script.OpTRUE}})
	parent.MerklePath, _ = transaction.NewMerklePathFromCoinbaseTxid(parent.TxID(), 1)
	tx := transaction.NewTransaction()
	tx.AddInputFromTx(parent, 0, nil)
	tx.Inputs[0].UnlockingScript = &script.Script{}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: &script.Script{script.OpTRUE}})
	beef, err := tx.BEEF()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Admit(ctx, beef, []int{1}, h); err == nil {
		t.Fatal("a previous coin past the inputs was taken")
	}
	if _, err := Admit(ctx, beef, nil, h); Reason(err) != "not-bbox" {
		t.Fatalf("a stranger's transaction: %v", err)
	}
	a, err := Admit(ctx, beef, []int{0, 0}, h)
	if err != nil || a.Kind != TxSpend || len(a.Retain) != 1 || len(a.Outputs) != 0 {
		t.Fatalf("a spend of a held output: %+v %v", a, err)
	}
	big := make([]byte, DefaultMaxBEEF+1)
	copy(big, beef)
	if _, err := Admit(ctx, big, nil, h); Reason(err) != "beef" {
		t.Fatalf("over the bound: %v", err)
	}
}
