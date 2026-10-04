package vectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/pushdrop"

	"github.com/lightwebinc/bbox/boxrec"
)

// spec describes one envelope vector before it is sealed.
type spec struct {
	name, note string
	office     string
	box        string
	created    uint64
	expires    uint64
	plaintext  string
	label      string // names the fixed key id and IV
	// recipient and sender members beyond identityKey (claims).
	recipient map[string]string
	sender    map[string]string
	quoteID   string
	extraDoc  []boxrec.Member // BRC-169 members this contract does not define
	extra     cbor.Map        // record keys above 7
}

// sealed is an envelope built from a spec.
type sealed struct {
	env    *boxrec.Envelope
	doc    *boxrec.Object
	brc78  []byte
	keyID  [32]byte
	iv     [32]byte
	record []byte
}

func party(key []byte, claims map[string]string) *boxrec.Object {
	o := &boxrec.Object{}
	o.Set(boxrec.MemberIdentity, h(key))
	for _, k := range sortedKeys(claims) {
		o.Set(k, claims[k])
	}
	return o
}

func sortedKeys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	for i := range ks {
		for j := i + 1; j < len(ks); j++ {
			if ks[j] < ks[i] {
				ks[i], ks[j] = ks[j], ks[i]
			}
		}
	}
	return ks
}

// build seals s: the plaintext is encrypted to the recipient under BRC-78
// with a fixed key id and IV, the BRC-169 envelope is signed through the
// sender's wallet, and the record is encoded and read back.
func build(s spec) (*sealed, error) {
	out := &sealed{keyID: fixed32("keyid/" + s.label), iv: fixed32("iv/" + s.label)}
	var err error
	if out.brc78, err = BRC78([]byte(s.plaintext), SenderKey, RecipientKey, out.keyID, out.iv); err != nil {
		return nil, err
	}
	doc := &boxrec.Object{}
	doc.Set(boxrec.MemberVersion, boxrec.MetanetHandles)
	doc.Set(boxrec.MemberRecipient, party(Recipient(), s.recipient))
	doc.Set(boxrec.MemberSender, party(Sender(), s.sender))
	doc.Set(boxrec.MemberCreated, boxrec.RFC3339(s.created))
	if s.quoteID != "" {
		doc.Set(boxrec.MemberQuoteID, s.quoteID)
	}
	for _, m := range s.extraDoc {
		doc.Set(m.Name, m.Value)
	}
	content, err := boxrec.Seal(context.Background(), protoWallet(SenderKey), "", doc, out.brc78)
	if err != nil {
		return nil, err
	}
	out.doc = doc
	out.env = &boxrec.Envelope{
		Office: s.office, To: Recipient(), Box: s.box, From: Sender(),
		Created: s.created, Expires: s.expires, Content: content, Extra: s.extra,
	}
	if out.record, err = out.env.Encode(); err != nil {
		return nil, fmt.Errorf("%s: %w", s.name, err)
	}
	back, err := boxrec.DecodeEnvelope(out.record)
	if err != nil {
		return nil, fmt.Errorf("%s: decode: %w", s.name, err)
	}
	if again, err := back.Encode(); err != nil || !bytes.Equal(again, out.record) {
		return nil, fmt.Errorf("%s: does not round-trip", s.name)
	}
	if _, err := boxrec.CheckContent(back); err != nil {
		return nil, fmt.Errorf("%s: content: %w", s.name, err)
	}
	return out, nil
}

// The plaintexts. The first carries an escape sequence that a renderer must
// strip (spec section 11); the envelope carries it verbatim, encrypted.
const (
	plainNote = "{\"body\":\"Lunch at noon? \\u001b[2Jcleared?\"}"
	plainRefs = "{\"body\":\"The report is attached.\",\"refs\":[{\"key\":\"%s\",\"length\":48213,\"sha256\":\"%s\",\"url\":\"https://files.example.com/r/%s\"}]}"
)

// sixtyFourKeys is 56 unknown keys, 8 to 63: with keys 0 to 7 the record
// has 64 keys, the most a record may have.
func sixtyFourKeys() cbor.Map {
	var m cbor.Map
	for k := uint64(8); k < 64; k++ {
		m = append(m, cbor.Pair{Key: k, Val: k})
	}
	return m
}

