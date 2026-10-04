package vectors

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/pushdrop"

	"github.com/lightwebinc/bbox/boxrec"
)

type refusalJSON struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Note        string `json:"note,omitempty"`
	Record      string `json:"record"`
	Reason      string `json:"reason"`
	AtAdmission string `json:"atAdmission"`
	// Claims and AdmissionReason are what admission does with an output
	// whose first field is Record: which record it claims to be, and the
	// reason it is refused as that record. They differ from Kind and
	// Reason when the magic names the other record.
	Claims          string `json:"admissionClaims"`
	AdmissionReason string `json:"admissionReason,omitempty"`
}

// admit is what admission does with a first field: classify it, then
// decode it as the record it claims.
func admit(rec []byte) (boxrec.Kind, string) {
	k := boxrec.ClaimOfField(rec)
	var err error
	switch k {
	case boxrec.KindEnvelope:
		var e *boxrec.Envelope
		if e, err = boxrec.DecodeEnvelope(rec); err == nil {
			_, err = boxrec.CheckContent(e)
		}
	case boxrec.KindReceipt:
		_, err = boxrec.DecodeReceipt(rec)
	default:
		return k, ""
	}
	return k, boxrec.Reason(err)
}

// fields is a record as its known keys, for building refused variants.
type fields map[uint64]cbor.Value

func (f fields) encode() []byte {
	var m cbor.Map
	for k, v := range f {
		m = append(m, cbor.Pair{Key: k, Val: v})
	}
	b, err := cbor.Encode(m)
	if err != nil {
		panic(err)
	}
	return b
}

func (f fields) with(k uint64, v cbor.Value) fields {
	g := fields{}
	for a, b := range f {
		g[a] = b
	}
	if v == nil {
		delete(g, k)
	} else {
		g[k] = v
	}
	return g
}

// drop marks a key for removal in with.
var drop cbor.Value

func envelopeFields(e *boxrec.Envelope) fields {
	return fields{
		boxrec.EKMagic: boxrec.MagicEnvelope, boxrec.EKOffice: e.Office, boxrec.EKTo: e.To,
		boxrec.EKBox: e.Box, boxrec.EKFrom: e.From, boxrec.EKCreated: e.Created,
		boxrec.EKExpires: e.Expires, boxrec.EKContent: e.Content,
	}
}

// aliasKey is 02 || p + 1: the point with x = 1 under an x-coordinate at or
// above the field prime, which some parsers reduce and accept.
func aliasKey() []byte {
	p, _ := new(big.Int).SetString("fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f", 16)
	x := new(big.Int).Add(p, big.NewInt(1)).FillBytes(make([]byte, 32))
	return append([]byte{0x02}, x...)
}

// nonMinimalCreated rewrites key 5's value from a four-byte to an
// eight-byte unsigned integer head: the same value, not canonical.
func nonMinimalCreated(rec []byte, created uint64) []byte {
	var four, eight [4]byte
	binary.BigEndian.PutUint32(four[:], uint32(created))
	old := append([]byte{0x05, 0x1a}, four[:]...)
	repl := append([]byte{0x05, 0x1b}, append(eight[:], four[:]...)...)
	if !bytes.Contains(rec, old) {
		panic("created head not found")
	}
	return bytes.Replace(rec, old, repl, 1)
}

func sigOver(key *ec.PrivateKey, msg []byte) string {
	h := sha256.Sum256(msg)
	s, err := key.Sign(h[:])
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(s.Serialize())
}

func derivedPriv(identity []byte, d pushdrop.Derivation) *ec.PrivateKey {
	k, err := wallet.NewKeyDeriver(priv(identity)).DerivePrivateKey(d.Protocol, d.KeyID, pushdrop.Anyone())
	if err != nil {
		panic(err)
	}
	return k
}

// highS rewrites a low-S DER signature into its high-S twin: the same
// signature for ECDSA, a different byte string.
func highS(sigHex string) string {
	b, _ := hex.DecodeString(sigHex)
	sig, err := ec.ParseDERSignature(b)
	if err != nil {
		panic(err)
	}
	n := ec.S256().N
	s := new(big.Int).Sub(n, sig.S)
	r := sig.R.Bytes()
	if r[0]&0x80 != 0 {
		r = append([]byte{0}, r...)
	}
	sb := s.Bytes()
	if sb[0]&0x80 != 0 {
		sb = append([]byte{0}, sb...)
	}
	out := []byte{0x30, byte(4 + len(r) + len(sb)), 0x02, byte(len(r))}
	out = append(out, r...)
	out = append(out, 0x02, byte(len(sb)))
	out = append(out, sb...)
	return hex.EncodeToString(out)
}

