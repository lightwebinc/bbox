package testchain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"

	"github.com/lightwebinc/bbox/boxrec"
)

// Host is a stand-in overlay host carrying bbox offices: /submit runs the
// engine's SPV and the bbox topic manager (boxrec.Admit) over the topic's
// previous coins, and /lookup answers the ls_bbox classes by the statuses
// of spec section 8.2 and the answer window of section 8.3. A refusal is
// recorded nowhere, so the same transaction offered again is decided
// afresh. The priced classes are refused on /lookup when Priced is set, as
// a host that sells them refuses them there. Retention, restore and the
// evidence cap are the real module's and are not modelled.
type Host struct {
	mu      sync.Mutex
	chain   *Chain
	offices map[string]bool
	applied map[string]bool
	// carriers by commitment (display txid), sweeps by txid.
	carriers map[string]*held
	sweeps   map[string]*sweepRow
	// coins are the outputs the host holds, by outpoint, with the topic.
	coins map[transaction.Outpoint]string
	// Now is the host's clock.
	Now func() time.Time
	// Down answers every request with 503.
	Down bool
	// Priced refuses the history classes on /lookup.
	Priced bool
	// Drop, when set, is asked about each submission; true discards it
	// unseen, as a plane that lost the object would.
	Drop func(tx *transaction.Transaction) bool
	// Tamper, when set, rewrites every answered BEEF, as a host that edits
	// what it serves would.
	Tamper func(beef []byte) []byte
	// Stale answers every carrier under a proof of its funding tree that
	// names another block than the one the chain mined it in: a host whose
	// stored proofs a reorganization left behind.
	Stale bool
	// HistoryPage, when set, is the most envelopes a history page holds
	// here, in place of the class's 64.
	HistoryPage int
	// Submits counts submissions; Refused counts refusals by reason.
	Submits int
	Refused map[string]int
}

type held struct {
	txid    string
	beef    []byte
	c       [32]byte
	op      string
	env     *boxrec.Envelope
	rcpt    *boxrec.Receipt
	created uint64
}

type sweepRow struct {
	txid  string
	beef  []byte
	spent []string
}

// NewHost is a host carrying offices whose engine checks proofs against
// chain.
func NewHost(chain *Chain, offices ...string) *Host {
	h := &Host{chain: chain, offices: map[string]bool{}, applied: map[string]bool{}, carriers: map[string]*held{},
		sweeps: map[string]*sweepRow{}, coins: map[transaction.Outpoint]string{}, Now: time.Now, Refused: map[string]int{}}
	for _, o := range offices {
		h.offices[o] = true
	}
	return h
}

// Carry adds an office.
func (h *Host) Carry(office string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.offices[office] = true
}

func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	down := h.Down
	h.mu.Unlock()
	if down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/submit":
		h.submit(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/lookup":
		h.lookup(w, r)
	default:
		http.NotFound(w, r)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (h *Host) submit(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body")
		return
	}
	topic := r.Header.Get("x-topics")
	office := strings.TrimPrefix(topic, boxrec.TopicPrefix)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Submits++
	if !h.offices[office] {
		writeError(w, http.StatusBadRequest, "topic not carried")
		return
	}
	_, tx, txid, err := guard.ParseBEEF(body, 8<<20)
	if err != nil || tx == nil {
		writeError(w, http.StatusBadRequest, "not a BEEF")
		return
	}
	steak := func(admit []int, retain []int) {
		if admit == nil {
			admit = []int{}
		}
		if retain == nil {
			retain = []int{}
		}
		writeJSON(w, map[string]any{topic: map[string]any{"outputsToAdmit": admit, "coinsToRetain": retain}})
	}
	if h.Drop != nil && h.Drop(tx) {
		steak(nil, nil)
		return
	}
	if h.applied[txid.String()] {
		steak(nil, nil)
		return
	}
	ok, verr := spv.Verify(context.Background(), tx, h.chain, nil)
	if verr != nil || !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("SPV: %v", verr))
		return
	}
	var prev []int
	for i, in := range tx.Inputs {
		if h.coins[transaction.Outpoint{Txid: *in.SourceTXID, Index: in.SourceTxOutIndex}] == topic {
			prev = append(prev, i)
		}
	}
	a, err := boxrec.Admit(context.Background(), body, prev, boxrec.Host{Office: office, Headers: h.chain})
	if err != nil {
		h.Refused[boxrec.Reason(err)]++
		steak(nil, nil)
		return
	}
	h.applied[txid.String()] = true
	for _, i := range a.Outputs {
		h.coins[transaction.Outpoint{Txid: *txid, Index: uint32(i)}] = topic //nolint:gosec // an output index
	}
	switch a.Kind {
	case boxrec.TxEnvelope, boxrec.TxReceipt:
		c := a.Carrier
		row := &held{txid: txid.String(), beef: body, c: c.Commitment, op: c.Funding.String(), env: c.Envelope, rcpt: c.Receipt}
		if c.Envelope != nil {
			row.created = c.Envelope.Created
		} else {
			row.created = c.Receipt.Created
		}
		h.carriers[row.txid] = row
	case boxrec.TxSweep:
		row := &sweepRow{txid: txid.String(), beef: body}
		for _, op := range a.Spent {
			row.spent = append(row.spent, op.String())
		}
		h.sweeps[row.txid] = row
	}
	steak(a.Outputs, a.Retain)
}

