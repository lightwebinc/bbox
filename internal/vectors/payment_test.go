package vectors

import (
	"errors"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/verify"

	"github.com/lightwebinc/bbox/boxrec"
)

// TestPaymentRefusesNonFinal holds the payment check to go-sdk's spv.Verify:
// a payment must be mineable, so an unmined payment that is not final is
// refused. bcommon verify.Check, the overlay plane's SPV for carriers, has
// no finality rule and admits the same transaction, so a payment checked
// through it would pass here.
func TestPaymentRefusesNonFinal(t *testing.T) {
	var built []*sealed
	for _, s := range specs() {
		b, err := build(s)
		if err != nil {
			t.Fatal(err)
		}
		built = append(built, b)
	}
	e, err := newTxEnv(built, nil)
	if err != nil {
		t.Fatal(err)
	}
	env, err := boxrec.DecodeEnvelope(e.payEnv.record)
	if err != nil {
		t.Fatal(err)
	}
	p := e.pay
	check := func(tx *transaction.Transaction) error {
		beef, err := tx.AtomicBEEF(false)
		if err != nil {
			t.Fatal(err)
		}
		var outs []boxrec.PaymentOutput
		for i, s := range p.suffixes {
			outs = append(outs, boxrec.PaymentOutput{OutputIndex: uint32(i), DerivationSuffix: s, Satoshis: p.sats[i]}) //nolint:gosec // two outputs
		}
		pay := &boxrec.Payment{BEEF: beef, DerivationPrefix: p.prefix, Outputs: outs}
		_, err = boxrec.CheckPayment(e.ctx, e.r.w, originator, env, pay, e.c.headers, t0+360)
		return err
	}

	// The payment as the vectors carry it, final, passes.
	if err := check(p.tx); err != nil {
		t.Fatalf("final payment: %v", err)
	}

	// The same spend, not final: a lock time above the chain tip and an
	// input sequence below the maximum.
	nf := transaction.NewTransaction()
	nf.AddInputFromTx(e.coin, 1, e.s.feeUnlocker())
	for _, o := range p.tx.Outputs {
		nf.AddOutput(&transaction.TransactionOutput{Satoshis: o.Satoshis, LockingScript: o.LockingScript})
	}
	nf.LockTime = e.c.height + 10
	nf.Inputs[0].SequenceNumber = 0
	if err := nf.Sign(); err != nil {
		t.Fatal(err)
	}
	if err := check(nf); !errors.Is(err, boxrec.ErrPaymentSPV) {
		t.Fatalf("non-final unmined payment: %v, want %v", err, boxrec.ErrPaymentSPV)
	}
	// The refusal is finality and nothing else: the overlay plane's SPV,
	// which has no finality rule, admits it.
	if v, err := verify.Check(e.ctx, nf, e.c.headers); v != verify.Passed {
		t.Fatalf("verify.Check of the non-final payment: %v %v, want it admitted", v, err)
	}
}
