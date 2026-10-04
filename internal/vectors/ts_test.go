package vectors

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/message"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bbox/boxrec"
)

const tsFile = "../ts/ts-v1.json"

// TestTypeScriptWrittenVectors reads what the TypeScript codec wrote
// (host/src/tsvectors.ts): the office draw reproduces from the same random
// bytes; every record decodes and re-encodes to the same bytes; each
// envelope passes the content rules with the signature the TypeScript SDK
// made, and its BRC-78 message opens for the recipient to the plaintext the
// file lists; the scripts rebuild identically; the carrier the TypeScript
// side built is admitted with the commitment it computed; and the payment
// it built is accepted by the recipient.
func TestTypeScriptWrittenVectors(t *testing.T) {
	var v struct {
		Office     string `json:"office"`
		OfficeDraw struct {
			Name   string `json:"name"`
			Random string `json:"random"`
			Office string `json:"office"`
		} `json:"officeDraw"`
		Envelopes []struct {
			Label      string `json:"label"`
			Plaintext  string `json:"plaintext"`
			KeyID      string `json:"keyID"`
			IV         string `json:"iv"`
			SignedJSON string `json:"signedJson"`
			Record     string `json:"record"`
		} `json:"envelopes"`
		Receipt struct {
			Record    string   `json:"record"`
			Acks      []string `json:"acks"`
			ExtraKeys []uint64 `json:"extraKeys"`
		} `json:"receipt"`
		RecordScript struct {
			Key, Record, Signature, Script string
		} `json:"recordScript"`
		FundingScript struct {
			Key, Script string
		} `json:"fundingScript"`
		Headers []struct {
			Height uint32 `json:"height"`
			Root   string `json:"merkleRoot"`
		} `json:"headers"`
		Carrier struct {
			Txid       string `json:"txid"`
			Commitment string `json:"commitment"`
			BEEF       string `json:"beef"`
		} `json:"carrier"`
		Payment struct {
			Now     uint64 `json:"now"`
			Txid    string `json:"paymentTxid"`
			Outputs []struct {
				OutputIndex      uint32 `json:"outputIndex"`
				DerivationSuffix string `json:"derivationSuffix"`
				Satoshis         uint64 `json:"satoshis"`
			} `json:"outputs"`
		} `json:"payment"`
	}
	load(t, tsFile, &v)
	ctx := context.Background()

	drawn, err := boxrec.NewOffice(v.OfficeDraw.Name, bytes.NewReader(unhex(t, v.OfficeDraw.Random)))
	if err != nil || drawn != v.OfficeDraw.Office {
		t.Fatalf("office draw: %q %v, TypeScript drew %q", drawn, err, v.OfficeDraw.Office)
	}

	if len(v.Envelopes) != 2 {
		t.Fatalf("%d envelopes", len(v.Envelopes))
	}
	for _, x := range v.Envelopes {
		rec := unhex(t, x.Record)
		e, err := boxrec.DecodeEnvelope(rec)
		if err != nil {
			t.Fatalf("%s: %v", x.Label, err)
		}
		if again, err := e.Encode(); err != nil || !bytes.Equal(again, rec) {
			t.Fatalf("%s: does not re-encode byte for byte: %v", x.Label, err)
		}
		c, err := boxrec.CheckContent(e)
		if err != nil {
			t.Fatalf("%s: content: %v", x.Label, err)
		}
		if string(c.Signed) != x.SignedJSON || hex.EncodeToString(c.Cipher[70:102]) != x.KeyID || hex.EncodeToString(c.Cipher[102:134]) != x.IV {
			t.Fatalf("%s: signed serialization, key id or IV differ", x.Label)
		}
		plain, err := message.Decrypt(c.Cipher, priv(RecipientKey))
		if err != nil || string(plain) != x.Plaintext {
			t.Fatalf("%s: message.Decrypt: %v", x.Label, err)
		}
		if _, err := boxrec.ParsePlaintext(plain); err != nil {
			t.Fatalf("%s: plaintext: %v", x.Label, err)
		}
	}
	// The note's extra keys: a text value and a nested array with a map.
	note, _ := boxrec.DecodeEnvelope(unhex(t, v.Envelopes[0].Record))
	if len(note.Extra) != 2 || note.Extra[0].Key.(uint64) != 9 || note.Extra[1].Key.(uint64) != 40 {
		t.Errorf("note extra keys: %v", note.Extra)
	}

	r, err := boxrec.DecodeReceipt(unhex(t, v.Receipt.Record))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := r.Encode(); err != nil || !bytes.Equal(again, unhex(t, v.Receipt.Record)) {
		t.Errorf("receipt does not re-encode: %v", err)
	}
	for i, a := range r.Acks {
		if hex.EncodeToString(a[:]) != v.Receipt.Acks[i] {
			t.Errorf("ack %d", i)
		}
	}
	if len(r.Extra) != 1 || r.Extra[0].Key.(uint64) != v.Receipt.ExtraKeys[0] {
		t.Error("receipt extra keys")
	}

	key, err := ec.PublicKeyFromBytes(unhex(t, v.RecordScript.Key))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := boxrec.EnvelopeDerivation.ExpectedLockingKey(priv(SenderKey).PubKey())
	if !key.IsEqual(want) {
		t.Fatal("the TypeScript envelope key is not the Go reader derivation")
	}
	if got := boxrec.PushDropScript(key, [][]byte{unhex(t, v.RecordScript.Record)}, unhex(t, v.RecordScript.Signature)); !bytes.Equal(got, unhex(t, v.RecordScript.Script)) {
		t.Errorf("record script differs:\n go %x\n ts %s", got, v.RecordScript.Script)
	}
	if got := boxrec.FundingScript(key); !bytes.Equal(got, unhex(t, v.FundingScript.Script)) {
		t.Errorf("funding script differs")
	}

	headers := Headers{}
	for _, x := range v.Headers {
		root, err := hexHash(x.Root)
		if err != nil {
			t.Fatal(err)
		}
		headers[x.Height] = root
	}
	a, err := boxrec.Admit(ctx, unhex(t, v.Carrier.BEEF), nil, boxrec.Host{Office: v.Office, Headers: headers})
	if err != nil {
		t.Fatalf("the TypeScript carrier is refused: %v", err)
	}
	if a.Kind != boxrec.TxEnvelope || hex.EncodeToString(a.Carrier.Commitment[:]) != v.Carrier.Commitment || displayTxid(a.Carrier.Commitment) != v.Carrier.Txid {
		t.Errorf("the TypeScript carrier: %s %x", a.Kind, a.Carrier.Commitment)
	}

	w, err := wallet.NewCompletedProtoWallet(priv(RecipientKey))
	if err != nil {
		t.Fatal(err)
	}
	tx, pay, _, err := recipientView(ctx, w, headers, unhex(t, v.Envelopes[1].Record), v.Payment.Now)
	if err != nil {
		t.Fatalf("the TypeScript payment is refused: %v", err)
	}
	if tx.TxID().String() != v.Payment.Txid || len(pay.Outputs) != len(v.Payment.Outputs) {
		t.Fatal("the TypeScript payment differs")
	}
	for i, o := range pay.Outputs {
		p := v.Payment.Outputs[i]
		if o.OutputIndex != p.OutputIndex || o.DerivationSuffix != p.DerivationSuffix || o.Satoshis != p.Satoshis {
			t.Errorf("payment output %d", i)
		}
	}
}

func hexHash(s string) (chainhash.Hash, error) {
	h, err := chainhash.NewHashFromHex(s)
	if err != nil {
		return chainhash.Hash{}, err
	}
	return *h, nil
}
