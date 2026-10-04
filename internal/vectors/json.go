package vectors

import (
	"context"
	"strings"

	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bbox/boxrec"
)

type jsonCase struct {
	Name      string `json:"name"`
	Note      string `json:"note,omitempty"`
	Input     string `json:"input"`
	InputHex  string `json:"inputHex"`
	Accepted  bool   `json:"accepted"`
	Canonical string `json:"canonical,omitempty"`
	// IsCanonical is whether the input is its own canonical form: a
	// content is admitted only when it is.
	IsCanonical bool `json:"isCanonical"`
}

func nest(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }

// nestObject is n objects, each the one member of the one around it.
func nestObject(n int) string {
	return strings.Repeat(`{"a":`, n-1) + "{}" + strings.Repeat("}", n-1)
}

// jsonVectors pins the JSON subset of spec section 4.1 case by case, so a
// host in another language parses, refuses and serializes as this one does.
func jsonVectors() map[string]any {
	cases := []struct{ name, note, in string }{
		{"max-integer", "2^53 - 1", `{"a":9007199254740991}`},
		{"min-integer", "-(2^53 - 1)", `{"a":-9007199254740991}`},
		{"integer-2-53", "2^53 is not exact as a double in every parser's hands: refused", `{"a":9007199254740992}`},
		{"integer-minus-2-53", "", `{"a":-9007199254740992}`},
		{"exponent", "an exponent, even one a double holds exactly", `{"a":1e+21}`},
		{"fraction", "", `{"a":1.5}`},
		{"fraction-zero", "an integer value written with a fraction", `{"a":1.0}`},
		{"minus-zero", "", `{"a":-0}`},
		{"leading-zero", "", `{"a":01}`},
		{"lone-high-surrogate", "", `{"a":"\ud800"}`},
		{"lone-low-surrogate", "", `{"a":"\udc00"}`},
		{"lone-surrogate-in-name", "names are strings too", `{"\udc00":1}`},
		{"surrogate-pair", "a pair is one code point, written raw in the canonical form", `{"a":"\ud83d\ude00"}`},
		{"duplicate-name", "", `{"a":1,"a":1}`},
		{"byte-order-mark", "", "\xef\xbb\xbf{}"},
		{"invalid-utf8", "", "{\"a\":\"\xff\"}"},
		{"raw-control", "a raw control character inside a string", "{\"a\":\"\x01\"}"},
		{"escaped-control", "the canonical form writes \\u001f in lowercase hex", `{"a":"\u001F"}`},
		{"escaped-letter", "parses; the canonical form writes the letter itself", `{"a":"\u0041"}`},
		{"line-separator", "U+2028 is written raw, as JSON.stringify writes it", "{\"a\":\"\xe2\x80\xa8\"}"},
		{"solidus", "\\/ parses as / and is written unescaped", `{"a":"\/"}`},
		{"utf16-order", "names sort by UTF-16 code units: U+1F600 (D83D DE00) before U+FF61, the reverse of their UTF-8 byte order", "{\"\xef\xbd\xa1\":1,\"\xf0\x9f\x98\x80\":2}"},
		{"white-space", "", `{ "b" : [ 1 , true , null ] , "a" : "x" }`},
		{"depth-16", "sixteen nested arrays: the top level counts as one", nest(16)},
		{"depth-17", "", nest(17)},
		{"object-depth-16", "sixteen nested objects, the top-level object being depth 1: the deepest content allowed", nestObject(16)},
		{"object-depth-17", "", nestObject(17)},
		{"trailing", "", `{} x`},
		{"proto-name", "a name some JavaScript objects treat specially is an ordinary name", `{"__proto__":1}`},
	}
	var out []jsonCase
	for _, c := range cases {
		j := jsonCase{Name: c.name, Note: c.note, Input: c.in, InputHex: h([]byte(c.in))}
		v, err := boxrec.ParseJSON([]byte(c.in))
		if err == nil {
			cb, err := boxrec.Canonical(v)
			if err == nil {
				j.Accepted, j.Canonical, j.IsCanonical = true, string(cb), string(cb) == c.in
			}
		}
		out = append(out, j)
	}
	return map[string]any{
		"description": "The JSON subset of docs/spec.md section 4.1, case by case: whether a value is in the subset, its RFC 8785 serialization, and whether the input is already that serialization. An envelope's content is admitted only when it is accepted and canonical. inputHex holds the exact input bytes (input is the same as a string, and cannot carry invalid UTF-8); JSON escapes in them are part of the input.",
		"cases":       out,
	}
}

type scriptCase struct {
	Name   string `json:"name"`
	Note   string `json:"note,omitempty"`
	Script string `json:"script"`
	Claims string `json:"claims"`
}

// scriptVectors pins the admission classifier of spec section 8.1 over raw
// locking-script bytes, with real scripts from bcommon's PushDrop.
func scriptVectors(built []*sealed) (map[string]any, error) {
	ctx := context.Background()
	w, err := wallet.NewCompletedProtoWallet(priv(SenderKey))
	if err != nil {
		return nil, err
	}
	envLock, err := boxrec.EnvelopeDerivation.Lock(ctx, w, "", [][]byte{built[0].record}, true)
	if err != nil {
		return nil, err
	}
	rec, err := (&boxrec.Receipt{Office: Office, By: Sender(), Acks: sortedCommitments(2, 1), Created: t0}).Encode()
	if err != nil {
		return nil, err
	}
	rcpLock, err := boxrec.EnvelopeDerivation.Lock(ctx, w, "", [][]byte{rec}, true)
	if err != nil {
		return nil, err
	}
	fundLock, err := boxrec.EnvelopeDerivation.Lock(ctx, w, "", [][]byte{boxrec.TagFunding}, false)
	if err != nil {
		return nil, err
	}
	env := envLock.Bytes()
	// Lock-after: the fields first, the key and OP_CHECKSIG last.
	after := append(append([]byte{}, env[35:]...), env[:35]...)
	// The same record behind a 65-byte uncompressed key.
	pub := priv(SenderKey).PubKey().Uncompressed()
	uncompressed := append(append([]byte{0x41}, pub...), env[34:]...)
	// A push that declares more bytes than the script holds.
	short := append(append([]byte{}, env[:35]...), 0x4d, 0xff, 0xff, 0xa8, 0x00)
	cases := []struct {
		name, note string
		s          []byte
	}{
		{"envelope-carrier", "the envelope-note record as bcommon's PushDrop locks it", env},
		{"receipt-carrier", "a receipt record as bcommon's PushDrop locks it", rcpLock.Bytes()},
		{"funding-output", "a funding output claims no record", fundLock.Bytes()},
		{"lock-after", "the fields before the key: not the layout this contract uses", after},
		{"uncompressed-key", "a 65-byte key push", uncompressed},
		{"truncated-push", "the record push declares more bytes than are present", short},
	}
	var out []scriptCase
	for _, c := range cases {
		out = append(out, scriptCase{Name: c.name, Note: c.note, Script: h(c.s), Claims: boxrec.ClaimOf(c.s).String()})
	}
	return map[string]any{
		"description": "The admission classifier of docs/spec.md section 8.1 over raw locking-script bytes: a 33-byte key push, OP_CHECKSIG, then a push whose data starts with a record's claim prefix. The lock rule compares the whole script afterwards; this file pins only which record an output claims.",
		"scripts":     out,
	}, nil
}
