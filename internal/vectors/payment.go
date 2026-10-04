package vectors

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bbox/boxrec"
)

// paymentBuild is the payment the vectors' payment envelope carries, and
// what its cases are built from.
type paymentBuild struct {
	tx       *transaction.Transaction // pays the recipient, unbroadcast
	other    *transaction.Transaction // pays the third identity instead
	prefix   string
	suffixes []string
	sats     []uint64
}

// b64fixed is a fixed stand-in for a random derivation string: standard
// base64 of 16 bytes.
func b64fixed(label string) string {
	f := fixed32(label)
	return base64.StdEncoding.EncodeToString(f[:16])
}

func (e *txEnv) buildPayment() error {
	p := &paymentBuild{
		prefix:   b64fixed("payment/prefix"),
		suffixes: []string{b64fixed("payment/suffix/0"), b64fixed("payment/suffix/1")},
		sats:     []uint64{1500, 2500},
	}
	pay := func(vout uint32, payee *identity) (*transaction.Transaction, error) {
		tx := transaction.NewTransaction()
		tx.AddInputFromTx(e.coin, vout, e.s.feeUnlocker())
		for i, suffix := range p.suffixes {
			k, err := e.s.w.GetPublicKey(e.ctx, wallet.GetPublicKeyArgs{EncryptionArgs: wallet.EncryptionArgs{
				ProtocolID:   boxrec.PaymentProtocol,
				KeyID:        p.prefix + " " + suffix,
				Counterparty: wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: payee.id},
			}}, originator)
			if err != nil {
				return nil, err
			}
			tx.AddOutput(out(p.sats[i], scr(boxrec.P2PKH(k.PublicKey))))
		}
		tx.AddOutput(out(50000-p.sats[0]-p.sats[1]-200, e.s.feeLock))
		return tx, tx.Sign()
	}
	var err error
	if p.tx, err = pay(1, e.r); err != nil {
		return err
	}
	if p.other, err = pay(2, e.o); err != nil {
		return err
	}
	e.pay = p
	beef, err := p.tx.AtomicBEEF(false)
	if err != nil {
		return err
	}
	e.payEnv, err = e.paymentEnvelope("payment", plaintextWith(beef, p.prefix, p.outputs(0, 1)), t0+300, t0+300+86400)
	return err
}

// payOut is one entry of a plaintext payment's outputs.
type payOut struct {
	index  int64
	suffix string
	sats   int64
}

func (p *paymentBuild) outputs(idx ...int) []payOut {
	var o []payOut
	for _, i := range idx {
		o = append(o, payOut{int64(i), p.suffixes[i], int64(p.sats[i])})
	}
	return o
}

// plaintextWith is the canonical plaintext of a message carrying a
// payment: its body and the payment member.
func plaintextWith(beef []byte, prefix string, outs []payOut) string {
	return canonicalPlain(func(o *boxrec.Object) {
		o.Set(boxrec.PlainBody, "Paid for the report.")
		o.Set(boxrec.PlainPayment, paymentObject(base64.StdEncoding.EncodeToString(beef), prefix, outs))
	})
}

func paymentObject(beef, prefix string, outs []payOut) *boxrec.Object {
	var list []any
	for _, x := range outs {
		list = append(list, &boxrec.Object{Members: []boxrec.Member{
			{Name: "outputIndex", Value: boxrec.Int(x.index)},
			{Name: "derivationSuffix", Value: x.suffix},
			{Name: "satoshis", Value: boxrec.Int(x.sats)},
		}})
	}
	if list == nil {
		list = []any{}
	}
	o := &boxrec.Object{}
	if beef != "" {
		o.Set("beef", beef)
	}
	if prefix != "" {
		o.Set("derivationPrefix", prefix)
	}
	o.Set("outputs", list)
	return o
}

