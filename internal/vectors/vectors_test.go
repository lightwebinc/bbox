package vectors

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/message"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/carrier"

	"github.com/lightwebinc/bbox/internal/boxrec"
)

const dir = "../../testdata/vectors"

// TestVectors regenerates every vector and compares it byte for byte with
// the checked-in file, and refuses a checked-in file nothing generates.
func TestVectors(t *testing.T) {
	files, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range Names(files) {
		have, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v (run: GOWORK=off go run ./cmd/vectors)", name, err)
		}
		if !bytes.Equal(have, files[name]) {
			t.Errorf("%s differs from what the codec generates", name)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := files[e.Name()]; !ok {
			t.Errorf("%s: nothing generates it", e.Name())
		}
	}
}

// TestDeterministic runs the generator twice.
func TestDeterministic(t *testing.T) {
	a, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Generate()
	for k := range a {
		if !bytes.Equal(a[k], b[k]) {
			t.Errorf("%s differs between runs", k)
		}
	}
}

func load(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestEnvelopesFromFile reads each envelope vector as a reader would: the
// record decodes and re-encodes to the same bytes, the content passes every
// content rule, and the recipient opens the BRC-78 message two ways, with
// go-sdk's message.Decrypt and through its own wallet, to the plaintext.
func TestEnvelopesFromFile(t *testing.T) {
	var f struct {
		Envelopes []envelopeJSON `json:"envelopes"`
	}
	load(t, "envelope-v1.json", &f)
	if len(f.Envelopes) == 0 {
		t.Fatal("no envelopes")
	}
	recipient := priv(RecipientKey)
	rw := protoWallet(RecipientKey)
	for _, v := range f.Envelopes {
		rec := unhex(t, v.Record)
		if !boxrec.IsEnvelope(rec) || boxrec.IsReceipt(rec) {
			t.Fatalf("%s: classifier", v.Name)
		}
		e, err := boxrec.DecodeEnvelope(rec)
		if err != nil {
			t.Fatalf("%s: %v", v.Name, err)
		}
		if again, _ := e.Encode(); !bytes.Equal(again, rec) {
			t.Fatalf("%s: re-encode differs", v.Name)
		}
		if len(v.Extra) != len(e.Extra) {
			t.Fatalf("%s: unknown keys not preserved", v.Name)
		}
		c, err := boxrec.CheckContent(e)
		if err != nil {
			t.Fatalf("%s: %v", v.Name, err)
		}
		if string(c.Signed) != v.Signed {
			t.Fatalf("%s: signed serialization differs", v.Name)
		}
		plain, err := message.Decrypt(c.Cipher, recipient)
		if err != nil || string(plain) != v.Plaintext {
			t.Fatalf("%s: message.Decrypt: %v", v.Name, err)
		}
		keyID := c.Cipher[70:102]
		sender, _ := ec.PublicKeyFromBytes(e.From)
		d, err := rw.Decrypt(context.Background(), wallet.DecryptArgs{
			EncryptionArgs: wallet.EncryptionArgs{
				ProtocolID:   wallet.Protocol{SecurityLevel: 2, Protocol: "message encryption"},
				KeyID:        b64(keyID),
				Counterparty: wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: sender},
			},
			Ciphertext: c.Cipher[102:],
		}, "")
		if err != nil || string(d.Plaintext) != v.Plaintext {
			t.Fatalf("%s: wallet decrypt: %v", v.Name, err)
		}
		// The plaintext is itself a canonical object in the subset.
		pv, err := boxrec.ParseJSON(plain)
		if err != nil {
			t.Fatalf("%s: plaintext: %v", v.Name, err)
		}
		if cb, _ := boxrec.Canonical(pv); !bytes.Equal(cb, plain) {
			t.Fatalf("%s: plaintext is not canonical", v.Name)
		}
	}
}