// retracted reports whether an admitted sweep spends op.
func (h *Host) retracted(op string) bool {
	for _, s := range h.sweeps {
		for _, x := range s.spent {
			if x == op {
				return true
			}
		}
	}
	return false
}

// winner is the answered carrier on op: the lowest commitment among the
// envelopes on it a receipt acknowledges, else among all.
func (h *Host) winner(op string) string {
	var all []*held
	for _, c := range h.carriers {
		if c.op == op {
			all = append(all, c)
		}
	}
	sort.Slice(all, func(i, j int) bool { return bytes.Compare(all[i].c[:], all[j].c[:]) < 0 })
	for _, c := range all {
		if c.env != nil && h.acked(c) {
			return c.txid
		}
	}
	if len(all) == 0 {
		return ""
	}
	return all[0].txid
}

// acked reports whether an answered receipt by the envelope's to lists it.
func (h *Host) acked(e *held) bool {
	for _, r := range h.carriers {
		if r.rcpt == nil || !bytes.Equal(r.rcpt.By, e.env.To) || h.retracted(r.op) {
			continue
		}
		for _, a := range r.rcpt.Acks {
			if a == e.c {
				// The receipt must be its outpoint's winner; a receipt's
				// outpoint has no acknowledged envelopes, so the lowest.
				if h.lowest(r.op) == r.txid {
					return true
				}
			}
		}
	}
	return false
}

func (h *Host) lowest(op string) string {
	var best *held
	for _, c := range h.carriers {
		if c.op == op && (best == nil || bytes.Compare(c.c[:], best.c[:]) < 0) {
			best = c
		}
	}
	if best == nil {
		return ""
	}
	return best.txid
}

// Status is a carrier's status (spec section 8.2).
func (h *Host) Status(txid string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.carriers[txid]
	if c == nil {
		return ""
	}
	return h.status(c)
}

func (h *Host) status(c *held) string {
	switch {
	case h.retracted(c.op):
		return "retracted"
	case h.winner(c.op) != c.txid:
		return "superseded"
	case c.env != nil && h.acked(c):
		return "acknowledged"
	}
	return "held"
}

func (h *Host) open(c *held, now uint64) bool {
	e := c.env
	return h.status(c) == "held" && (e.Expires == 0 || now < e.Expires) && e.Created+2592000 >= now && e.Created <= now+3600
}