func canonicalPlain(f func(o *boxrec.Object)) string {
	o := &boxrec.Object{}
	f(o)
	b, err := boxrec.Canonical(o)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// refsList is n well-formed references, each with a key, as a plaintext's
// refs member carries them.
func refsList(n int) []any {
	var list []any
	for i := 0; i < n; i++ {
		digest, key := fixed32(fmt.Sprintf("refs/digest/%d", i)), fixed32(fmt.Sprintf("refs/key/%d", i))
		list = append(list, &boxrec.Object{Members: []boxrec.Member{
			{Name: "key", Value: hex.EncodeToString(key[:])},
			{Name: "length", Value: boxrec.Int(4096 + i)},
			{Name: "sha256", Value: hex.EncodeToString(digest[:])},
			{Name: "url", Value: fmt.Sprintf("https://files.example.com/r/%x", digest[:8])},
		}})
	}
	return list
}

// paymentEnvelope seals plaintext from the sender to the recipient in box
// payment_inbox.
func (e *txEnv) paymentEnvelope(label, plaintext string, created, expires uint64) (*sealed, error) {
	return build(spec{name: "payment-" + label, label: "payment/" + label, office: Office, box: "payment_inbox",
		created: created, expires: expires, plaintext: plaintext})
}

type paymentOutJSON struct {
	OutputIndex      uint32 `json:"outputIndex"`
	DerivationSuffix string `json:"derivationSuffix"`
	Satoshis         uint64 `json:"satoshis"`
	LockingScript    string `json:"lockingScript"`
}

type paymentJSON struct {
	Name      string           `json:"name"`
	Note      string           `json:"note"`
	Now       uint64           `json:"now"`
	Created   uint64           `json:"created"`
	Expires   uint64           `json:"expires"`
	Record    string           `json:"record"`
	Plaintext string           `json:"plaintext"`
	Verdict   string           `json:"verdict"`
	Reason    string           `json:"reason,omitempty"`
	Txid      string           `json:"paymentTxid,omitempty"`
	Outputs   []paymentOutJSON `json:"outputs,omitempty"`
}

// recipientView is what the recipient does with an envelope record: decode
// it, check its content, decrypt it through its own wallet, parse the
// plaintext and check the payment at its time now.
func recipientView(ctx context.Context, w wallet.Interface, headers Headers, record []byte, now uint64) (*transaction.Transaction, *boxrec.Payment, []byte, error) {
	env, err := boxrec.DecodeEnvelope(record)
	if err != nil {
		return nil, nil, nil, err
	}
	c, err := boxrec.CheckContent(env)
	if err != nil {
		return nil, nil, nil, err
	}
	plain, err := boxrec.Open(ctx, w, originator, c, env.From)
	if err != nil {
		return nil, nil, nil, err
	}
	pt, err := boxrec.ParsePlaintext(plain)
	if err != nil {
		return nil, nil, plain, err
	}
	if pt.Payment == nil {
		return nil, nil, plain, fmt.Errorf("%w: no payment", boxrec.ErrPaymentShape)
	}
	tx, err := boxrec.CheckPayment(ctx, w, originator, env, pt.Payment, headers, now)
	return tx, pt.Payment, plain, err
}

// paymentVectors builds each payment case: an envelope whose encrypted
// plaintext carries a payment, which the recipient decrypts through its
// wallet and checks. Every case but its target passes every other check.
func (e *txEnv) paymentVectors() (map[string]any, error) {
	p := e.pay
	atomic, err := p.tx.AtomicBEEF(false)
	if err != nil {
		return nil, err
	}
	v1, err := p.tx.BEEF()
	if err != nil {
		return nil, err
	}
	toOther, err := p.other.AtomicBEEF(false)
	if err != nil {
		return nil, err
	}
	stray := cloneTx(e.coin)
	stray.MerklePath = transaction.NewMerklePath(unproven, e.coin.MerklePath.Path)
	strayBEEF, err := spendFrom(p.tx, stray).AtomicBEEF(false)
	if err != nil {
		return nil, err
	}
	created, day := uint64(t0+300), uint64(86400)
	now := created + 60
	std := plaintextWith(atomic, p.prefix, p.outputs(0, 1))
	withPayment := func(po *boxrec.Object) string {
		return canonicalPlain(func(o *boxrec.Object) {
			o.Set(boxrec.PlainBody, "Paid for the report.")
			o.Set(boxrec.PlainPayment, po)
		})
	}
	b64 := base64.StdEncoding.EncodeToString(atomic)
	outs := p.outputs(0, 1)
	type pcase struct {
		name, note, plaintext string
		created, expires, now uint64
		reason                string
	}
	cases := []pcase{
		{"payment-two-outputs", "a payment of two outputs to keys derived for the recipient, both listed; the envelope expires a day after it was created", std, created, created + day, now, ""},
		{"payment-one-of-two", "the same payment listing only output 1: a recipient internalizes the outputs listed", plaintextWith(atomic, p.prefix, p.outputs(1)), created, created + day, now, ""},
		{"payment-expires-two-hours", "the shortest life a payment envelope may have, expires = created + 7200, checked an hour before the last second it may be internalized", std, created, created + 7200, created + 7200 - 3601, ""},
		{"plaintext-not-canonical", "the plaintext with a space after its first brace", "{ " + std[1:], created, created + day, now, "plaintext-json"},
		{"plaintext-body-number", "body is a number", canonicalPlain(func(o *boxrec.Object) {
			o.Set(boxrec.PlainBody, boxrec.Int(7))
			o.Set(boxrec.PlainPayment, paymentObject(b64, p.prefix, outs))
		}), created, created + day, now, "plaintext-shape"},
		{"payment-no-prefix", "derivationPrefix is missing", withPayment(paymentObject(b64, "", outs)), created, created + day, now, "payment-shape"},
		{"payment-outputs-empty", "outputs lists nothing", withPayment(paymentObject(b64, p.prefix, nil)), created, created + day, now, "payment-shape"},
		{"payment-output-listed-twice", "output 0 listed twice", withPayment(paymentObject(b64, p.prefix, append(p.outputs(0), p.outputs(0)...))), created, created + day, now, "payment-shape"},
		{"payment-satoshis-zero", "an output listed with 0 satoshis", withPayment(paymentObject(b64, p.prefix, []payOut{{0, p.suffixes[0], 0}})), created, created + day, now, "payment-shape"},
		{"payment-beef-line-breaks", "beef in base64 broken into lines of 64 characters, as MIME writes it, rather than one standard base64 string", withPayment(paymentObject(b64[:64]+"\r\n"+b64[64:], p.prefix, outs)), created, created + day, now, "payment-shape"},
		{"payment-expires-short", "the envelope expires 7199 seconds after it was created, one short of the least a payment envelope may", std, created, created + 7199, now, "payment-expires"},
		{"payment-expires-zero", "the envelope does not expire", std, created, 0, now, "payment-expires"},
		{"payment-late", "checked at expires - 3600, the first second a recipient no longer internalizes", std, created, created + day, created + day - 3600, "payment-late"},
		{"payment-beef-v1", "the payment as BEEF V1 rather than Atomic BEEF", plaintextWith(v1, p.prefix, outs), created, created + day, now, "payment-beef"},
		{"payment-beef-truncated", "the Atomic BEEF less its last byte", plaintextWith(atomic[:len(atomic)-1], p.prefix, outs), created, created + day, now, "payment-beef"},
		{"payment-satoshis-mismatch", "output 0 listed with 1501 satoshis; it holds 1500", withPayment(paymentObject(b64, p.prefix, []payOut{{0, p.suffixes[0], 1501}})), created, created + day, now, "payment-output"},
		{"payment-output-index-range", "output 3 listed; the payment has three outputs", withPayment(paymentObject(b64, p.prefix, []payOut{{3, p.suffixes[0], 1500}})), created, created + day, now, "payment-output"},
		{"payment-wrong-suffix", "output 0 listed under output 1's derivation suffix", withPayment(paymentObject(b64, p.prefix, []payOut{{0, p.suffixes[1], 1500}})), created, created + day, now, "payment-output"},
		{"payment-to-another-recipient", "a payment whose outputs are derived for the third identity, not the recipient", plaintextWith(toOther, p.prefix, outs), created, created + day, now, "payment-output"},
		{"payment-change-listed", "the sender's change output listed as paid to the recipient", withPayment(paymentObject(b64, p.prefix, []payOut{{2, p.suffixes[0], int64(p.tx.Outputs[2].Satoshis)}})), created, created + day, now, "payment-output"},
		{"payment-parent-unproven", "the payment's parent carries a proof at a height the recipient's headers do not hold", plaintextWith(strayBEEF, p.prefix, outs), created, created + day, now, "payment-spv"},
		{"plaintext-refs-at-cap", "the payment beside 32 references, the most a plaintext carries", canonicalPlain(func(o *boxrec.Object) {
			o.Set(boxrec.PlainBody, "Paid for the report.")
			o.Set(boxrec.PlainPayment, paymentObject(b64, p.prefix, outs))
			o.Set(boxrec.PlainRefs, refsList(boxrec.MaxRefs))
		}), created, created + day, now, ""},
		{"plaintext-refs-over-cap", "the payment beside 33 references, one more than a plaintext carries", canonicalPlain(func(o *boxrec.Object) {
			o.Set(boxrec.PlainBody, "Paid for the report.")
			o.Set(boxrec.PlainPayment, paymentObject(b64, p.prefix, outs))
			o.Set(boxrec.PlainRefs, refsList(boxrec.MaxRefs+1))
		}), created, created + day, now, "plaintext-shape"},
	}
	var list []paymentJSON
	for i, c := range cases {
		s, err := e.paymentEnvelope(fmt.Sprintf("%02d", i), c.plaintext, c.created, c.expires)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.name, err)
		}
		tx, pay, plain, err := recipientView(e.ctx, e.r.w, e.c.headers, s.record, c.now)
		if !bytes.Equal(plain, []byte(c.plaintext)) {
			return nil, fmt.Errorf("%s: the recipient's wallet did not open the plaintext", c.name)
		}
		j := paymentJSON{Name: c.name, Note: c.note, Now: c.now, Created: c.created, Expires: c.expires,
			Record: h(s.record), Plaintext: c.plaintext}
		switch {
		case c.reason == "" && err != nil:
			return nil, fmt.Errorf("%s: refused: %w", c.name, err)
		case c.reason != "" && err == nil:
			return nil, fmt.Errorf("%s: accepted, want %s", c.name, c.reason)
		case c.reason != "" && boxrec.Reason(err) != c.reason:
			return nil, fmt.Errorf("%s: reason %s, want %s (%v)", c.name, boxrec.Reason(err), c.reason, err)
		case err == nil:
			j.Verdict, j.Txid = "accept", tx.TxID().String()
			for _, o := range pay.Outputs {
				j.Outputs = append(j.Outputs, paymentOutJSON{OutputIndex: o.OutputIndex, DerivationSuffix: o.DerivationSuffix,
					Satoshis: o.Satoshis, LockingScript: tx.Outputs[o.OutputIndex].LockingScript.String()})
			}
		default:
			j.Verdict, j.Reason = "refuse", c.reason
		}
		list = append(list, j)
	}
	var headers []map[string]any
	for ht := uint32(801); ht <= e.c.height; ht++ {
		headers = append(headers, map[string]any{"height": ht, "merkleRoot": e.c.headers[ht].String()})
	}
	return map[string]any{
		"description": strings.Join([]string{
			"Payments inside envelopes (docs/spec.md sections 4.5 and 10): the plaintext's payment member, which no host sees.",
			"Each case is an envelope record from the sender to the recipient whose encrypted plaintext carries a payment; the recipient decodes the record, checks its content, decrypts it through its own wallet (protocol [2, \"message encryption\"], counterparty the sender), and then checks, in order: the plaintext (plaintext-json, plaintext-shape), the payment's shape (payment-shape), that the envelope expires at least 7200 seconds after it was created (payment-expires), that now is before expires - 3600 (payment-late), that beef is an Atomic BEEF that parses (payment-beef), that each listed output exists, holds the satoshis listed and is P2PKH to the key the recipient's wallet derives under BRC-29 (protocol [2, \"3241645161d8\"], key id \"<derivationPrefix> <derivationSuffix>\", counterparty the sender) (payment-output), and that the payment verifies against the recipient's headers (payment-spv).",
			"An accepted payment is what the recipient hands its wallet's internalizeAction, with the sender's identity key; the recipient never broadcasts it any other way, and nor does anyone else, since only the recipient can read it.",
			"These checks are the recipient client's, never a host's. Derivation strings and BRC-78 key ids and IVs are fixed stand-ins for random values; now is the recipient's clock.",
		}, " "),
		"office":               Office,
		"senderIdentityKey":    h(e.s.id.Compressed()),
		"recipientIdentityKey": h(e.r.id.Compressed()),
		"paymentProtocol":      []any{2, boxrec.PaymentProtocol.Protocol},
		"derivationPrefix":     p.prefix,
		"derivationSuffixes":   p.suffixes,
		"paymentTxid":          p.tx.TxID().String(),
		"paymentAtomicBeef":    h(atomic),
		"headersNote":          "merkleRoot is in display byte order, as a txid is printed",
		"headers":              headers,
		"payments":             list,
	}, nil
}