// TestRefusalsFromFile decodes every refusal case from its bytes.
func TestRefusalsFromFile(t *testing.T) {
	var f struct {
		Refusals []refusalJSON `json:"refusals"`
	}
	load(t, "refusal-v1.json", &f)
	for _, r := range f.Refusals {
		rec := unhex(t, r.Record)
		var err error
		switch r.Kind {
		case "envelope":
			var e *boxrec.Envelope
			if e, err = boxrec.DecodeEnvelope(rec); err == nil {
				_, err = boxrec.CheckContent(e)
			}
		case "receipt":
			_, err = boxrec.DecodeReceipt(rec)
		default:
			t.Fatalf("%s: kind %q", r.Name, r.Kind)
		}
		if got := boxrec.Reason(err); got != r.Reason {
			t.Errorf("%s: reason %q, want %q (%v)", r.Name, got, r.Reason, err)
		}
		k, ar := admit(rec)
		if k.String() != r.Claims || ar != r.AdmissionReason || (k == boxrec.KindNone) != (r.AtAdmission == "skip") {
			t.Errorf("%s: admission claims %s reason %q, file says %s %q %s", r.Name, k, ar, r.Claims, r.AdmissionReason, r.AtAdmission)
		}
	}
}

// TestQueriesFromFile validates every question from its JSON text.
func TestQueriesFromFile(t *testing.T) {
	var f struct {
		Questions []queryJSON `json:"questions"`
	}
	load(t, "query-v1.json", &f)
	for _, q := range f.Questions {
		var m map[string]any
		if err := json.Unmarshal([]byte(q.Question), &m); err != nil {
			t.Fatal(err)
		}
		got, err := boxrec.ParseQuery(m)
		switch {
		case q.Refused != "":
			if boxrec.Reason(err) != q.Refused {
				t.Errorf("%s: %v, want refused %s", q.Name, err, q.Refused)
			}
		case err != nil:
			t.Errorf("%s: %v", q.Name, err)
		case got.Class.Name != q.Class || got.Class.Free != *q.Free:
			t.Errorf("%s: class %s", q.Name, got.Class.Name)
		}
	}
}

// TestReceiptsFromFile decodes each receipt and checks that each ack's
// display txid is the byte-reversed commitment.
func TestReceiptsFromFile(t *testing.T) {
	var f struct {
		Receipts []receiptJSON `json:"receipts"`
	}
	load(t, "receipt-v1.json", &f)
	for _, v := range f.Receipts {
		rec := unhex(t, v.Record)
		if !boxrec.IsReceipt(rec) || boxrec.IsEnvelope(rec) {
			t.Fatalf("%s: classifier", v.Name)
		}
		r, err := boxrec.DecodeReceipt(rec)
		if err != nil {
			t.Fatalf("%s: %v", v.Name, err)
		}
		for i, a := range r.Acks {
			if displayTxid(a) != v.AcksTxids[i] || hex.EncodeToString(a[:]) != v.Acks[i] {
				t.Fatalf("%s: ack %d", v.Name, i)
			}
		}
	}
}