// cloneDoc copies the envelope object one level deep, and the recipient and
// sender objects, so a case may change a member without touching the base.
func cloneDoc(o *boxrec.Object) *boxrec.Object {
	out := &boxrec.Object{}
	for _, m := range o.Members {
		if sub, ok := m.Value.(*boxrec.Object); ok {
			c := &boxrec.Object{Members: append([]boxrec.Member(nil), sub.Members...)}
			out.Members = append(out.Members, boxrec.Member{Name: m.Name, Value: c})
			continue
		}
		out.Members = append(out.Members, m)
	}
	return out
}

func mustCanon(o *boxrec.Object) []byte {
	b, err := boxrec.Canonical(o)
	if err != nil {
		panic(err)
	}
	return b
}

// resign replaces the signature member with a valid one for the document
// as it now stands, so a case is refused for the rule it breaks and not for
// the signature.
func resign(o *boxrec.Object) {
	signed := mustCanon(o.Without(boxrec.MemberContent, boxrec.MemberSignature))
	o.Set(boxrec.MemberSignature, sigOver(derivedPriv(SenderKey, boxrec.SignatureDerivation), signed))
}

// refusals returns the refusal vectors, and each record by name for the
// transaction vectors that carry one.
func refusals(built []*sealed) (map[string]any, map[string][]byte, error) {
	base := built[0]
	ef := envelopeFields(base.env)
	var out []refusalJSON
	records := map[string][]byte{}

	addEnv := func(name, note string, rec []byte, want string) error {
		j := refusalJSON{Name: name, Kind: "envelope", Note: note, Record: h(rec), AtAdmission: "refuse"}
		k, ar := admit(rec)
		j.Claims, j.AdmissionReason = k.String(), ar
		if k == boxrec.KindNone {
			j.AtAdmission = "skip"
		}
		e, err := boxrec.DecodeEnvelope(rec)
		if err == nil {
			_, err = boxrec.CheckContent(e)
		}
		if err == nil {
			return fmt.Errorf("%s: not refused", name)
		}
		j.Reason = boxrec.Reason(err)
		if j.Reason != want {
			return fmt.Errorf("%s: refused %q (%v), want %q", name, j.Reason, err, want)
		}
		out = append(out, j)
		records[name] = rec
		return nil
	}
	record := []struct {
		name, note string
		rec        []byte
		want       string
	}{
		{"envelope-too-large", "over 20480 bytes: refused before it is decoded", ef.with(boxrec.EKContent, bytes.Repeat([]byte{'x'}, boxrec.MaxEnvelopeRecord)).encode(), "too-large"},
		{"envelope-not-canonical", "created written with an eight-byte head", nonMinimalCreated(base.record, base.env.Created), "cbor"},
		{"envelope-not-a-map", "an array: claims nothing", mustCBOR([]cbor.Value{boxrec.MagicEnvelope}), "cbor"},
		{"envelope-65-keys", "more than 64 keys, unknown ones included", func() []byte {
			f := ef
			for k := uint64(8); k < 8+57; k++ {
				f = f.with(k, uint64(0))
			}
			return f.encode()
		}(), "too-large"},
		{"envelope-text-key", "a key that is not an unsigned integer", func() []byte {
			var m cbor.Map
			for k, v := range ef {
				m = append(m, cbor.Pair{Key: k, Val: v})
			}
			return mustCBOR(append(m, cbor.Pair{Key: "x", Val: uint64(1)}))
		}(), "key-type"},
		{"envelope-version-2", "magic bbe 0x02: claims nothing this version reads", ef.with(boxrec.EKMagic, []byte{'b', 'b', 'e', 0x02}).encode(), "magic"},
		{"envelope-receipt-magic", "a receipt's magic where an envelope's belongs: admission reads it as a receipt and refuses it for that", ef.with(boxrec.EKMagic, boxrec.MagicReceipt).encode(), "magic"},
		{"envelope-no-office", "", ef.with(boxrec.EKOffice, drop).encode(), "missing"},
		{"envelope-office-bytes", "", ef.with(boxrec.EKOffice, []byte(Office)).encode(), "type"},
		{"envelope-office-readable-only", "the readable part alone never names an office", ef.with(boxrec.EKOffice, "example_office").encode(), "office"},
		{"envelope-office-uppercase", "", ef.with(boxrec.EKOffice, "example_office_QZXKVBMWTR").encode(), "office"},
		{"envelope-to-alias", "02 || p + 1, which go-sdk parses as the point with x = 1", ef.with(boxrec.EKTo, aliasKey()).encode(), "identity"},
		{"envelope-to-uncompressed", "", ef.with(boxrec.EKTo, priv(RecipientKey).PubKey().Uncompressed()).encode(), "identity"},
		{"envelope-box-leading-digit", "", ef.with(boxrec.EKBox, "1inbox").encode(), "box"},
		{"envelope-box-trailing-underscore", "", ef.with(boxrec.EKBox, "inbox_").encode(), "box"},
		{"envelope-box-51", "", ef.with(boxrec.EKBox, "b"+strings.Repeat("x", 50)).encode(), "box"},
		{"envelope-no-from", "", ef.with(boxrec.EKFrom, drop).encode(), "missing"},
		{"envelope-created-zero", "", ef.with(boxrec.EKCreated, uint64(0)).encode(), "range"},
		{"envelope-created-beyond-9999", "", ef.with(boxrec.EKCreated, uint64(boxrec.MaxTime+1)).encode(), "range"},
		{"envelope-created-negative", "", ef.with(boxrec.EKCreated, int64(-1)).encode(), "type"},
		{"envelope-expires-at-created", "expires is zero or after created", ef.with(boxrec.EKExpires, base.env.Created).encode(), "expires"},
		{"envelope-content-empty", "", ef.with(boxrec.EKContent, []byte{}).encode(), "range"},
		{"envelope-content-16385", "one byte over the content bound", ef.with(boxrec.EKContent, bytes.Repeat([]byte{'x'}, boxrec.MaxContent+1)).encode(), "range"},
		{"envelope-content-text", "content is a byte string, not a text string", ef.with(boxrec.EKContent, string(base.env.Content)).encode(), "type"},
	}
	for _, r := range record {
		if err := addEnv(r.name, r.note, r.rec, r.want); err != nil {
			return nil, nil, err
		}
	}

	// Content rules: each record is valid; its content breaks one rule.
	withContent := func(c []byte) []byte { return ef.with(boxrec.EKContent, c).encode() }
	docCase := func(mod func(o *boxrec.Object)) []byte {
		o := cloneDoc(base.doc)
		mod(o)
		resign(o)
		return withContent(mustCanon(o))
	}
	b64 := base64.StdEncoding.EncodeToString(base.brc78)
	canon := string(base.env.Content)
	selfCipher, err := BRC78([]byte(plainNote), SenderKey, SenderKey, base.keyID, base.iv)
	if err != nil {
		return nil, nil, err
	}
	signed := mustCanon(base.doc.Without(boxrec.MemberContent, boxrec.MemberSignature))
	content := []struct {
		name, note string
		rec        []byte
		want       string
	}{
		{"content-not-json", "", withContent([]byte("hello")), "content-json"},
		{"content-array", "a JSON array, not an object", withContent([]byte("[]")), "content-json"},
		{"content-white-space", "the same object with a space: not its canonical serialization", withContent([]byte("{ " + canon[1:])), "content-json"},
		{"content-duplicate-member", "a member name given twice", withContent([]byte(`{"metanetHandles":"1.0",` + canon[1:])), "content-json"},
		{"content-fraction", "numbers are integers only", withContent([]byte(`{"a":1.5,` + canon[1:])), "content-json"},
		{"content-lone-surrogate", "an escaped lone surrogate", withContent([]byte(`{"a":"\ud800",` + canon[1:])), "content-json"},
		{"content-escaped-letter", "an escape the canonical form never writes", withContent([]byte(`{"a":"\u0041",` + canon[1:])), "content-json"},
		{"content-version", "metanetHandles other than 1.0", docCase(func(o *boxrec.Object) { o.Set(boxrec.MemberVersion, "1.1") }), "content-shape"},
		{"content-sender-mismatch", "sender.identityKey is not the record's from", docCase(func(o *boxrec.Object) {
			s, _ := o.Get(boxrec.MemberSender)
			s.(*boxrec.Object).Set(boxrec.MemberIdentity, h(Recipient()))
		}), "content-shape"},
		{"content-recipient-no-key", "recipient without identityKey", docCase(func(o *boxrec.Object) {
			o.Set(boxrec.MemberRecipient, &boxrec.Object{Members: []boxrec.Member{{Name: "handle", Value: "robin"}}})
		}), "content-shape"},
		{"content-recipient-handle-number", "a recipient claim that is not a string", docCase(func(o *boxrec.Object) {
			s, _ := o.Get(boxrec.MemberRecipient)
			s.(*boxrec.Object).Set("handle", boxrec.Int(7))
		}), "content-shape"},
		{"content-created-mismatch", "created is not the record's created", docCase(func(o *boxrec.Object) { o.Set(boxrec.MemberCreated, boxrec.RFC3339(base.env.Created+1)) }), "content-shape"},
		{"content-created-offset", "the same instant written with an offset: only YYYY-MM-DDTHH:MM:SSZ", docCase(func(o *boxrec.Object) { o.Set(boxrec.MemberCreated, "2026-01-01T00:00:00+00:00") }), "content-shape"},
		{"content-payment-object", "a payment in the clear: payment is null and a payment rides inside the encrypted content", docCase(func(o *boxrec.Object) {
			o.Set(boxrec.MemberPayment, &boxrec.Object{Members: []boxrec.Member{{Name: "satoshis", Value: boxrec.Int(1000)}}})
		}), "content-shape"},
		{"content-payment-missing", "", docCase(func(o *boxrec.Object) { *o = *o.Without(boxrec.MemberPayment) }), "content-shape"},
		{"content-quote-number", "", docCase(func(o *boxrec.Object) { o.Set(boxrec.MemberQuoteID, boxrec.Int(1)) }), "content-shape"},
		{"content-cipher-truncated", "base64 with its last character removed", docCase(func(o *boxrec.Object) { o.Set(boxrec.MemberContent, b64[:len(b64)-1]) }), "content-cipher"},
		{"content-cipher-line-break", "standard base64 broken by a line break after 64 characters, which some decoders skip", docCase(func(o *boxrec.Object) { o.Set(boxrec.MemberContent, b64[:64]+"\n"+b64[64:]) }), "content-cipher"},
		{"content-cipher-short", "149 bytes: one under the shortest BRC-78 message", docCase(func(o *boxrec.Object) {
			o.Set(boxrec.MemberContent, base64.StdEncoding.EncodeToString(base.brc78[:boxrec.BRC78Min-1]))
		}), "content-cipher"},
		{"content-cipher-version-prose-order", "version bytes 10 33 42 42, as the BRC-78 hex example writes them; go-sdk writes 42 42 10 33", docCase(func(o *boxrec.Object) {
			c := append([]byte{0x10, 0x33, 0x42, 0x42}, base.brc78[4:]...)
			o.Set(boxrec.MemberContent, base64.StdEncoding.EncodeToString(c))
		}), "content-cipher"},
		{"content-cipher-wrong-recipient", "a BRC-78 message the sender encrypted to itself", docCase(func(o *boxrec.Object) {
			o.Set(boxrec.MemberContent, base64.StdEncoding.EncodeToString(selfCipher))
		}), "content-cipher"},
		{"content-signature-identity-key", "signed by the sender's identity key itself, as BRC-169 section 7.2 writes it; this contract signs under the derived signature key", docCase2(base, sigOver(priv(SenderKey), signed)), "content-signature"},
		{"content-signature-envelope-key", "signed under the carrier's key id envelope, not signature", docCase2(base, sigOver(derivedPriv(SenderKey, boxrec.EnvelopeDerivation), signed)), "content-signature"},
		{"content-signature-recipient", "signed by the recipient's signature key", docCase2(base, sigOver(derivedPriv(RecipientKey, boxrec.SignatureDerivation), signed)), "content-signature"},
		{"content-signature-high-s", "the valid signature with S replaced by n - S", docCase2(base, highS(sigOver(derivedPriv(SenderKey, boxrec.SignatureDerivation), signed))), "content-signature"},
		{"content-signature-uppercase", "", docCase2(base, strings.ToUpper(sigOver(derivedPriv(SenderKey, boxrec.SignatureDerivation), signed))), "content-signature"},
		{"content-signature-over-content", "signed over the serialization with content included", docCase2(base, sigOver(derivedPriv(SenderKey, boxrec.SignatureDerivation), mustCanon(base.doc.Without(boxrec.MemberSignature)))), "content-signature"},
	}
	for _, c := range content {
		if err := addEnv(c.name, c.note, c.rec, c.want); err != nil {
			return nil, nil, err
		}
	}

	// Receipts.
	rf := fields{
		boxrec.RKMagic: boxrec.MagicReceipt, boxrec.RKOffice: Office, boxrec.RKBy: Recipient(),
		boxrec.RKAcks: acksValue(sortedCommitments(2, 1)), boxrec.RKCreated: uint64(t0 + 3600),
	}
	two := sortedCommitments(2, 1)
	rcases := []struct {
		name, note string
		rec        []byte
		want       string
	}{
		{"receipt-acks-empty", "", rf.with(boxrec.RKAcks, []cbor.Value{}).encode(), "acks"},
		{"receipt-acks-descending", "acks are strictly ascending", rf.with(boxrec.RKAcks, acksValue([][32]byte{two[1], two[0]})).encode(), "acks"},
		{"receipt-acks-duplicate", "", rf.with(boxrec.RKAcks, acksValue([][32]byte{two[0], two[0]})).encode(), "acks"},
		{"receipt-acks-31-bytes", "", rf.with(boxrec.RKAcks, []cbor.Value{two[0][:31]}).encode(), "acks"},
		{"receipt-acks-65", "one over the most a receipt names", rf.with(boxrec.RKAcks, acksValue(sortedCommitments(boxrec.MaxAcks+1, 100))).encode(), "acks"},
		{"receipt-acks-not-array", "", rf.with(boxrec.RKAcks, two[0][:]).encode(), "type"},
		{"receipt-by-alias", "", rf.with(boxrec.RKBy, aliasKey()).encode(), "identity"},
		{"receipt-no-created", "", rf.with(boxrec.RKCreated, drop).encode(), "missing"},
		{"receipt-envelope-magic", "an envelope's magic where a receipt's belongs: admission reads it as an envelope and refuses it for that", rf.with(boxrec.RKMagic, boxrec.MagicEnvelope).encode(), "magic"},
	}
	for _, c := range rcases {
		j := refusalJSON{Name: c.name, Kind: "receipt", Note: c.note, Record: h(c.rec), AtAdmission: "refuse"}
		k, ar := admit(c.rec)
		j.Claims, j.AdmissionReason = k.String(), ar
		if k == boxrec.KindNone {
			j.AtAdmission = "skip"
		}
		_, err := boxrec.DecodeReceipt(c.rec)
		if err == nil {
			return nil, nil, fmt.Errorf("%s: not refused", c.name)
		}
		if j.Reason = boxrec.Reason(err); j.Reason != c.want {
			return nil, nil, fmt.Errorf("%s: refused %q (%v), want %q", c.name, j.Reason, err, c.want)
		}
		out = append(out, j)
	}
	return map[string]any{
		"description": "Records refused by the record and content rules (docs/spec.md sections 3.1, 4.6 and 5), each with its reason as the record of its kind, and what admission does with an output whose first field it is (section 8.1): skip one that claims no record, or refuse the transaction for the reason of the record the output claims (admissionClaims, admissionReason), which differs from kind and reason where the magic names the other record. Every one is decided by the record alone. Content cases carry a record that passes the record rules; their content breaks exactly one content rule, and every earlier content rule holds (signatures are valid for the altered document unless the case is a signature case).",
		"refusals":    out,
	}, records, nil
}

// docCase2 replaces only the signature member of the base envelope.
func docCase2(base *sealed, sig string) []byte {
	o := cloneDoc(base.doc)
	o.Set(boxrec.MemberSignature, sig)
	return envelopeFields(base.env).with(boxrec.EKContent, mustCanon(o)).encode()
}

func acksValue(cs [][32]byte) []cbor.Value {
	out := make([]cbor.Value, len(cs))
	for i := range cs {
		out[i] = append([]byte(nil), cs[i][:]...)
	}
	return out
}

func mustCBOR(v cbor.Value) []byte {
	b, err := cbor.Encode(v)
	if err != nil {
		panic(err)
	}
	return b
}
