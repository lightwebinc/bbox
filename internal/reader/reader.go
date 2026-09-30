// Package reader asks overlay hosts the ls_bbox questions and checks every
// answer itself (docs/spec.md sections 7.2, 9 and 13): the carrier's rules
// and content rules (the admission rules a host applies), SPV of the carrier
// through its funding parent against the reader's own headers, and the
// record's office and recipient. A host is trusted for nothing: an answer is
// verified here or reported as refused, naming the host. An empty answer
// means only that the host holds nothing that answers.
//
// A reader that asks several hosts compares their answers and reports a
// difference as such, never merging it silently: a host can withhold an
// envelope, and only a comparison shows it.
package reader

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/hostset"
	"github.com/lightwebinc/bcommon/lookup"

	"github.com/lightwebinc/bbox/internal/boxrec"
	"github.com/lightwebinc/bbox/internal/limits"
)

// MaxBEEF is the most a reader takes for one answered carrier: a carrier
// is its record and its funding tree, and a tree of 1000 outputs makes a
// carrier of about 70 KB (spec section 12).
const MaxBEEF = 1 << 20

// Client asks hosts and checks their answers.
type Client struct {
	// Hosts are the overlay hosts' base URLs, asked in order.
	Hosts []string
	// Headers is the reader's own header source. It is required: an answer
	// is only as good as the headers it is checked against.
	Headers chaintracker.ChainTracker
	// Timeout bounds each request; HTTP, when set, is the client used.
	Timeout time.Duration
	HTTP    *http.Client
	// MaxPages bounds the pages one question reads per host.
	MaxPages int
}

// Query is one ls_bbox question: its members, every one a string.
type Query map[string]string

// ErrHost is a host that answered with an error: its words travel with it.
var ErrHost = errors.New("reader: the host refused the question")

// Ask asks one host one question and returns the outputs it answered.
func (c *Client) Ask(ctx context.Context, host string, q Query) ([]lookup.Output, error) {
	hs := &hostset.Client{Timeout: c.Timeout, HTTP: c.HTTP}
	res, err := lookup.Query(ctx, hs, host, lookup.Question{Service: boxrec.LookupService, Query: q})
	if err != nil {
		if strings.Contains(err.Error(), "answered status 4") {
			return nil, fmt.Errorf("%w: %v", ErrHost, err)
		}
		return nil, err
	}
	var out []lookup.Output
	for _, r := range res {
		out = append(out, r.Answer.Outputs...)
	}
	return out, nil
}

// Item is one answered carrier that verified.
type Item struct {
	Txid string
	// Beef is the carrier's BEEF as answered.
	Beef []byte
	Tx   *transaction.Transaction
	// Admission is what the carrier is, by the host's own rules.
	Admission *boxrec.Admission
}

// Envelope is the item's envelope record, or nil.
func (it *Item) Envelope() *boxrec.Envelope {
	if it.Admission == nil || it.Admission.Carrier == nil {
		return nil
	}
	return it.Admission.Carrier.Envelope
}

// Created is the envelope's created, or zero.
func (it *Item) Created() uint64 {
	if e := it.Envelope(); e != nil {
		return e.Created
	}
	return 0
}

// Refusal is an answered output that did not verify.
type Refusal struct {
	Host   string
	Txid   string
	Reason string
	Err    error
}