// TestBRC78MatchesSDK encrypts with the vectors' fixed key id and IV and
// with go-sdk's message.Encrypt, which draws both, and requires the header
// and the length to agree: the vector differs from the SDK only in the
// random bytes.
func TestBRC78MatchesSDK(t *testing.T) {
	msg := []byte("same plaintext")
	fixed, err := BRC78(msg, SenderKey, RecipientKey, fixed32("t/k"), fixed32("t/iv"))
	if err != nil {
		t.Fatal(err)
	}
	sdk, err := message.Encrypt(msg, priv(SenderKey), priv(RecipientKey).PubKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(fixed) != len(sdk) || !bytes.Equal(fixed[:70], sdk[:70]) {
		t.Fatalf("header or length differ: %x vs %x", fixed[:70], sdk[:70])
	}
	if p, err := message.Decrypt(sdk, priv(RecipientKey)); err != nil || !bytes.Equal(p, msg) {
		t.Fatal(err)
	}
}

// TestJSONFromFile parses every JSON subset case from its input bytes.
func TestJSONFromFile(t *testing.T) {
	var f struct {
		Cases []jsonCase `json:"cases"`
	}
	load(t, "json-v1.json", &f)
	for _, c := range f.Cases {
		in := unhex(t, c.InputHex)
		v, err := boxrec.ParseJSON(in)
		if (err == nil) != c.Accepted {
			t.Errorf("%s: parse error %v, accepted %v", c.Name, err, c.Accepted)
			continue
		}
		if err != nil {
			continue
		}
		cb, err := boxrec.Canonical(v)
		if err != nil || string(cb) != c.Canonical || bytes.Equal(cb, in) != c.IsCanonical {
			t.Errorf("%s: canonical %q", c.Name, cb)
		}
	}
}

// TestScriptsFromFile classifies every script case from its bytes.
func TestScriptsFromFile(t *testing.T) {
	var f struct {
		Scripts []scriptCase `json:"scripts"`
	}
	load(t, "script-v1.json", &f)
	for _, c := range f.Scripts {
		if got := boxrec.ClaimOf(unhex(t, c.Script)).String(); got != c.Claims {
			t.Errorf("%s: claims %s, want %s", c.Name, got, c.Claims)
		}
	}
}

func headersFrom(t *testing.T, list []struct {
	Height uint32 `json:"height"`
	Root   string `json:"merkleRoot"`
}) Headers {
	t.Helper()
	hs := Headers{}
	for _, x := range list {
		r, err := chainhash.NewHashFromHex(x.Root)
		if err != nil {
			t.Fatal(err)
		}
		hs[x.Height] = *r
	}
	return hs
}

// TestTransactionVectorsFromBEEF admits every transaction case from its
// BEEF bytes, as a host receives it, with the previous coins the case
// lists, against the headers the file lists.
func TestTransactionVectorsFromBEEF(t *testing.T) {
	var f struct {
		Office  string `json:"office"`
		MaxBEEF int    `json:"maxBeef"`
		Headers []struct {
			Height uint32 `json:"height"`
			Root   string `json:"merkleRoot"`
		} `json:"headers"`
		Transactions []txJSON `json:"transactions"`
	}
	load(t, "transaction-v1.json", &f)
	headers := headersFrom(t, f.Headers)
	var admitted, refused int
	for _, c := range f.Transactions {
		var held []int
		for _, p := range c.Coins {
			held = append(held, p.InputIndex)
		}
		a, err := boxrec.Admit(context.Background(), unhex(t, c.BEEF), held, boxrec.Host{Office: f.Office, Headers: headers, MaxBEEF: f.MaxBEEF})
		if c.Verdict == "refuse" {
			refused++
			if got := boxrec.Reason(err); got != c.Reason {
				t.Errorf("%s: reason %s, want %s (%v)", c.Name, got, c.Reason, err)
			}
			continue
		}
		admitted++
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		got, _ := json.Marshal(admission(a))
		want, _ := json.Marshal(c.Admits)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: admits\n %s\nwant\n %s", c.Name, got, want)
		}
		tx, err := transaction.NewTransactionFromHex(c.RawTx)
		if err != nil || tx.TxID().String() != c.Txid || hex.EncodeToString(a.Txid[:]) != hex.EncodeToString(tx.TxID()[:]) {
			t.Errorf("%s: txid", c.Name)
		}
	}
	if admitted == 0 || refused == 0 {
		t.Fatalf("admitted %d, refused %d", admitted, refused)
	}
}