func (h *Host) lookup(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Service string         `json:"service"`
		Query   map[string]any `json:"query"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&q); err != nil || q.Service != boxrec.LookupService {
		writeError(w, http.StatusBadRequest, "not an ls_bbox question")
		return
	}
	qq, err := boxrec.ParseQuery(q.Query)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.offices[qq.Office] {
		writeError(w, http.StatusBadRequest, "this host does not carry the office")
		return
	}
	if !qq.Class.Free && h.Priced {
		writeError(w, http.StatusBadRequest, qq.Class.Name+" is priced; ask it at the host's terms route")
		return
	}
	writeJSON(w, map[string]any{"type": "output-list", "outputs": h.Answer(qq)})
}

// Output is one answered output.
type Output struct {
	Beef        []byte `json:"beef"`
	OutputIndex int    `json:"outputIndex"`
}

// Answer answers a validated question. The caller holds the lock.
func (h *Host) Answer(q *boxrec.Question) []Output {
	now := uint64(h.Now().Unix()) //nolint:gosec // a clock after 1970
	var rows []*held
	switch q.Class.Name {
	case "receipt":
		for _, c := range h.carriers {
			if c.rcpt == nil || c.rcpt.Office != q.Office || !bytes.Equal(c.rcpt.By, q.Key) {
				continue
			}
			if st := h.status(c); st == "retracted" || st == "superseded" {
				continue
			}
			for _, a := range c.rcpt.Acks {
				if chainhash.Hash(a).String() == q.ReceiptFor {
					rows = append(rows, c)
				}
			}
		}
	case "sweep":
		var out []Output
		op := fmt.Sprintf("%s.%d", q.SpentTxid, q.SpentVout)
		for _, s := range h.sweeps {
			for _, x := range s.spent {
				if x == op {
					out = append(out, Output{Beef: s.beef})
				}
			}
		}
		return out
	default:
		history := !q.Class.Free
		for _, c := range h.carriers {
			e := c.env
			if e == nil || e.Office != q.Office || !bytes.Equal(e.To, q.Key) {
				continue
			}
			if q.Box != "" && e.Box != q.Box {
				continue
			}
			if q.From != nil && !bytes.Equal(e.From, q.From) {
				continue
			}
			if history {
				st := h.status(c)
				if st == "retracted" || st == "superseded" || h.open(c, now) || e.Created > now+3600 {
					continue
				}
			} else if !h.open(c, now) {
				continue
			}
			if q.AfterTxid != "" && (c.created < q.AfterCreated || c.created == q.AfterCreated && c.txid <= q.AfterTxid) {
				continue
			}
			rows = append(rows, c)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].created != rows[j].created {
			return rows[i].created < rows[j].created
		}
		return rows[i].txid < rows[j].txid
	})
	page := q.Class.Page
	if !q.Class.Free && h.HistoryPage > 0 {
		page = h.HistoryPage
	}
	if len(rows) > page {
		rows = rows[:page]
	}
	out := make([]Output, 0, len(rows))
	for _, c := range rows {
		b := c.beef
		if h.Tamper != nil {
			b = h.Tamper(append([]byte(nil), b...))
		}
		out = append(out, Output{Beef: h.stale(b)})
	}
	return out
}

// stale rewrites the proof of a carrier's funding tree to name another
// block when Stale is set.
func (h *Host) stale(beef []byte) []byte {
	if !h.Stale {
		return beef
	}
	_, tx, _, err := guard.ParseBEEF(beef, 8<<20)
	if err != nil || tx == nil || len(tx.Inputs) == 0 || tx.Inputs[0].SourceTransaction == nil || tx.Inputs[0].SourceTransaction.MerklePath == nil {
		return beef
	}
	mp := *tx.Inputs[0].SourceTransaction.MerklePath
	mp.BlockHeight += 3
	tx.Inputs[0].SourceTransaction.MerklePath = &mp
	out, err := tx.AtomicBEEF(false)
	if err != nil {
		return beef
	}
	return out
}

// Holds reports whether the host holds a carrier or sweep with txid.
func (h *Host) Holds(txid string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.carriers[txid] != nil || h.sweeps[txid] != nil
}

// SetDown takes the host down or brings it back.
func (h *Host) SetDown(down bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Down = down
}

// Plane stands in for the multicast plane: a facade that delivers each
// submission to every host subscribed to the topic, answering with the
// first host's answer. Lookups go to the first host, the facade's own.
type Plane struct {
	Hosts []http.Handler
	// Lost, when set, is asked per host and submission; true loses the
	// object on the way to that host.
	Lost func(host int, tx []byte) bool
}

func (p *Plane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/submit" {
		p.Hosts[0].ServeHTTP(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "body", http.StatusBadRequest)
		return
	}
	var first *recorder
	for i, h := range p.Hosts {
		if p.Lost != nil && p.Lost(i, body) {
			continue
		}
		rec := &recorder{header: http.Header{}, code: http.StatusOK}
		req := r.Clone(r.Context())
		req.Body = io.NopCloser(bytes.NewReader(body))
		h.ServeHTTP(rec, req)
		if first == nil {
			first = rec
		}
	}
	if first == nil {
		http.Error(w, "lost", http.StatusServiceUnavailable)
		return
	}
	for k, v := range first.header {
		w.Header()[k] = v
	}
	w.WriteHeader(first.code)
	_, _ = w.Write(first.body.Bytes())
}

type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(c int)           { r.code = c }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
