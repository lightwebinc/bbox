package boxrec

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Query member names.
const (
	QOffice     = "office"
	QTo         = "to"
	QBox        = "box"
	QAfter      = "after"
	QBy         = "by"
	QReceiptFor = "receiptFor"
	QHistory    = "history"
	QFrom       = "from"
	QSpent      = "spent"
)

// Class is one ls_bbox question class: a set of query member names.
type Class struct {
	Name    string
	Members []string // sorted
	// Free classes are answered at no price on every conforming host.
	Free bool
	// Page is the most outputs one answer holds.
	Page int
}

// Page sizes.
const (
	PageEnvelopes = 64
	PageReceipts  = 8
	PageSweeps    = 8
)

// Classes is every question class ls_bbox answers, in the spec's order.
var Classes = []Class{
	{"inbox", []string{QOffice, QTo}, true, PageEnvelopes},
	{"inbox-after", []string{QAfter, QOffice, QTo}, true, PageEnvelopes},
	{"box", []string{QBox, QOffice, QTo}, true, PageEnvelopes},
	{"box-after", []string{QAfter, QBox, QOffice, QTo}, true, PageEnvelopes},
	{"sender", []string{QFrom, QOffice, QTo}, true, PageEnvelopes},
	{"sender-after", []string{QAfter, QFrom, QOffice, QTo}, true, PageEnvelopes},
	{"receipt", []string{QBy, QOffice, QReceiptFor}, true, PageReceipts},
	{"sweep", []string{QOffice, QSpent}, true, PageSweeps},
	{"history", []string{QHistory, QOffice}, false, PageEnvelopes},
	{"history-after", []string{QAfter, QHistory, QOffice}, false, PageEnvelopes},
}

// Question is a validated question.
type Question struct {
	Class  Class
	Office string
	// Key is the identity key the question is about: to, by or history.
	Key []byte
	// From is the sender a sender question is about.
	From []byte
	Box  string
	// After is the page cursor: created and a txid, display order.
	AfterCreated uint64
	AfterTxid    string
	// ReceiptFor is an envelope's txid, display order.
	ReceiptFor string
	// SpentTxid and SpentVout are the outpoint a sweep question is about,
	// txid in display order.
	SpentTxid string
	SpentVout uint32
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// parseKeyHex requires 66 lowercase hex digits of a canonical compressed key.
func parseKeyHex(s string) ([]byte, bool) {
	if !isLowerHex(s, 66) {
		return nil, false
	}
	b, _ := hex.DecodeString(s)
	return b, CheckIdentity(b) == nil
}

// ParseAfter parses a page cursor: the decimal created (1 to MaxTime, no
// leading zero), a colon, and 64 lowercase hex digits of a txid in display
// order.
func ParseAfter(s string) (uint64, string, bool) {
	c, t, ok := strings.Cut(s, ":")
	if !ok || len(c) == 0 || len(c) > 12 || c[0] == '0' || !isLowerHex(t, 64) {
		return 0, "", false
	}
	for i := 0; i < len(c); i++ {
		if c[i] < '0' || c[i] > '9' {
			return 0, "", false
		}
	}
	n, err := strconv.ParseUint(c, 10, 64)
	if err != nil || n > MaxTime {
		return 0, "", false
	}
	return n, t, true
}

// ParseOutpoint parses an outpoint as the engine writes one: 64 lowercase
// hex digits of a txid in display order, a dot, and the output index in
// decimal (0 to 4294967295, no leading zero).
func ParseOutpoint(s string) (string, uint32, bool) {
	t, v, ok := strings.Cut(s, ".")
	if !ok || !isLowerHex(t, 64) || len(v) == 0 || len(v) > 10 || (len(v) > 1 && v[0] == '0') {
		return "", 0, false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return "", 0, false
		}
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return "", 0, false
	}
	return t, uint32(n), true
}

// ParseQuery validates a BRC-24 query object as parsed (a member given twice
// has already taken its last value). It must carry exactly the members of
// one class, each a string meeting its rule; anything else is refused,
// never answered empty and never ignored.
func ParseQuery(q map[string]any) (*Question, error) {
	names := make([]string, 0, len(q))
	for k := range q {
		names = append(names, k)
	}
	sort.Strings(names)
	var cls *Class
	for i := range Classes {
		if strings.Join(Classes[i].Members, ",") == strings.Join(names, ",") {
			cls = &Classes[i]
		}
	}
	if cls == nil {
		return nil, fmt.Errorf("%w: members %v", ErrQuery, names)
	}
	out := &Question{Class: *cls}
	for _, n := range names {
		s, ok := q[n].(string)
		if !ok {
			return nil, fmt.Errorf("%w: %s is not a string", ErrQuery, n)
		}
		switch n {
		case QOffice:
			if CheckOffice(s) != nil {
				return nil, fmt.Errorf("%w: %s", ErrQuery, n)
			}
			out.Office = s
		case QTo, QBy, QHistory:
			k, ok := parseKeyHex(s)
			if !ok {
				return nil, fmt.Errorf("%w: %s", ErrQuery, n)
			}
			out.Key = k
		case QFrom:
			k, ok := parseKeyHex(s)
			if !ok {
				return nil, fmt.Errorf("%w: %s", ErrQuery, n)
			}
			out.From = k
		case QSpent:
			t, v, ok := ParseOutpoint(s)
			if !ok {
				return nil, fmt.Errorf("%w: %s", ErrQuery, n)
			}
			out.SpentTxid, out.SpentVout = t, v
		case QBox:
			if CheckBox(s) != nil {
				return nil, fmt.Errorf("%w: %s", ErrQuery, n)
			}
			out.Box = s
		case QAfter:
			c, t, ok := ParseAfter(s)
			if !ok {
				return nil, fmt.Errorf("%w: %s", ErrQuery, n)
			}
			out.AfterCreated, out.AfterTxid = c, t
		case QReceiptFor:
			if !isLowerHex(s, 64) {
				return nil, fmt.Errorf("%w: %s", ErrQuery, n)
			}
			out.ReceiptFor = s
		}
	}
	return out, nil
}