func specs() []spec {
	attKey := fixed32("attachment/key")
	attDigest := fixed32("attachment/ciphertext")
	return []spec{
		{
			name: "envelope-note", label: "note",
			note:   "a note to box inbox; the plaintext's body carries ESC [2J, which a renderer strips; the record carries unknown key 24, which a reader preserves and ignores",
			office: Office, box: "inbox", created: t0, expires: 0,
			plaintext: plainNote,
			recipient: map[string]string{"handle": "robin", "domain": "example.com"},
			sender:    map[string]string{"handle": "sam", "domain": "example.org"},
			extra:     cbor.Map{{Key: uint64(24), Val: "a key this version does not define"}},
		},
		{
			name: "envelope-refs", label: "refs",
			note:   "an envelope that expires after seven days, to box files; the plaintext references a larger encrypted attachment by digest (refs); the BRC-169 envelope carries a quoteId and a member this contract does not define, both covered by the signature",
			office: Office, box: "files", created: t0 + 60, expires: t0 + 60 + 7*86400,
			plaintext: fmt.Sprintf(plainRefs, h(attKey[:]), h(attDigest[:]), h(attDigest[:])),
			quoteID:   "q-0001",
			extraDoc:  []boxrec.Member{{Name: "threadId", Value: "t-7"}},
		},
		{
			name: "envelope-64-keys", label: "keys",
			note:   "the record has 64 keys, the most a record may have: 0 to 7 and unknown keys 8 to 63, which a reader preserves and ignores",
			office: Office, box: "inbox", created: t0 + 120,
			plaintext: "{\"body\":\"ok\"}",
			extra:     sixtyFourKeys(),
		},
	}
}

type envelopeJSON struct {
	Name         string         `json:"name"`
	Note         string         `json:"note"`
	Office       string         `json:"office"`
	Topic        string         `json:"topic"`
	To           string         `json:"to"`
	Box          string         `json:"box"`
	From         string         `json:"from"`
	Created      uint64         `json:"created"`
	CreatedText  string         `json:"createdRfc3339"`
	Expires      uint64         `json:"expires"`
	Plaintext    string         `json:"plaintext"`
	BRC78KeyID   string         `json:"brc78KeyId"`
	BRC78IV      string         `json:"brc78Iv"`
	BRC78        string         `json:"brc78"`
	Content      string         `json:"content"`
	Signed       string         `json:"signedJson"`
	SignedHash   string         `json:"signedHash"`
	SignatureKey string         `json:"signatureKey"`
	Record       string         `json:"record"`
	RecordLen    int            `json:"recordLength"`
	FieldHash    string         `json:"carrierFieldHash"`
	Extra        []extraKeyJSON `json:"extra,omitempty"`
}

type extraKeyJSON struct {
	Key   uint64 `json:"key"`
	Value string `json:"valueCbor"`
}

func signatureKey(identity []byte) (string, error) {
	pub, err := ec.PublicKeyFromBytes(identity)
	if err != nil {
		return "", err
	}
	k, err := boxrec.SignatureDerivation.ExpectedLockingKey(pub)
	if err != nil {
		return "", err
	}
	return h(k.Compressed()), nil
}

