package boxrec

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/pushdrop"
)

func key(b byte) []byte {
	p, _ := ec.PrivateKeyFromBytes(bytes.Repeat([]byte{b}, 32))
	return p.PubKey().Compressed()
}

const office = "example_office_qzxkvbmwtr"

// TestProtocolName runs the SDK: the application name alone cannot derive,
// and every bbox derivation can.
func TestProtocolName(t *testing.T) {
	p, _ := ec.PrivateKeyFromBytes(bytes.Repeat([]byte{0x42}, 32))
	kd := wallet.NewKeyDeriver(p)
	if _, err := kd.DerivePublicKey(wallet.Protocol{SecurityLevel: 1, Protocol: "bbox"}, KeyEnvelope, pushdrop.Anyone(), true); err == nil {
		t.Fatal(`go-sdk derived under [1, "bbox"]`)
	}
	for _, d := range []pushdrop.Derivation{EnvelopeDerivation, SignatureDerivation, FundDerivation} {
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
		if _, err := kd.DerivePublicKey(d.Protocol, d.KeyID, pushdrop.Anyone(), true); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOfficeGrammar(t *testing.T) {
	for s, ok := range map[string]bool{
		office:                                  true,
		"a_abcdefghij":                          true,
		strings.Repeat("a", 31) + "_abcdefghij": true,
		strings.Repeat("a", 32) + "_abcdefghij": false, // topic over 50
		"example_office":                        false,
		"_a_abcdefghij":                         false,
		"a__b_abcdefghij":                       false,
		"a_b__abcdefghij":                       false,
		"a1_abcdefghij":                         false,
		"a_abcdefghiJ":                          false,
		"a_abcdefghi":                           false,
		"a-abcdefghij":                          false,
	} {
		if got := CheckOffice(s) == nil; got != ok {
			t.Errorf("%q: %v", s, got)
		}
		if ok {
			if topic, _ := Topic(s); len(topic) > 50 {
				t.Errorf("%q: topic %d characters", s, len(topic))
			}
		}
	}
}

func TestNewOffice(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		o, err := NewOffice("team", nil)
		if err != nil || CheckOffice(o) != nil {
			t.Fatal(o, err)
		}
		seen[o] = true
	}
	if len(seen) != 200 {
		t.Fatal("suffixes repeat")
	}
	if _, err := NewOffice("Team", nil); err == nil {
		t.Fatal("bad name accepted")
	}
}

func TestBoxGrammar(t *testing.T) {
	for s, ok := range map[string]bool{
		"inbox": true, "payment_inbox": true, "box2": true, "a": true,
		strings.Repeat("b", 50): true, strings.Repeat("b", 51): false,
		"2box": false, "_box": false, "box_": false, "a__b": false, "Box": false, "": false, "a-b": false,
	} {
		if got := CheckBox(s) == nil; got != ok {
			t.Errorf("%q: %v", s, got)
		}
	}
}

func envelope() *Envelope {
	return &Envelope{Office: office, To: key(0x43), Box: "inbox", From: key(0x42), Created: 1767225600, Content: []byte("{}")}
}

// TestKeyCount: a record with 64 keys decodes and re-encodes; 65 is
// refused both ways.
func TestKeyCount(t *testing.T) {
	e := envelope()
	for k := uint64(8); k < 64; k++ {
		e.Extra = append(e.Extra, cbor.Pair{Key: k, Val: k})
	}
	b, err := e.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeEnvelope(b)
	if err != nil || len(back.Extra) != 56 {
		t.Fatal(err)
	}
	e.Extra = append(e.Extra, cbor.Pair{Key: uint64(64), Val: uint64(0)})
	if _, err := e.Encode(); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	m := cbor.Map{}
	for k := uint64(0); k < 65; k++ {
		m = append(m, cbor.Pair{Key: k, Val: uint64(0)})
	}
	raw, _ := cbor.Encode(m)
	if _, err := DecodeEnvelope(raw); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
}

// TestExtraCannotShadow: a preserved key at or below the last defined key
// is refused on encode.
func TestExtraCannotShadow(t *testing.T) {
	e := envelope()
	e.Extra = cbor.Map{{Key: uint64(EKContent), Val: []byte("x")}}
	if _, err := e.Encode(); !errors.Is(err, ErrKeyType) {
		t.Fatal(err)
	}
}

// TestClassifier recognises every definite-length map head.
func TestClassifier(t *testing.T) {
	tail := append([]byte{0x00, 0x44}, MagicEnvelope...)
	for _, head := range [][]byte{{0xa8}, {0xb8, 30}, {0xb9, 0, 30}, {0xba, 0, 0, 0, 30}, {0xbb, 0, 0, 0, 0, 0, 0, 0, 30}} {
		if !IsEnvelope(append(append([]byte{}, head...), tail...)) {
			t.Errorf("head %x", head)
		}
	}
	for _, p := range [][]byte{nil, {0xa8}, {0xbf, 0x00, 0x44, 'b', 'b', 'e', 1}, {0x88, 0x00, 0x44, 'b', 'b', 'e', 1}} {
		if IsEnvelope(p) {
			t.Errorf("%x claimed", p)
		}
	}
	r := &Receipt{Office: office, By: key(0x43), Acks: [][32]byte{sha256.Sum256(nil)}, Created: 1}
	b, _ := r.Encode()
	if !IsReceipt(b) || IsEnvelope(b) {
		t.Fatal("receipt classifier")
	}
}

func TestJSONSubset(t *testing.T) {
	for _, s := range []string{
		"\xef\xbb\xbf{}", `{} x`, `{"a":1,"a":2}`, `{"a":01}`, `{"a":-0}`, `{"a":1e2}`, `{"a":1.0}`,
		`{"a":9007199254740992}`, "{\"a\":\"\x01\"}", "{\"a\":\"\xff\"}", `{"a":"\udc00"}`,
		`{"a":"\ud800x"}`, `{"a":tru}`, `{"a":1,}`, `[1,]`, `{"a":"\x"}`,
		strings.Repeat("[", 17) + strings.Repeat("]", 17),
	} {
		if _, err := ParseJSON([]byte(s)); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	for _, s := range []string{`{}`, `[]`, `{"a":-9007199254740991}`, strings.Repeat("[", 16) + strings.Repeat("]", 16), `"\ud83d\ude00"`, `{"a":"\ud83d\ude00"}`} {
		if _, err := ParseJSON([]byte(s)); err != nil {
			t.Errorf("%q refused: %v", s, err)
		}
	}
}

// TestCanonical checks RFC 8785's rules the subset reaches: members sorted
// by UTF-16 code units (which differs from UTF-8 byte order between U+FF61
// and U+1F600), strings escaped as JSON.stringify escapes them, no white
// space.
func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		`{"｡":1,"😀":2}`:                             `{"😀":2,"｡":1}`,
		`{ "b" : [ 1 , true , null ] , "a" : "x" }`: `{"a":"x","b":[1,true,null]}`,
		`"\u001f\b\f\n\r\t\"\\\/ é"`:                "\"\\u001f\\b\\f\\n\\r\\t\\\"\\\\/ é\"",
		`{"a":{"d":1,"c":-2}}`:                      `{"a":{"c":-2,"d":1}}`,
	} {
		v, err := ParseJSON([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		got, err := Canonical(v)
		if err != nil || string(got) != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// TestPricedNeverSupersetOfFree is the structural rule of section 7.3: no
// priced class contains every member of a free one.
func TestPricedNeverSupersetOfFree(t *testing.T) {
	for _, p := range Classes {
		if p.Free {
			continue
		}
		for _, f := range Classes {
			if !f.Free {
				continue
			}
			all := true
			for _, m := range f.Members {
				found := false
				for _, n := range p.Members {
					found = found || n == m
				}
				all = all && found
			}
			if all {
				t.Errorf("priced %s contains free %s", p.Name, f.Name)
			}
		}
	}
}

func TestRFC3339(t *testing.T) {
	if RFC3339(1767225600) != "2026-01-01T00:00:00Z" || RFC3339(MaxTime) != "9999-12-31T23:59:59Z" {
		t.Fatal(RFC3339(1767225600), RFC3339(MaxTime))
	}
}
