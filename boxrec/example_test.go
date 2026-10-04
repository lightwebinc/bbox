package boxrec_test

import (
	"bytes"
	"context"
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bbox/boxrec"
)

// An application's own plaintext member rides beside bbox's: written with
// EncodePlaintext, read back with Extension, its members as the sender
// wrote them.
func ExampleEncodePlaintext() {
	app := &boxrec.Object{}
	app.Set("v", boxrec.Int(1))
	app.Set("kind", "dm")
	app.Set("seq", boxrec.Int(1))

	doc := &boxrec.Object{}
	doc.Set(boxrec.PlainBody, "hello")
	doc.Set("app", app)
	plain, err := boxrec.EncodePlaintext(doc)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(plain))

	p, err := boxrec.ParsePlaintext(plain)
	if err != nil {
		panic(err)
	}
	m, _ := p.Extension("app")
	kind, _ := m.Get("kind")
	seq, _ := m.Get("seq")
	fmt.Println(*p.Body, kind, seq)

	// A plaintext the recipient would refuse is not written.
	bad := &boxrec.Object{}
	bad.Set(boxrec.PlainBody, boxrec.Int(7))
	_, err = boxrec.EncodePlaintext(bad)
	fmt.Println(boxrec.Reason(err))
	// Output:
	// {"app":{"kind":"dm","seq":1,"v":1},"body":"hello"}
	// hello dm 1
	// plaintext-shape
}

// An envelope from sealing to opening: the sender seals the plaintext into
// the content, the record carries it, and the recipient checks the content
// against the record, decrypts it and applies the plaintext's rules.
func ExampleSealEnvelope() {
	ctx := context.Background()
	senderKey, _ := ec.NewPrivateKey()
	recipientKey, _ := ec.NewPrivateKey()
	sender, _ := wallet.NewCompletedProtoWallet(senderKey)
	recipient, _ := wallet.NewCompletedProtoWallet(recipientKey)
	from, to := senderKey.PubKey().Compressed(), recipientKey.PubKey().Compressed()

	doc := &boxrec.Object{}
	doc.Set(boxrec.PlainBody, "hello")
	plain, err := boxrec.EncodePlaintext(doc)
	if err != nil {
		panic(err)
	}
	const created = 1700000000
	content, err := boxrec.SealEnvelope(ctx, sender, "example", from, to, plain, created)
	if err != nil {
		panic(err)
	}
	env := &boxrec.Envelope{Office: "example_abcdefghij", To: to, Box: "inbox", From: from, Created: created, Content: content}
	record, err := env.Encode()
	if err != nil {
		panic(err)
	}

	got, err := boxrec.DecodeEnvelope(record)
	if err != nil {
		panic(err)
	}
	c, err := boxrec.CheckContent(got)
	if err != nil {
		panic(err)
	}
	opened, err := boxrec.Open(ctx, recipient, "example", c, got.From)
	if err != nil {
		panic(err)
	}
	p, err := boxrec.ParsePlaintext(opened)
	if err != nil {
		panic(err)
	}
	fmt.Println(got.Box, bytes.Equal(got.From, from), *p.Body)
	// Output:
	// inbox true hello
}