// TestCarriersAgreeWithBcommon holds the admitted carriers to bcommon's own
// carrier.Decode and Validate, and every carrier refused for a rule bcommon
// also checks (mineable, unlock, lock) to a refusal there too. The field
// signature's high-S twin is the one refusal bcommon's Validate does not
// make, which docs/spec.md section 8.1 rule 8 states.
func TestCarriersAgreeWithBcommon(t *testing.T) {
	var f struct {
		Transactions []txJSON `json:"transactions"`
	}
	load(t, "transaction-v1.json", &f)
	classify := func(p []byte) (bool, error) { return boxrec.ClaimOfField(p) != boxrec.KindNone, nil }
	checked := 0
	for _, c := range f.Transactions {
		tx, err := transaction.NewTransactionFromBEEF(unhex(t, c.BEEF))
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		verdict := func() error {
			k, err := carrier.Decode(tx, classify)
			if err != nil {
				return err
			}
			owner := []byte(nil)
			switch boxrec.ClaimOfField(k.Payload) {
			case boxrec.KindEnvelope:
				e, err := boxrec.DecodeEnvelope(k.Payload)
				if err != nil {
					return err
				}
				owner = e.From
			case boxrec.KindReceipt:
				r, err := boxrec.DecodeReceipt(k.Payload)
				if err != nil {
					return err
				}
				owner = r.By
			}
			return k.Validate(boxrec.Params(), owner)
		}
		switch {
		case c.Verdict == "admit" && (c.Admits.Kind == "envelope" || c.Admits.Kind == "receipt"):
			if err := verdict(); err != nil {
				t.Errorf("%s: bcommon refuses an admitted carrier: %v", c.Name, err)
			}
			checked++
		case c.Name == "carrier-field-signature-high-s":
			if err := verdict(); err != nil {
				t.Errorf("%s: bcommon now refuses it (%v); rule 8's note in the spec is stale", c.Name, err)
			}
		case c.Reason == "mineable" || c.Reason == "unlock" || c.Reason == "lock":
			if verdict() == nil {
				t.Errorf("%s: bcommon admits what bbox refuses as %s", c.Name, c.Reason)
			}
			checked++
		}
	}
	if checked < 10 {
		t.Fatalf("only %d carriers checked", checked)
	}
}

// TestPaymentVectorsFromFile reads every payment case as the recipient
// would: from the envelope record, through its own wallet, at the case's
// time, against the file's headers.
func TestPaymentVectorsFromFile(t *testing.T) {
	var f struct {
		Headers []struct {
			Height uint32 `json:"height"`
			Root   string `json:"merkleRoot"`
		} `json:"headers"`
		Payments []paymentJSON `json:"payments"`
	}
	load(t, "payment-v1.json", &f)
	headers := headersFrom(t, f.Headers)
	w, err := wallet.NewCompletedProtoWallet(priv(RecipientKey))
	if err != nil {
		t.Fatal(err)
	}
	var accepted int
	for _, c := range f.Payments {
		tx, pay, plain, err := recipientView(context.Background(), w, headers, unhex(t, c.Record), c.Now)
		if string(plain) != c.Plaintext {
			t.Errorf("%s: plaintext differs", c.Name)
		}
		if c.Verdict == "refuse" {
			if got := boxrec.Reason(err); got != c.Reason {
				t.Errorf("%s: reason %s, want %s (%v)", c.Name, got, c.Reason, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		accepted++
		if tx.TxID().String() != c.Txid || len(pay.Outputs) != len(c.Outputs) {
			t.Errorf("%s: payment", c.Name)
			continue
		}
		for i, o := range pay.Outputs {
			if o.OutputIndex != c.Outputs[i].OutputIndex || tx.Outputs[o.OutputIndex].LockingScript.String() != c.Outputs[i].LockingScript {
				t.Errorf("%s: output %d", c.Name, i)
			}
		}
	}
	if accepted == 0 {
		t.Fatal("no payment accepted")
	}
}

// TestPaymentOnlyTheRecipientOpens: the payment envelope's message opens for
// the recipient's wallet and for no other key, and a host's checks pass
// without it.
func TestPaymentOnlyTheRecipientOpens(t *testing.T) {
	var f struct {
		Payments []paymentJSON `json:"payments"`
	}
	load(t, "payment-v1.json", &f)
	e, err := boxrec.DecodeEnvelope(unhex(t, f.Payments[0].Record))
	if err != nil {
		t.Fatal(err)
	}
	c, err := boxrec.CheckContent(e)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := wallet.NewCompletedProtoWallet(priv(OtherKey))
	if _, err := boxrec.Open(context.Background(), other, "", c, e.From); boxrec.Reason(err) != "undecryptable" {
		t.Fatalf("a third key opened the payment envelope: %v", err)
	}
}