// Generate returns every vector file, keyed by its name under
// testdata/vectors.
func Generate() (map[string][]byte, error) {
	files := map[string][]byte{}

	var envs []envelopeJSON
	var built []*sealed
	for _, s := range specs() {
		b, err := build(s)
		if err != nil {
			return nil, err
		}
		built = append(built, b)
		c, _ := boxrec.ParseContent(b.env)
		sh := sha256.Sum256(c.Signed)
		fh := sha256.Sum256(b.record)
		topic, _ := boxrec.Topic(s.office)
		sk, err := signatureKey(Sender())
		if err != nil {
			return nil, err
		}
		j := envelopeJSON{
			Name: s.name, Note: s.note, Office: s.office, Topic: topic, To: h(Recipient()),
			Box: s.box, From: h(Sender()), Created: s.created, CreatedText: boxrec.RFC3339(s.created),
			Expires: s.expires, Plaintext: s.plaintext, BRC78KeyID: h(b.keyID[:]), BRC78IV: h(b.iv[:]),
			BRC78: h(b.brc78), Content: string(b.env.Content), Signed: string(c.Signed),
			SignedHash: h(sh[:]), SignatureKey: sk, Record: h(b.record), RecordLen: len(b.record),
			FieldHash: h(fh[:]),
		}
		for _, p := range s.extra {
			v, _ := cbor.Encode(p.Val)
			j.Extra = append(j.Extra, extraKeyJSON{Key: p.Key.(uint64), Value: h(v)})
		}
		envs = append(envs, j)
		files[s.name+".hex"] = []byte(h(b.record) + "\n")
	}
	if err := putJSON(files, "envelope-v1.json", map[string]any{
		"description": "Envelope records (docs/spec.md sections 3 and 4). Sender: the test key 32 bytes of 0x42; recipient: 32 bytes of 0x43. Each BRC-78 key id and IV is a fixed stand-in for 32 random bytes, SHA-256(\"bbox/vector/keyid/<label>\") and SHA-256(\"bbox/vector/iv/<label>\"), so the ciphertext is reproducible; a real sender draws both at random. signedJson is the RFC 8785 serialization of the BRC-169 envelope without content and signature; the signature is over SHA-256 of it (signedHash) under signatureKey, the sender's key for [1, \"bbox message\"], key id signature, counterparty anyone. carrierFieldHash is SHA-256 of the record, which the carrier's field signature covers.",
		"envelopes":   envs,
	}); err != nil {
		return nil, err
	}

	rec, err := receipts(files)
	if err != nil {
		return nil, err
	}
	if err := putJSON(files, "receipt-v1.json", rec); err != nil {
		return nil, err
	}
	q, err := queries()
	if err != nil {
		return nil, err
	}
	if err := putJSON(files, "query-v1.json", q); err != nil {
		return nil, err
	}
	d, err := derivations()
	if err != nil {
		return nil, err
	}
	if err := putJSON(files, "derivation-v1.json", d); err != nil {
		return nil, err
	}
	if err := putJSON(files, "json-v1.json", jsonVectors()); err != nil {
		return nil, err
	}
	sc, err := scriptVectors(built)
	if err != nil {
		return nil, err
	}
	if err := putJSON(files, "script-v1.json", sc); err != nil {
		return nil, err
	}
	r, refused, err := refusals(built)
	if err != nil {
		return nil, err
	}
	if err := putJSON(files, "refusal-v1.json", r); err != nil {
		return nil, err
	}
	e, err := newTxEnv(built, refused)
	if err != nil {
		return nil, err
	}
	txv, err := e.transactionVectors()
	if err != nil {
		return nil, err
	}
	if err := putJSON(files, "transaction-v1.json", txv); err != nil {
		return nil, err
	}
	pv, err := e.paymentVectors()
	if err != nil {
		return nil, err
	}
	if err := putJSON(files, "payment-v1.json", pv); err != nil {
		return nil, err
	}
	return files, nil
}

type receiptJSON struct {
	Name      string   `json:"name"`
	Note      string   `json:"note"`
	Office    string   `json:"office"`
	By        string   `json:"by"`
	Acks      []string `json:"acks"`
	AcksTxids []string `json:"acksDisplayTxids"`
	Created   uint64   `json:"created"`
	Record    string   `json:"record"`
	RecordLen int      `json:"recordLength"`
	FieldHash string   `json:"carrierFieldHash"`
}

// displayTxid is a hash-order commitment as a txid is written in JSON and in
// lookup questions: byte-reversed hex.
func displayTxid(c [32]byte) string {
	r := make([]byte, 32)
	for i := range c {
		r[i] = c[31-i]
	}
	return h(r)
}

func sortedCommitments(n int, from int) [][32]byte {
	var cs [][32]byte
	for i := 0; i < n; i++ {
		cs = append(cs, commitment(from+i))
	}
	for i := range cs {
		for j := i + 1; j < len(cs); j++ {
			if bytes.Compare(cs[j][:], cs[i][:]) < 0 {
				cs[i], cs[j] = cs[j], cs[i]
			}
		}
	}
	return cs
}

func receipts(files map[string][]byte) (map[string]any, error) {
	var out []receiptJSON
	for _, v := range []struct {
		name, note string
		acks       [][32]byte
		created    uint64
	}{
		{"receipt-two", "acknowledges two envelopes; the acks are sorted ascending by their bytes (hash byte order)", sortedCommitments(2, 1), t0 + 3600},
		{"receipt-max", "acknowledges 64 envelopes, the most one receipt may name", sortedCommitments(boxrec.MaxAcks, 100), t0 + 7200},
	} {
		r := &boxrec.Receipt{Office: Office, By: Recipient(), Acks: v.acks, Created: v.created}
		b, err := r.Encode()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", v.name, err)
		}
		back, err := boxrec.DecodeReceipt(b)
		if err != nil {
			return nil, fmt.Errorf("%s: decode: %w", v.name, err)
		}
		if again, _ := back.Encode(); !bytes.Equal(again, b) {
			return nil, fmt.Errorf("%s: does not round-trip", v.name)
		}
		fh := sha256.Sum256(b)
		j := receiptJSON{Name: v.name, Note: v.note, Office: Office, By: h(Recipient()), Created: v.created,
			Record: h(b), RecordLen: len(b), FieldHash: h(fh[:])}
		for _, a := range v.acks {
			j.Acks = append(j.Acks, h(a[:]))
			j.AcksTxids = append(j.AcksTxids, displayTxid(a))
		}
		out = append(out, j)
		files[v.name+".hex"] = []byte(h(b) + "\n")
	}
	return map[string]any{
		"description": "Receipt records (docs/spec.md section 5). By: the recipient test key (32 bytes of 0x43). Commitments are fixed stand-ins for envelope carrier txids, SHA-256(\"bbox/vector/commitment/<i>\"), in hash byte order in the record and byte-reversed as display txids.",
		"receipts":    out,
	}, nil
}

