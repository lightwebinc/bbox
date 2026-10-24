package module

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/headers"
	bmodule "github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/termsroute"

	"github.com/lightwebinc/bbox/boxrec"
)

// Name is the module's name, as a host's module list names it.
const Name = "bbox"

// PruneEvery is how often retention runs; TickEvery how often the
// background tick runs (the payment watch; retention every PruneEvery).
const (
	PruneEvery = time.Hour
	TickEvery  = 30 * time.Second
)

// Build is the version the host logs; a binary sets it.
var Build = "unversioned"

// Mounted is the module for one configuration: the contract's Module, the
// lookup service, and what Start runs.
type Mounted struct {
	Module bmodule.Module
	LS     *Service
	// Front is the terms route, when configured.
	Front   *Front
	config  *Config
	host    bmodule.Host
	ledger  *termsroute.Ledger
	journal Journal
}

// Front is the terms route and where it listens.
type Front struct {
	*termsroute.Front
	Addr string
}

// Mount builds the module for c. overlayTopics is OVERLAY_TOPICS, when
// known; journal replaces the state directory's, hdrs the configured
// header source and now the host's clock, for tests.
func Mount(host bmodule.Host, c *Config, overlayTopics []string, journal Journal, hdrs chaintracker.ChainTracker, now func() time.Time) (*Mounted, error) {
	names := map[string]bool{}
	for _, office := range c.Offices {
		names[topicOf(office)] = true
	}
	for _, t := range overlayTopics {
		if strings.HasPrefix(t, boxrec.TopicPrefix) && !names[t] {
			return nil, fmt.Errorf("OVERLAY_TOPICS names %s but BBOX_OFFICES does not configure it; it would be admitted by the default manager", t)
		}
	}
	m := host.Metrics()
	for _, kind := range AdmitKinds {
		m.Preset("bbox_admitted_total", map[string]string{"kind": kind})
		m.Preset("bbox_admitted_repeats_total", map[string]string{"kind": kind})
	}
	for _, why := range []string{"cap", "idle"} {
		m.Preset("bbox_sessions_evicted_total", map[string]string{"why": why})
	}
	for _, reason := range Reasons {
		m.Preset("bbox_refused_total", map[string]string{"reason": reason})
	}
	for _, why := range []string{"evidence", "retention"} {
		m.Preset("bbox_dropped_total", map[string]string{"why": why})
	}
	m.Preset("bbox_retractions_total", nil)
	m.Preset("bbox_journal_lines_total", nil)
	m.Preset("bbox_journal_wait_seconds_total", nil)
	for _, result := range []string{"ok", "error"} {
		m.Preset("bbox_journal_syncs_total", map[string]string{"result": result})
	}
	m.Preset("bbox_journal_sync_seconds_total", nil)
	priced := map[string]bool{}
	for name, p := range c.Prices {
		if p > 0 {
			priced[name] = true
		}
	}
	if now == nil {
		now = time.Now
	}
	if journal == nil {
		fj, err := NewFileJournal(c.StateDir)
		if err != nil {
			return nil, err
		}
		fj.SyncEvery = c.JournalSync
		fj.OnSync = journalSyncs(host)
		journal = fj
	}
	ls, err := NewService(Options{Offices: c.Offices, Host: host, Journal: journal, Priced: priced, RetentionDays: c.RetentionDays, Now: now})
	if err != nil {
		return nil, err
	}
	m.Gauge("bbox_offices", func() float64 { return float64(len(c.Offices)) })
	m.Gauge("bbox_envelopes", func() float64 { return float64(ls.Counts().Envelopes) })
	m.Gauge("bbox_receipts", func() float64 { return float64(ls.Counts().Receipts) })
	m.Gauge("bbox_sweeps", func() float64 { return float64(ls.Counts().Sweeps) })
	m.Gauge("bbox_outpoint_rows", func() float64 { return float64(ls.Counts().Rows) })
	m.Gauge("bbox_carrier_lines", func() float64 { return float64(ls.Counts().Lines) })
	topics := map[string]bmodule.TopicManager{}
	for _, office := range c.Offices {
		tm, err := NewTopicManager(topicOf(office), host, nil, c.MaxBEEF)
		if err != nil {
			return nil, err
		}
		topics[topicOf(office)] = tm
	}
	out := &Mounted{Module: bmodule.Module{Topics: topics, Lookups: map[string]bmodule.LookupService{boxrec.LookupService: ls}}, LS: ls, config: c, host: host, journal: journal}
	// The header source payments verify against, when there is one.
	var payHeaders chaintracker.ChainTracker
	switch {
	case hdrs != nil:
		payHeaders = hdrs
	case c.HeadersURL != "":
		payHeaders = headers.New(c.HeadersURL)
	}
	if c.Listen != "" {
		var receiver termsroute.Receiver
		var gate *termsroute.Gate
		if len(priced) > 0 {
			ledger, err := termsroute.OpenLedger(c.StateDir, termsroute.LayoutSpread, now)
			if err != nil {
				return nil, err
			}
			out.ledger = ledger
			receiver = ledger
			if payHeaders != nil {
				gate = termsroute.GateFor("bbox", host, payHeaders, c.Accept, c.StateDir)
			}
		}
		f, err := termsroute.New(termsroute.Options{App: "bbox", Service: boxrec.LookupService, Classes: Classes, PaymentProtocol: boxrec.PaymentProtocol,
			LS: RouteAnswerer{LS: ls}, Host: host, Prices: c.Prices, Payee: c.PayeeKey, Receiver: receiver, Headers: payHeaders, Gate: gate,
			Sessions: c.Sessions, Budget: c.Budget, Responses: c.Response, Now: now})
		if err != nil {
			return nil, err
		}
		if out.ledger != nil {
			f.RestoreFast(out.ledger.Fast)
		}
		out.Front = &Front{Front: f, Addr: c.Listen}
	}
	terms, sessions, handshakes, responses, payments := "none", "none", "none", "none", "none"
	if c.Listen != "" {
		terms = c.Listen
		sessions = fmt.Sprintf("%d, idle %ds", c.Sessions.Max, int(c.Sessions.TTL/time.Second))
		handshakes = fmt.Sprintf("%s/s burst %d, per address %s/s burst %d", num(c.Budget.PerSec), c.Budget.Burst, num(c.Budget.PerAddressPerSec), c.Budget.AddressBurst)
		responses = fmt.Sprintf("%s/s burst %d a session", num(c.Response.PerSec), c.Response.Burst)
	}
	if out.Front != nil && out.Front.Gate != nil {
		if out.Front.Gate.Networked() {
			payments = fmt.Sprintf("fast to %d sat, payer %d, total %d", c.Accept.Policy.ThresholdSats, c.Accept.Policy.PayerLimit, c.Accept.Policy.TotalLimit)
		} else {
			payments = "held until mined (no arcade, or no asset URL or chain)"
		}
	}
	pricedNames := make([]string, 0, len(priced))
	for n := range priced {
		pricedNames = append(pricedNames, n)
	}
	topicNames := make([]string, 0, len(topics))
	for _, office := range c.Offices {
		topicNames = append(topicNames, topicOf(office))
	}
	journalLine := "each line"
	if c.JournalSync > 0 {
		journalLine = fmt.Sprintf("every %s", c.JournalSync)
	}
	host.Log("bbox module mounted", map[string]any{"topics": strings.Join(topicNames, ","), "lookup": boxrec.LookupService,
		"terms": terms, "priced": orNone(sortedStrings(pricedNames)), "sessions": sessions, "handshakes": handshakes, "responses": responses,
		"payments": payments, "journal_sync": journalLine, "build": Build})
	return out, nil
}

