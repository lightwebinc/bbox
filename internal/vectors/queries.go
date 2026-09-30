package vectors

import (
	"encoding/json"
	"fmt"

	"github.com/lightwebinc/bbox/internal/boxrec"
)

type queryJSON struct {
	Name     string `json:"name"`
	Note     string `json:"note,omitempty"`
	Question string `json:"question"`
	Class    string `json:"class,omitempty"`
	Free     *bool  `json:"free,omitempty"`
	Page     int    `json:"page,omitempty"`
	Refused  string `json:"refused,omitempty"`
}

// queries lists questions as the JSON text a client sends in the query
// member, each with the class it is or the reason it is refused.
func queries() (map[string]any, error) {
	to := h(Recipient())
	env := displayTxid(commitment(1))
	after := fmt.Sprintf("%d:%s", t0, env)
	alias := "02" + "fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc30"
	cases := []struct{ name, note, q string }{
		{"inbox", "every unacknowledged envelope to a key, all boxes", fmt.Sprintf(`{"office":%q,"to":%q}`, Office, to)},
		{"inbox-after", "the next page after a cursor", fmt.Sprintf(`{"office":%q,"to":%q,"after":%q}`, Office, to, after)},
		{"box", "one box", fmt.Sprintf(`{"office":%q,"to":%q,"box":"inbox"}`, Office, to)},
		{"box-after", "one box, the next page", fmt.Sprintf(`{"office":%q,"to":%q,"box":"inbox","after":%q}`, Office, to, after)},
		{"box-since", "a cursor of a time and 64 zeros asks for everything created at or after that second", fmt.Sprintf(`{"office":%q,"to":%q,"box":"inbox","after":"%d:%064d"}`, Office, to, t0, 0)},
		{"sender", "the open envelopes to a key from one sender, all boxes", fmt.Sprintf(`{"office":%q,"to":%q,"from":%q}`, Office, to, h(Sender()))},
		{"sender-after", "the same, the next page", fmt.Sprintf(`{"office":%q,"to":%q,"from":%q,"after":%q}`, Office, to, h(Sender()), after)},
		{"sweep", "the admitted sweeps that spend one outpoint", fmt.Sprintf(`{"office":%q,"spent":"%s.0"}`, Office, env)},
		{"receipt", "the receipts by a recipient that acknowledge one envelope", fmt.Sprintf(`{"office":%q,"by":%q,"receiptFor":%q}`, Office, to, env)},
		{"history", "priceable: acknowledged or expired envelopes the host still keeps", fmt.Sprintf(`{"office":%q,"history":%q}`, Office, to)},
		{"history-after", "priceable, the next page", fmt.Sprintf(`{"office":%q,"history":%q,"after":%q}`, Office, to, after)},
		{"repeated-member", "a member given twice takes its last value, as JSON parsers do", fmt.Sprintf(`{"office":%q,"to":"not a key","to":%q}`, Office, to)},

		{"extra-member", "an unknown member is refused, never ignored: ignoring it would let any word make a priced alias of a free class", fmt.Sprintf(`{"office":%q,"to":%q,"fast":"yes"}`, Office, to)},
		{"priced-superset", "a free class plus a priced member is no class", fmt.Sprintf(`{"office":%q,"to":%q,"history":%q}`, Office, to, to)},
		{"no-office", "every class names its office", fmt.Sprintf(`{"to":%q}`, to)},
		{"empty", "", `{}`},
		{"readable-part-only", "the readable part alone never names an office", fmt.Sprintf(`{"office":"example_office","to":%q}`, to)},
		{"office-uppercase", "", fmt.Sprintf(`{"office":"example_office_QZXKVBMWTR","to":%q}`, to)},
		{"to-uppercase", "keys are lowercase hex", fmt.Sprintf(`{"office":%q,"to":%q}`, Office, to[:2]+upper(to[2:]))},
		{"to-alias", "02 || p + 1 is an alias of the point with x = 1, which some parsers accept", fmt.Sprintf(`{"office":%q,"to":%q}`, Office, alias)},
		{"to-number", "every member is a string", fmt.Sprintf(`{"office":%q,"to":7}`, Office)},
		{"box-leading-digit", "", fmt.Sprintf(`{"office":%q,"to":%q,"box":"1inbox"}`, Office, to)},
		{"after-leading-zero", "", fmt.Sprintf(`{"office":%q,"to":%q,"after":"0%d:%s"}`, Office, to, t0, env)},
		{"after-no-txid", "", fmt.Sprintf(`{"office":%q,"to":%q,"after":"%d"}`, Office, to, t0)},
		{"after-beyond-9999", "", fmt.Sprintf(`{"office":%q,"to":%q,"after":"%d:%s"}`, Office, to, uint64(boxrec.MaxTime)+1, env)},
		{"sweep-leading-zero", "an output index is decimal with no leading zero", fmt.Sprintf(`{"office":%q,"spent":"%s.01"}`, Office, env)},
		{"sweep-colon", "an outpoint is txid.vout", fmt.Sprintf(`{"office":%q,"spent":"%s:0"}`, Office, env)},
		{"sweep-plus-to", "", fmt.Sprintf(`{"office":%q,"spent":"%s.0","to":%q}`, Office, env, to)},
		{"receipt-short-txid", "", fmt.Sprintf(`{"office":%q,"by":%q,"receiptFor":%q}`, Office, to, env[:63])},
	}
	var out []queryJSON
	for _, c := range cases {
		var m map[string]any
		if err := json.Unmarshal([]byte(c.q), &m); err != nil {
			return nil, fmt.Errorf("%s: %w", c.name, err)
		}
		j := queryJSON{Name: c.name, Note: c.note, Question: c.q}
		q, err := boxrec.ParseQuery(m)
		if err != nil {
			j.Refused = boxrec.Reason(err)
		} else {
			free := q.Class.Free
			j.Class, j.Free, j.Page = q.Class.Name, &free, q.Class.Page
		}
		out = append(out, j)
	}
	var classes []map[string]any
	for _, c := range boxrec.Classes {
		classes = append(classes, map[string]any{"class": c.Name, "members": c.Members, "free": c.Free, "page": c.Page})
	}
	return map[string]any{
		"description": "ls_bbox questions (docs/spec.md section 7.2): the query member of a BRC-24 lookup, as JSON text. Each is either a class, with whether it is free on every conforming host and the most outputs one answer holds, or refused. Txids in questions are display order.",
		"service":     boxrec.LookupService,
		"classes":     classes,
		"questions":   out,
	}, nil
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}