func derivations() (map[string]any, error) {
	var keys []map[string]string
	ctx := context.Background()
	forSelf := true
	for _, who := range []struct {
		name string
		key  []byte
	}{{"sender", SenderKey}, {"recipient", RecipientKey}} {
		w := protoWallet(who.key)
		for _, d := range []pushdrop.Derivation{boxrec.EnvelopeDerivation, boxrec.SignatureDerivation} {
			if err := d.Validate(); err != nil {
				return nil, err
			}
			reader, err := d.ExpectedLockingKey(priv(who.key).PubKey())
			if err != nil {
				return nil, err
			}
			r, err := w.GetPublicKey(ctx, wallet.GetPublicKeyArgs{
				EncryptionArgs: wallet.EncryptionArgs{ProtocolID: d.Protocol, KeyID: d.KeyID, Counterparty: pushdrop.Anyone()},
				ForSelf:        &forSelf,
			}, "")
			if err != nil {
				return nil, err
			}
			if !r.PublicKey.IsEqual(reader) {
				return nil, fmt.Errorf("derivation %s: the owner's and the reader's keys differ", d.KeyID)
			}
			keys = append(keys, map[string]string{
				"identity":   who.name,
				"keyId":      d.KeyID,
				"invoice":    fmt.Sprintf("%d-%s-%s", d.Protocol.SecurityLevel, d.Protocol.Protocol, d.KeyID),
				"lockingKey": h(reader.Compressed()),
			})
		}
	}
	// The application name alone is refused by the SDK: recorded, so a
	// later SDK that changes the rule fails this generator.
	_, sdkErr := wallet.NewKeyDeriver(priv(SenderKey)).DerivePublicKey(
		wallet.Protocol{SecurityLevel: 1, Protocol: "bbox"}, boxrec.KeyEnvelope, pushdrop.Anyone(), true)
	if sdkErr == nil {
		return nil, fmt.Errorf("go-sdk derived under [1, \"bbox\"]; the rename is no longer forced")
	}
	var offices []map[string]any
	for _, id := range []string{Office, OtherOffice} {
		name, suffix, err := boxrec.SplitOffice(id)
		if err != nil {
			return nil, err
		}
		t, _ := boxrec.Topic(id)
		offices = append(offices, map[string]any{"office": id, "name": name, "suffix": suffix, "topic": t})
	}
	drawn, err := boxrec.NewOffice("example_office", bytes.NewReader(bytes.Repeat([]byte{7, 250, 33, 180}, 16)))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"description":      "Registry values and the keys they derive for the two test identities, counterparty anyone, forSelf true on the owner's side (docs/spec.md section 6). Key id fund is the embedded wallet's funding key, derived under counterparty self; no reader or host derives it, so it has no entry here.",
		"fundKeyId":        boxrec.KeyFund,
		"protocol":         []any{1, boxrec.ProtocolName},
		"refusedProtocol":  map[string]string{"protocol": "bbox", "sdkError": sdkErr.Error()},
		"identities":       map[string]string{"sender": h(Sender()), "recipient": h(Recipient())},
		"keys":             keys,
		"tagFunding":       h(boxrec.TagFunding),
		"magicEnvelope":    h(boxrec.MagicEnvelope),
		"magicReceipt":     h(boxrec.MagicReceipt),
		"classifierPrefix": map[string]string{"envelope": "0044" + h(boxrec.MagicEnvelope), "receipt": "0044" + h(boxrec.MagicReceipt)},
		"offices":          offices,
		"officesNote":      "Two offices with one readable part and different suffixes: different offices, different topics. Suffixes here are fixed examples; a real office draws its suffix at random when it is created.",
		"drawnOffice":      map[string]string{"randomBytes": strings.Repeat("07fa21b4", 16), "office": drawn, "note": "NewOffice over these bytes: each byte below 234 gives the letter a + byte mod 26; bytes 234 to 255 are skipped"},
		"maxOfficeName":    boxrec.MaxOfficeName,
		"suffixLength":     boxrec.SuffixLen,
		"lookupService":    boxrec.LookupService,
		"topicPrefix":      boxrec.TopicPrefix,
	}, nil
}