// Verify checks one answered carrier (spec section 13 steps 1 and 2): the
// host's rules for office (the carrier and its content), SPV through its
// funding parent against headers, and, for an envelope, that it is to the
// recipient when one is named. want is the kind the question answers.
func Verify(ctx context.Context, beef []byte, office string, want boxrec.TxKind, to []byte, headers chaintracker.ChainTracker) (*Item, error) {
	if headers == nil {
		return nil, errors.New("reader: no header source; refusing to verify against a default")
	}
	_, tx, txid, err := guard.ParseBEEF(beef, MaxBEEF)
	if err != nil || tx == nil {
		return nil, fmt.Errorf("%w: %v", boxrec.ErrBEEF, err)
	}
	a, err := boxrec.Admit(ctx, beef, nil, boxrec.Host{Office: office, Headers: headers, MaxBEEF: MaxBEEF})
	if err != nil {
		return nil, err
	}
	if a.Kind != want {
		return nil, fmt.Errorf("reader: the answer is a %s, not a %s", a.Kind, want)
	}
	if a.Kind == boxrec.TxEnvelope || a.Kind == boxrec.TxReceipt {
		ok, err := spv.Verify(ctx, tx, headers, nil)
		if err != nil || !ok {
			return nil, fmt.Errorf("reader: SPV of the carrier through its funding parent: %v", err)
		}
	}
	if to != nil && a.Carrier != nil && a.Carrier.Envelope != nil && !bytes.Equal(a.Carrier.Envelope.To, to) {
		return nil, errors.New("reader: the envelope is to another key")
	}
	return &Item{Txid: txid.String(), Beef: beef, Tx: tx, Admission: a}, nil
}

// HostAnswer is what one host answered to one question, all pages.
type HostAnswer struct {
	Host    string
	Items   []*Item
	Refused []Refusal
	// Err is set when the host could not be asked.
	Err error
	// Truncated is set when the page bound stopped the reading.
	Truncated bool
}

// cursor is the -after member for the last envelope of a page.
func cursor(it *Item) string {
	return strconv.FormatUint(it.Created(), 10) + ":" + it.Txid
}

// Pages asks one host an envelope class, page after page, until a page is
// short. office is the question's office; to, when set, the recipient every
// envelope must be to.
func (c *Client) Pages(ctx context.Context, host string, q Query, to []byte) HostAnswer {
	ha := HostAnswer{Host: host}
	maxPages := c.MaxPages
	if maxPages <= 0 {
		maxPages = limits.MaxPages
	}
	page := Query{}
	for k, v := range q {
		page[k] = v
	}
	for n := 0; ; n++ {
		if n == maxPages {
			ha.Truncated = true
			return ha
		}
		outs, err := c.Ask(ctx, host, page)
		if err != nil {
			ha.Err = err
			return ha
		}
		var last *Item
		for _, o := range outs {
			it, err := Verify(ctx, o.Beef, q["office"], boxrec.TxEnvelope, to, c.Headers)
			if err != nil {
				ha.Refused = append(ha.Refused, Refusal{Host: host, Txid: subjectID(o.Beef), Reason: boxrec.Reason(err), Err: err})
				continue
			}
			ha.Items = append(ha.Items, it)
			if last == nil || less(last, it) {
				last = it
			}
		}
		if len(outs) < limits.PageSize || last == nil {
			return ha
		}
		page["after"] = cursor(last)
	}
}

func less(a, b *Item) bool {
	if a.Created() != b.Created() {
		return a.Created() < b.Created()
	}
	return a.Txid < b.Txid
}

// subjectID is the txid of a BEEF's subject: the one an Atomic BEEF names
// in its header, whether or not the rest parses, else the one the BEEF
// parses to, else a note that there is none.
func subjectID(beef []byte) string {
	if len(beef) >= 36 && binary.LittleEndian.Uint32(beef) == transaction.ATOMIC_BEEF {
		return chainhash.Hash(beef[4:36]).String()
	}
	_, _, id, err := transaction.ParseBeef(beef)
	if err != nil || id == nil {
		return "(no subject)"
	}
	return id.String()
}

// Listing is one question asked of every host: the envelopes that verified,
// with the hosts that answered each, and what each host refused or lacked.
type Listing struct {
	Items []*Item
	// By maps each item's txid to the hosts that answered it.
	By      map[string][]string
	Answers []HostAnswer
}