// num writes a rate as JavaScript writes a number: 4, 0.5, 2.5.
func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func orNone(a []string) string {
	if len(a) == 0 {
		return "none"
	}
	return strings.Join(a, ",")
}

func sortedStrings(a []string) []string {
	out := append([]string(nil), a...)
	sort.Strings(out)
	return out
}

// journalSyncs counts the rows journal's timed fsyncs and logs the first
// failure of a run of them and the recovery after it.
func journalSyncs(host bmodule.Host) func(lines int, took time.Duration, err error) {
	var mu sync.Mutex
	failing := false
	return func(lines int, took time.Duration, err error) {
		m := host.Metrics()
		m.Inc("bbox_journal_sync_seconds_total", nil, took.Seconds())
		result := "ok"
		if err != nil {
			result = "error"
		}
		m.Inc("bbox_journal_syncs_total", map[string]string{"result": result}, 1)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err != nil && !failing:
			failing = true
			host.Log("ls_bbox could not sync the journal; its lines stay pending and the sync is tried again", map[string]any{"lines": lines, "err": err.Error()})
		case err == nil && failing:
			failing = false
			host.Log("ls_bbox journal synced again", map[string]any{"lines": lines})
		}
	}
}

// Close flushes the rows journal and closes it (Start does when its context
// ends). A host that stops without it leaves at most the last sync window's
// lines in the page cache: written, not yet on disk.
func (m *Mounted) Close() error {
	if c, ok := m.journal.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// Start serves the terms route (when configured) until ctx ends, and runs
// the background tick: the payment watch every TickEvery and retention
// every PruneEvery once the index is restored. It returns when ctx ends,
// after closing the rows journal (Close).
func (m *Mounted) Start(ctx context.Context) error {
	defer func() { _ = m.Close() }()
	go func() {
		pruned := time.Now()
		t := time.NewTicker(TickEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if m.Front != nil {
				m.Front.Tick(ctx)
			}
			if !m.LS.Restored() || time.Since(pruned) < PruneEvery {
				continue
			}
			pruned = time.Now()
			m.LS.Prune(ctx)
		}
	}()
	if m.Front == nil {
		<-ctx.Done()
		return nil
	}
	return m.Front.Front.Listen(ctx, m.Front.Addr)
}

// New is the factory a host calls: the configuration from getenv, checked
// against OVERLAY_TOPICS.
func New(host bmodule.Host, getenv func(string) string) (*Mounted, error) {
	if getenv == nil {
		return nil, errors.New("bbox: no environment")
	}
	var topics []string
	for _, t := range strings.Split(getenv("OVERLAY_TOPICS"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			topics = append(topics, t)
		}
	}
	c, err := ParseConfig(getenv)
	if err != nil {
		return nil, err
	}
	return Mount(host, c, topics, nil, nil, nil)
}

// RouteAnswerer is ls_bbox as the terms route asks its questions: an
// answer is hydrated as the engine hydrates one.
type RouteAnswerer struct{ LS *Service }

var _ termsroute.Answerer = RouteAnswerer{}

// Restored implements termsroute.Answerer.
func (r RouteAnswerer) Restored() bool { return r.LS.Restored() }

// Classify implements termsroute.Answerer.
func (r RouteAnswerer) Classify(_ context.Context, q termsroute.Question) (string, error) {
	c, err := r.LS.Classify(q.Service, q.Query)
	if err != nil {
		return "", err
	}
	return c.Q.Class.Name, nil
}

// Answer implements termsroute.Answerer: a priced question reaches it only
// once paid.
func (r RouteAnswerer) Answer(ctx context.Context, q termsroute.Question) (any, error) {
	c, err := r.LS.Classify(q.Service, q.Query)
	if err != nil {
		return nil, err
	}
	txids, err := r.LS.Answer(c, true)
	if err != nil {
		return nil, err
	}
	outs, err := r.LS.Hydrate(ctx, txids)
	if err != nil {
		return nil, err
	}
	list := OutputList{Type: "output-list", Outputs: make([]OutputJSON, 0, len(outs))}
	for _, o := range outs {
		list.Outputs = append(list.Outputs, OutputJSON{Beef: byteArray(o.Beef), OutputIndex: o.OutputIndex})
	}
	return list, nil
}

// byteArray writes bytes as a JSON array of numbers, as the TypeScript
// route writes a number[].
type byteArray []byte

// MarshalJSON implements json.Marshaler.
func (b byteArray) MarshalJSON() ([]byte, error) {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, c := range b {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.Itoa(int(c)))
	}
	sb.WriteByte(']')
	return []byte(sb.String()), nil
}

// OutputJSON is one output of an answer as JSON.
type OutputJSON struct {
	Beef        byteArray `json:"beef"`
	OutputIndex uint32    `json:"outputIndex"`
}

// OutputList is the engine's lookup answer as JSON.
type OutputList struct {
	Type    string       `json:"type"`
	Outputs []OutputJSON `json:"outputs"`
}
