package purse

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"

	"github.com/lightwebinc/bbox/internal/send"
	"github.com/lightwebinc/bbox/internal/testchain"
)

type rig struct {
	chain *testchain.Chain
	asset *nodeapi.Asset
	rpc   *nodeapi.RPC
}

func newRig(t *testing.T) *rig {
	t.Helper()
	c := testchain.New(700)
	s := httptest.NewServer(c)
	t.Cleanup(s.Close)
	return &rig{chain: c, asset: &nodeapi.Asset{Base: s.URL}, rpc: &nodeapi.RPC{URL: s.URL + "/rpc", ID: "t"}}
}

// purse is a funded home as a Purse.
func (r *rig) purse(t *testing.T, maxPay uint64) *Purse {
	t.Helper()
	e, err := bwallet.Create(t.TempDir(), send.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := bwallet.FundFromCoinbase(context.Background(), e.Signer(), e.Pool, r.rpc, r.asset, 102, 0); err != nil {
		t.Fatal(err)
	}
	tip := r.chain.Height()
	settler := &publish.RPCSettler{RPC: r.rpc}
	s := e.Signer()
	return &Purse{Embedded: e, Fees: mint.DefaultFees, MaxPay: maxPay, Settler: settler, Asset: r.asset, Headers: r.chain,
		Wait: 5 * time.Second, Poll: 5 * time.Millisecond,
		NewPayer: func() *producer.Payer {
			return &producer.Payer{Pool: e.Pool, Tip: tip, Keys: map[string]*bwallet.Signer{s.IdentityHex(): s}, Settler: settler, Asset: r.asset, Fees: mint.DefaultFees}
		}}
}

func p2pkhTo(t *testing.T, p *Purse) []byte {
	t.Helper()
	s, err := p.Signer().FundScript()
	if err != nil {
		t.Fatal(err)
	}
	return *s
}

func TestCreateActionPaysOneOutputUpToItsCap(t *testing.T) {
	r := newRig(t)
	p := r.purse(t, 100)
	ctx := context.Background()
	lock := p2pkhTo(t, r.purse(t, 0))
	for name, args := range map[string]wallet.CreateActionArgs{
		"two outputs":   {Outputs: []wallet.CreateActionOutput{{Satoshis: 5, LockingScript: lock}, {Satoshis: 5, LockingScript: lock}}},
		"nothing":       {Outputs: []wallet.CreateActionOutput{{Satoshis: 0, LockingScript: lock}}},
		"over the cap":  {Outputs: []wallet.CreateActionOutput{{Satoshis: 101, LockingScript: lock}}},
		"not P2PKH":     {Outputs: []wallet.CreateActionOutput{{Satoshis: 5, LockingScript: []byte{0x51}}}},
		"inputs of its": {Inputs: []wallet.CreateActionInput{{}}, Outputs: []wallet.CreateActionOutput{{Satoshis: 5, LockingScript: lock}}},
	} {
		if _, err := p.CreateAction(ctx, args, ""); err == nil {
			t.Errorf("%s: made", name)
		}
	}
	coins := p.Pool.Count()
	res, err := p.CreateAction(ctx, wallet.CreateActionArgs{Outputs: []wallet.CreateActionOutput{{Satoshis: 7, LockingScript: lock}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, tx, _, err := guard.ParseBEEF(res.Tx, guard.DefaultBound)
	if err != nil || tx.TxID().String() != res.Txid.String() || tx.Outputs[0].Satoshis != 7 || string(*tx.Outputs[0].LockingScript) != string(lock) {
		t.Fatalf("the payment: %v", err)
	}
	if r.chain.Tx(tx.TxID().String()) != nil {
		t.Fatal("CreateAction broadcast the payment; the payee does")
	}
	if p.Pool.Count() != coins-1 {
		t.Fatal("the fee coin is not reserved")
	}
	p.Refund()
	if p.Pool.Count() != coins {
		t.Fatal("Refund did not give the coin back")
	}
	// Two payments, the host accepted the last: the first is refunded, the
	// last kept, its change held until it mines.
	for range 2 {
		if _, err := p.CreateAction(ctx, wallet.CreateActionArgs{Outputs: []wallet.CreateActionOutput{{Satoshis: 7, LockingScript: lock}}}, ""); err != nil {
			t.Fatal(err)
		}
	}
	kept := p.Settle()
	if kept == nil || p.Pool.Count() != coins || len(p.Pool.UnprovenTxids()) != 1 || p.Pool.UnprovenTxids()[0] != kept.TxID().String() {
		t.Fatalf("settle: %d coins (want %d: one spent, its change held), unproven %v", p.Pool.Count(), coins, p.Pool.UnprovenTxids())
	}
}

func TestInternalizeTakesABRC29PaymentIntoThePool(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	payer := r.purse(t, 0)
	payee := r.purse(t, 0)
	prefix, suffix := base64.StdEncoding.EncodeToString([]byte("prefix-1")), base64.StdEncoding.EncodeToString([]byte("suffix-1"))
	dest, err := payer.Signer().PaymentDestination(ctx, payee.Signer().IdentityHex(), prefix, suffix)
	if err != nil {
		t.Fatal(err)
	}
	pp := payer.NewPayer()
	fee, err := pp.TakeAtLeast(ctx, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	change, _ := payer.Signer().FundScript()
	tx, err := mint.Payment(ctx, dest, 4_000, fee, change, mint.DefaultFees)
	if err != nil {
		t.Fatal(err)
	}
	beef, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	remit := func(pre, suf string) []wallet.InternalizeOutput {
		rm, err := Remittance(pre, suf, payer.Signer().IdentityHex())
		if err != nil {
			t.Fatal(err)
		}
		return []wallet.InternalizeOutput{{OutputIndex: 0, Protocol: wallet.InternalizeProtocolWalletPayment, PaymentRemittance: rm}}
	}
	if _, err := payee.InternalizeAction(ctx, wallet.InternalizeActionArgs{Tx: beef, Outputs: remit(prefix, base64.StdEncoding.EncodeToString([]byte("other")))}, ""); err == nil ||
		!strings.Contains(err.Error(), "does not pay the key") {
		t.Fatalf("another suffix: %v", err)
	}
	if _, err := payer.InternalizeAction(ctx, wallet.InternalizeActionArgs{Tx: beef, Outputs: remit(prefix, suffix)}, ""); err == nil {
		t.Fatal("the payer internalized its own payment to the payee")
	}
	if r.chain.Tx(tx.TxID().String()) != nil {
		t.Fatal("a refused internalize broadcast")
	}
	before := payee.Pool.Balance()
	res, err := payee.InternalizeAction(ctx, wallet.InternalizeActionArgs{Tx: beef, Outputs: remit(prefix, suffix)}, "")
	if err != nil || !res.Accepted {
		t.Fatalf("internalize: %v", err)
	}
	if !r.chain.Mined(tx.TxID().String()) || payee.Pool.Balance() != before+4_000 {
		t.Fatalf("mined %v, balance %d (was %d)", r.chain.Mined(tx.TxID().String()), payee.Pool.Balance(), before)
	}
	// Again: already mined, taken as it is, and the pool holds it once.
	if _, err := payee.InternalizeAction(ctx, wallet.InternalizeActionArgs{Tx: beef, Outputs: remit(prefix, suffix)}, ""); err != nil || payee.Pool.Balance() != before+4_000 {
		t.Fatalf("again: %v %d", err, payee.Pool.Balance())
	}
	// The pool spends it: its derivation re-derives the key.
	var derived *bwallet.Output
	for _, o := range payee.Pool.Outputs() {
		if o.TxID == tx.TxID().String() {
			derived = &o
		}
	}
	if derived == nil || derived.Derivation == nil {
		t.Fatal("the payment is not pooled with its derivation")
	}
	in, err := payee.NewPayer().Parent(ctx, *derived)
	if err != nil || in.MerklePath == nil {
		t.Fatalf("its parent with the proof: %v", err)
	}
	spend := transaction.NewTransaction()
	spend.AddInputFromTx(in, derived.Vout, payee.Signer().DerivedUnlocker(derived.Derivation))
	back, _ := payer.Signer().FundScript()
	spend.AddOutput(&transaction.TransactionOutput{Satoshis: 3_000, LockingScript: (*script.Script)(back)})
	if err := spend.Sign(); err != nil {
		t.Fatal(err)
	}
	if err := r.chain.Send(spend); err != nil {
		t.Fatalf("the pool cannot spend what it internalized: %v", err)
	}
}