// Ask asks every host the question, page after page, and compares.
func (c *Client) List(ctx context.Context, q Query, to []byte) (*Listing, error) {
	if len(c.Hosts) == 0 {
		return nil, errors.New("no overlay host configured (config key hosts, or -hosts)")
	}
	l := &Listing{By: map[string][]string{}}
	seen := map[string]*Item{}
	for _, h := range c.Hosts {
		ha := c.Pages(ctx, h, q, to)
		l.Answers = append(l.Answers, ha)
		for _, it := range ha.Items {
			if _, ok := seen[it.Txid]; !ok {
				seen[it.Txid] = it
				l.Items = append(l.Items, it)
			}
			l.By[it.Txid] = append(l.By[it.Txid], h)
		}
	}
	sort.Slice(l.Items, func(i, j int) bool { return less(l.Items[i], l.Items[j]) })
	return l, nil
}

// Answered is how many hosts answered at all.
func (l *Listing) Answered() int {
	n := 0
	for _, a := range l.Answers {
		if a.Err == nil {
			n++
		}
	}
	return n
}

// Missing are, per host that answered, the verified envelopes another host
// answered and it did not.
func (l *Listing) Missing() map[string][]string {
	out := map[string][]string{}
	for _, a := range l.Answers {
		if a.Err != nil {
			continue
		}
		has := map[string]bool{}
		for _, it := range a.Items {
			has[it.Txid] = true
		}
		for _, it := range l.Items {
			if !has[it.Txid] {
				out[a.Host] = append(out[a.Host], it.Txid)
			}
		}
	}
	return out
}

// Agree reports whether every host that answered answered the same set.
func (l *Listing) Agree() bool { return len(l.Missing()) == 0 }

// Refusals are every output some host answered that did not verify.
func (l *Listing) Refusals() []Refusal {
	var out []Refusal
	for _, a := range l.Answers {
		out = append(out, a.Refused...)
	}
	return out
}

// Find returns the item with txid, if a host answered it.
func (l *Listing) Find(txid string) *Item {
	for _, it := range l.Items {
		if it.Txid == txid {
			return it
		}
	}
	return nil
}

// Held asks whether host answers a question with an output whose subject
// is txid: a unicast publisher's confirmation that a host that admitted
// nothing holds the object (spec section 9).
func (c *Client) Held(ctx context.Context, host string, q Query, txid string) (bool, error) {
	outs, err := c.Ask(ctx, host, q)
	if err != nil {
		return false, err
	}
	for _, o := range outs {
		if subjectID(o.Beef) == txid {
			return true, nil
		}
	}
	return false, nil
}

// Terms is a host's terms document (spec section 7.4).
type Terms struct {
	Service string `json:"service"`
	Terms   int    `json:"terms"`
	Classes []struct {
		Class    string `json:"class"`
		Satoshis uint64 `json:"satoshis"`
	} `json:"classes"`
}

// ErrNoTerms is a host that serves no terms document: it charges for
// nothing.
var ErrNoTerms = errors.New("reader: the host serves no terms document, so it charges for nothing")

// FetchTerms reads <base>/ls_bbox/terms. Members and classes a client does
// not know are ignored; a free class with a price is reported, and a client
// never pays for one.
func FetchTerms(ctx context.Context, hc *http.Client, base string) (*Terms, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/"+boxrec.LookupService+"/terms", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNoTerms
	default:
		return nil, fmt.Errorf("terms: status %d", resp.StatusCode)
	}
	var t Terms
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("terms: not the terms document: %w", err)
	}
	if t.Service != boxrec.LookupService || t.Terms != 1 {
		return nil, fmt.Errorf("terms: service %q version %d, not ls_bbox version 1", t.Service, t.Terms)
	}
	return &t, nil
}

// Hash is a display txid as a hash (hash byte order).
func Hash(txid string) ([32]byte, error) {
	h, err := chainhash.NewHashFromHex(txid)
	if err != nil || h.String() != txid {
		return [32]byte{}, fmt.Errorf("%q is not a transaction id (64 lowercase hex characters)", txid)
	}
	return [32]byte(*h), nil
}
