package reader

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bbox/internal/boxrec"
)

// Recipient is what a recipient's wallet does for the checks of spec
// section 4.7: decrypt, and derive the key a payment pays.
type Recipient interface {
	boxrec.Decrypter
	boxrec.KeyGetter
}

var _ Recipient = (wallet.Interface)(nil)

// Message is what a recipient reads from one verified envelope.
type Message struct {
	Item *Item
	// Plain is the plaintext when checks 1 to 3 passed; Err the first of
	// them that refused the message (undecryptable, plaintext-json,
	// plaintext-shape).
	Plain *boxrec.Plaintext
	Err   error
	// Payment is the payment transaction when the plaintext carries one
	// and it passed checks 4 to 9, which is what the wallet internalizes;
	// PayErr the first of them that refused the payment alone.
	Payment *transaction.Transaction
	PayErr  error
}

// Paid is the satoshis the payment's listed outputs hold.
func (m *Message) Paid() uint64 {
	if m.Plain == nil || m.Plain.Payment == nil {
		return 0
	}
	var n uint64
	for _, o := range m.Plain.Payment.Outputs {
		n += o.Satoshis
	}
	return n
}

// Open applies the recipient's checks of spec section 4.7 in order to an
// envelope that verified, at the recipient's time now: decrypt through w
// (undecryptable), the plaintext's JSON and members (plaintext-json,
// plaintext-shape), then, only when it carries a payment, the payment's
// shape, timing, BEEF, outputs and SPV against the recipient's headers.
// Checks 1 to 3 refuse the message; 4 to 9 refuse only the payment.
func Open(ctx context.Context, w Recipient, originator string, it *Item, headers chaintracker.ChainTracker, now time.Time) *Message {
	m := &Message{Item: it}
	c := it.Admission.Carrier
	if c == nil || c.Envelope == nil || c.Content == nil {
		m.Err = errors.New("reader: not an envelope")
		return m
	}
	plain, err := boxrec.Open(ctx, w, originator, c.Content, c.Envelope.From)
	if err != nil {
		m.Err = err
		return m
	}
	p, err := boxrec.ParsePlaintext(plain)
	switch {
	case errors.Is(err, boxrec.ErrPaymentShape):
		// The message stands; only its payment is refused. Parse it again
		// without the payment so the body and references are shown.
		m.PayErr = err
		doc, _ := boxrec.ParseJSON(plain)
		if o, ok := doc.(*boxrec.Object); ok {
			rest, cerr := boxrec.Canonical(o.Without(boxrec.PlainPayment))
			if cerr == nil {
				if p, err = boxrec.ParsePlaintext(rest); err == nil {
					m.Plain = p
				}
			}
		}
		if m.Plain == nil {
			m.Err = fmt.Errorf("%w: %v", boxrec.ErrPlaintextShape, err)
		}
		return m
	case err != nil:
		m.Err = err
		return m
	}
	m.Plain = p
	if p.Payment == nil {
		return m
	}
	if headers == nil {
		m.PayErr = fmt.Errorf("%w: no header source to verify it against", boxrec.ErrPaymentSPV)
		return m
	}
	t := now.Unix()
	if t < 0 {
		t = 0
	}
	m.Payment, m.PayErr = boxrec.CheckPayment(ctx, w, originator, c.Envelope, p.Payment, headers, uint64(t))
	return m
}
