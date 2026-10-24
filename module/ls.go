package module

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"
	bmodule "github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/pushdrop"

	"github.com/lightwebinc/bbox/boxrec"
)

// The answer window (spec section 8.3): 30 days before the host's time,
// one hour after.
const (
	WindowBefore = 30 * 86400
	WindowAfter  = 3600
)

// FloorDays is the retention floor for receipts and sweeps, from first
// sight (spec section 8.4).
const FloorDays = 31

// Evidence is how many superseded carriers are kept per outpoint, and
// EvidenceAcknowledged how many when its winner is acknowledged.
const (
	Evidence             = 8
	EvidenceAcknowledged = 64
)

// The statuses of spec section 8.2.
const (
	StatusRetracted    = "retracted"
	StatusSuperseded   = "superseded"
	StatusAcknowledged = "acknowledged"
	StatusHeld         = "held"
)

// Deleter is a storage that can delete an output: what retention and the
// evidence cap need beyond the contract's RestoreStorage. A host whose
// storage cannot delete keeps what the index dropped, unanswered.
type Deleter interface {
	DeleteOutput(ctx context.Context, outpoint *transaction.Outpoint, topic string) error
}

// Options build a Service.
type Options struct {
	// Offices are the office identifiers the host carries.
	Offices []string
	Host    bmodule.Host
	Journal Journal
	// Priced are the priceable classes this host prices: asked through the
	// engine, they are refused.
	Priced map[string]bool
	// RetentionDays is how long a host keeps what it no longer answers,
	// from first sight; at least FloorDays, and FloorDays when zero.
	RetentionDays int
	// Now is the host's clock; time.Now when nil.
	Now func() time.Time
}

// envEntry is an envelope carrier the host holds.
type envEntry struct {
	txid, c, office, to, box, from string
	created, expires               uint64
}

// rcptEntry is a receipt carrier the host holds.
type rcptEntry struct {
	txid, c, office, by string
	acks                map[string]bool
	created             uint64
}

// sweepEntry is a sweep tombstone the host holds.
type sweepEntry struct {
	txid string
	// offices are every office the sweep was admitted in: one mined sweep
	// may be published to several topics.
	offices map[string]bool
	spent   []string
	height  uint32
}

// row is one funding outpoint's row.
type row struct {
	carriers map[string]*Line
	dropped  map[string]bool
	// closed: its winner was dropped, so nothing on it is answered again.
	closed bool
	// sweeps are admitted sweeps that spend it.
	sweeps map[string]bool
}

type pendingDelete struct {
	txid, topic string
}

// orderedSet is a set that iterates in the order its members were first
// added, as a JavaScript Set does.
type orderedSet struct {
	has   map[string]bool
	order []string
}

func (o *orderedSet) add(v string) {
	if o.has == nil {
		o.has = map[string]bool{}
	}
	if !o.has[v] {
		o.has[v] = true
		o.order = append(o.order, v)
	}
}

// Service is ls_bbox: one lookup service for every office a host carries
// (spec sections 7.2, 7.3 and 8.2 to 8.5), the twin of the TypeScript
// module's ls_bbox.ts.
//
// Two stores back it. The engine's storage holds the outputs the host
// keeps: envelope and receipt carriers and sweep tombstones, which this
// service indexes in memory by office and key and rebuilds from storage on
// start. The journal holds the outpoint rows the host keeps for its life:
// every carrier any office admitted, with the outpoint it spent; every
// drop; every outpoint an admitted sweep spent. Every status (spec section
// 8.2) is a function of the rows, never of arrival order:
//
//   - retracted: a sweep spends the carrier's funding outpoint;
//   - superseded: the carrier is not its outpoint's winner, or the winner
//     was dropped;
//   - acknowledged: an envelope a receipt acknowledges, the receipt being
//     by the envelope's to, not retracted, and its own outpoint's winner;
//   - held: otherwise.
//
// The winner of an outpoint is the lowest commitment among its envelopes a
// receipt acknowledges, or else among all its carriers. A dropped receipt
// keeps acknowledging (its row outlives it), so a drop can never hand an
// outpoint a new winner.
type Service struct {
	offices   map[string]bool
	host      bmodule.Host
	journal   Journal
	priced    map[string]bool
	retention int64
	now       func() time.Time

	mu sync.Mutex
	// Outpoint rows, from the journal.
	rows map[string]*row
	// lines: commitment -> its carrier line; lineOrder the commitments in
	// the order first seen.
	lines     map[string]*Line
	lineOrder []string
	// ackers: envelope commitment -> receipt commitments listing it.
	ackers map[string]*orderedSet
	// sweepSeen: sweep txid -> first sight.
	sweepSeen map[string]int64
	// sweepOffices: sweep txid -> the offices the journal's s lines name it
	// in; sweepOrder the txids in the order first seen.
	sweepOffices map[string]*orderedSet
	sweepOrder   []string

	// What the host holds, from storage.
	envelopes  map[string]*envEntry
	envByTo    map[string]map[string]bool
	receipts   map[string]*rcptEntry
	rcptByBy   map[string]map[string]bool
	sweeps     map[string]*sweepEntry
	sweepsByOp map[string]map[string]bool

	// pending are outputs dropped from the index and still to be deleted
	// from storage.
	pending        map[string]pendingDelete
	storage        bmodule.RestoreStorage
	warnedNoDelete bool

	// Memo of the status functions, cleared on any change.
	winners   map[string]string
	acked     map[string]bool
	computing map[string]bool
}

var (
	_ bmodule.LookupService     = (*Service)(nil)
	_ bmodule.Restorer          = (*Service)(nil)
	_ bmodule.RetentionListener = (*Service)(nil)
)

// NewService makes the lookup service and loads the journal.
func NewService(o Options) (*Service, error) {
	days := o.RetentionDays
	if days == 0 {
		days = FloorDays
	}
	if days < FloorDays {
		return nil, fmt.Errorf("ls_bbox: retention of %d days is under the %d-day floor", days, FloorDays)
	}
	if o.Journal == nil {
		return nil, errors.New("ls_bbox: no journal")
	}
	s := &Service{offices: map[string]bool{}, host: o.Host, journal: o.Journal, priced: o.Priced, retention: int64(days) * 86400, now: o.Now}
	for _, off := range o.Offices {
		s.offices[off] = true
	}
	if s.priced == nil {
		s.priced = map[string]bool{}
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.clear()
	lines, err := s.journal.Load()
	if err != nil {
		return nil, err
	}
	for i := range lines {
		s.apply(&lines[i])
	}
	return s, nil
}

func (s *Service) clear() {
	s.rows = map[string]*row{}
	s.lines = map[string]*Line{}
	s.lineOrder = nil
	s.ackers = map[string]*orderedSet{}
	s.sweepSeen = map[string]int64{}
	s.sweepOffices = map[string]*orderedSet{}
	s.sweepOrder = nil
	s.envelopes = map[string]*envEntry{}
	s.envByTo = map[string]map[string]bool{}
	s.receipts = map[string]*rcptEntry{}
	s.rcptByBy = map[string]map[string]bool{}
	s.sweeps = map[string]*sweepEntry{}
	s.sweepsByOp = map[string]map[string]bool{}
	s.pending = map[string]pendingDelete{}
	s.changed()
}

func (s *Service) unix() int64 { return s.now().Unix() }

func addTo(m map[string]map[string]bool, k, v string) {
	set := m[k]
	if set == nil {
		set = map[string]bool{}
		m[k] = set
	}
	set[v] = true
}

func removeFrom(m map[string]map[string]bool, k, v string) {
	set := m[k]
	if set == nil {
		return
	}
	delete(set, v)
	if len(set) == 0 {
		delete(m, k)
	}
}

func outpoint(txid string, vout uint32) string { return fmt.Sprintf("%s.%d", txid, vout) }

// hashOrder is a display txid in hash byte order.
func hashOrder(display string) string {
	h, err := chainhash.NewHashFromHex(display)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(h[:])
}

func topicOf(office string) string { return boxrec.TopicPrefix + office }

// ---- counts

// Counts are what the index holds, the gauges' values.
type Counts struct {
	Envelopes, Receipts, Sweeps, Rows, Lines, PendingDeletes int
}

// Counts reads the index's sizes.
func (s *Service) Counts() Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Counts{Envelopes: len(s.envelopes), Receipts: len(s.receipts), Sweeps: len(s.sweeps), Rows: len(s.rows), Lines: len(s.lines), PendingDeletes: len(s.pending)}
}

// ---- outpoint rows

func (s *Service) row(op string) *row {
	r := s.rows[op]
	if r == nil {
		r = &row{carriers: map[string]*Line{}, dropped: map[string]bool{}, sweeps: map[string]bool{}}
		s.rows[op] = r
	}
	return r
}

func (s *Service) apply(l *Line) {
	r := s.row(l.Op)
	switch l.K {
	case "c":
		r.carriers[l.C] = l
		if _, seen := s.lines[l.C]; !seen {
			s.lineOrder = append(s.lineOrder, l.C)
		}
		s.lines[l.C] = l
		for _, a := range l.Acks {
			set := s.ackers[a]
			if set == nil {
				set = &orderedSet{}
				s.ackers[a] = set
			}
			set.add(l.C)
		}
	case "d":
		r.dropped[l.C] = true
		if l.W != nil && *l.W {
			r.closed = true
		}
	case "s":
		r.sweeps[l.S] = true
		if _, ok := s.sweepSeen[l.S]; !ok {
			s.sweepSeen[l.S] = l.at()
		}
		set := s.sweepOffices[l.S]
		if set == nil {
			set = &orderedSet{}
			s.sweepOffices[l.S] = set
			s.sweepOrder = append(s.sweepOrder, l.S)
		}
		set.add(l.Office)
	}
	s.changed()
}

func (s *Service) queueDelete(txid, office string) {
	t := topicOf(office)
	s.pending[t+"|"+txid] = pendingDelete{txid: txid, topic: t}
}

// write appends a line to the journal and applies it. A line that is not
// on disk is still applied: the host keeps answering as it decided, and
// says so.
func (s *Service) write(l Line) {
	start := time.Now()
	err := s.journal.Append(l)
	m := s.host.Metrics()
	m.Inc("bbox_journal_wait_seconds_total", nil, time.Since(start).Seconds())
	m.Inc("bbox_journal_lines_total", nil, 1)
	if err != nil {
		s.host.Log("ls_bbox could not write the journal", map[string]any{"err": err.Error()})
	}
	s.apply(&l)
}

func (s *Service) changed() {
	s.winners = map[string]string{}
	s.acked = map[string]bool{}
	if s.computing == nil {
		s.computing = map[string]bool{}
	}
}

// ---- statuses (spec section 8.2)

func sortedKeys(m map[string]*Line) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// winnerOf is the outpoint's winner: the lowest C among its acknowledged
// envelopes, else among all its carriers; "" when it has none.
func (s *Service) winnerOf(op string) string {
	if w, ok := s.winners[op]; ok {
		return w
	}
	r := s.rows[op]
	if r == nil || len(r.carriers) == 0 {
		return ""
	}
	all := sortedKeys(r.carriers)
	if s.computing[op] {
		return all[0]
	}
	s.computing[op] = true
	w := ""
	for _, c := range all {
		if s.isAcknowledged(c) {
			w = c
			break
		}
	}
	if w == "" {
		w = all[0]
	}
	delete(s.computing, op)
	s.winners[op] = w
	return w
}

// acknowledges is whether a receipt is not retracted and is its outpoint's
// winner, so it acknowledges.
func (s *Service) acknowledges(l *Line) bool {
	return len(s.rows[l.Op].sweeps) == 0 && s.winnerOf(l.Op) == l.C
}

// isAcknowledged is whether an acknowledging receipt by the envelope's to
// lists envelope c.
func (s *Service) isAcknowledged(c string) bool {
	if v, ok := s.acked[c]; ok {
		return v
	}
	e := s.lines[c]
	yes := false
	if e != nil && e.Kind == "e" {
		if set := s.ackers[c]; set != nil {
			for _, rc := range set.order {
				r := s.lines[rc]
				if r != nil && r.By == e.To && s.acknowledges(r) {
					yes = true
					break
				}
			}
		}
	}
	s.acked[c] = yes
	return yes
}

// status is the status of the carrier with commitment c (hash byte order
// hex), "" when the rows do not know it.
func (s *Service) status(c string) string {
	l := s.lines[c]
	if l == nil {
		return ""
	}
	r := s.rows[l.Op]
	if len(r.sweeps) > 0 {
		return StatusRetracted
	}
	if r.closed || s.winnerOf(l.Op) != c {
		return StatusSuperseded
	}
	if l.Kind == "e" && s.isAcknowledged(c) {
		return StatusAcknowledged
	}
	return StatusHeld
}

// Status is the status of the carrier with commitment c (hash byte order
// hex), "" when the rows do not know it.
func (s *Service) Status(c string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status(c)
}

// Dropped is whether the carrier with commitment c was dropped.
func (s *Service) Dropped(c string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.lines[c]
	return l != nil && s.rows[l.Op].dropped[c]
}

func (s *Service) open(e *envEntry, now int64) bool {
	created := int64(e.created) //nolint:gosec // a created is at most MaxTime
	return s.status(e.c) == StatusHeld &&
		(e.expires == 0 || now < int64(e.expires)) && //nolint:gosec // an expires is at most MaxTime
		created >= now-WindowBefore &&
		created <= now+WindowAfter
}

// ---- admission

func parse(raw []byte) (*transaction.Transaction, error) {
	_, tx, _, err := guard.ParseBEEF(raw, guard.DefaultBound)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, errors.New("no subject transaction")
	}
	return tx, nil
}

// parseFor is the transaction txid in a stored BEEF, with its proof, as
// the TypeScript module's Transaction.fromBEEF(beef, txid) reads it.
func parseFor(raw []byte, txid chainhash.Hash) (*transaction.Transaction, error) {
	beef, tx, _, err := guard.ParseBEEF(raw, guard.DefaultBound)
	if err != nil {
		return nil, err
	}
	if beef != nil {
		if t := beef.FindAtomicTransactionByHash(&txid); t != nil {
			return t, nil
		}
	}
	if tx != nil && tx.TxID().Equal(txid) {
		return tx, nil
	}
	return nil, fmt.Errorf("no transaction %s in the BEEF", txid)
}

func srcTxid(in *transaction.TransactionInput) string {
	if in.SourceTXID != nil {
		return in.SourceTXID.String()
	}
	if in.SourceTransaction != nil {
		return in.SourceTransaction.TxID().String()
	}
	return ""
}

// OutputAdmittedByTopic implements module.LookupService: it indexes an
// admitted output, then deletes what was dropped from storage. An
// admission that meets a restore in flight waits for it (the restore holds
// the service's lock), so that it is decided as any other.
func (s *Service) OutputAdmittedByTopic(ctx context.Context, p *bmodule.OutputAdmittedByTopic) error {
	if p.Mode != bmodule.AdmitWholeTx {
		return nil
	}
	office, ok := s.officeOf(p.Topic)
	if !ok {
		return nil
	}
	tx, err := parse(p.AtomicBEEF)
	if err != nil {
		s.host.Log("ls_bbox ignored an admission whose BEEF did not parse", map[string]any{"topic": p.Topic})
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.index(office, tx, p.Outpoint.Index); err != nil {
		return err
	}
	s.flush(ctx)
	return nil
}

// OutputEvicted implements module.LookupService.
func (s *Service) OutputEvicted(_ context.Context, op *transaction.Outpoint) error {
	if op == nil || op.Index != 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forget(op.Txid.String(), "")
	return nil
}

// OutputNoLongerRetainedInHistory implements module.RetentionListener: the
// output leaves that office's answers; its rows stay.
func (s *Service) OutputNoLongerRetainedInHistory(_ context.Context, op *transaction.Outpoint, topic string) error {
	office, ok := s.officeOf(topic)
	if op == nil || op.Index != 0 || !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forget(op.Txid.String(), office)
	return nil
}

// AdmissionMode implements module.LookupService: the whole transaction, as a
// carrier's funding outpoint and a sweep's spends are its inputs.
func (*Service) AdmissionMode() bmodule.AdmissionMode { return bmodule.AdmitWholeTx }

// SpendNotificationMode implements module.LookupService: spends are read
// from admitted transactions, never from the engine's notices.
func (*Service) SpendNotificationMode() bmodule.SpendNotificationMode { return bmodule.SpendNone }

func (s *Service) officeOf(t string) (string, bool) {
	if !strings.HasPrefix(t, boxrec.TopicPrefix) {
		return "", false
	}
	office := t[len(boxrec.TopicPrefix):]
	return office, s.offices[office]
}

// index indexes output vout of an admitted transaction on office, writing
// its rows the first time it is seen. A carrier the rows say was dropped is
// not indexed again, and its output is deleted. It reports whether the
// output is indexed.
func (s *Service) index(office string, tx *transaction.Transaction, vout uint32) (bool, error) {
	if vout != 0 || len(tx.Outputs) == 0 || tx.Outputs[0] == nil || tx.Outputs[0].LockingScript == nil {
		return false, nil
	}
	script := []byte(*tx.Outputs[0].LockingScript)
	txidH := tx.TxID()
	txid := txidH.String()
	kind := boxrec.ClaimOf(script)
	now := s.unix()
	if kind != boxrec.KindNone {
		if len(tx.Inputs) == 0 {
			return false, nil
		}
		in := tx.Inputs[0]
		src := srcTxid(in)
		if src == "" {
			return false, nil
		}
		op := outpoint(src, in.SourceTxOutIndex)
		c := hex.EncodeToString(txidH[:])
		record, _ := pushdrop.FirstPush(script)
		var line Line
		var env *envEntry
		var rc *rcptEntry
		if kind == boxrec.KindEnvelope {
			e, err := boxrec.DecodeEnvelope(record)
			if err != nil {
				return false, fmt.Errorf("ls_bbox: %s: %w", txid, err)
			}
			to := hex.EncodeToString(e.To)
			line = Line{K: "c", Op: op, C: c, Txid: txid, Kind: "e", Office: office, To: to, At: i64(now)}
			env = &envEntry{txid: txid, c: c, office: office, to: to, box: e.Box, from: hex.EncodeToString(e.From), created: e.Created, expires: e.Expires}
		} else {
			r, err := boxrec.DecodeReceipt(record)
			if err != nil {
				return false, fmt.Errorf("ls_bbox: %s: %w", txid, err)
			}
			by := hex.EncodeToString(r.By)
			acks := make([]string, 0, len(r.Acks))
			set := map[string]bool{}
			for _, a := range r.Acks {
				h := hex.EncodeToString(a[:])
				acks = append(acks, h)
				set[h] = true
			}
			line = Line{K: "c", Op: op, C: c, Txid: txid, Kind: "r", Office: office, By: by, Acks: acks, At: i64(now)}
			rc = &rcptEntry{txid: txid, c: c, office: office, by: by, acks: set, created: r.Created}
		}
		if _, seen := s.lines[c]; !seen {
			s.write(line)
		}
		if s.rows[op].dropped[c] {
			s.queueDelete(txid, office)
			return false, nil
		}
		if env != nil {
			s.envelopes[txid] = env
			addTo(s.envByTo, office+"|"+env.to, txid)
		} else {
			s.receipts[txid] = rc
			addTo(s.rcptByBy, office+"|"+rc.by, txid)
		}
		s.changed()
		s.enforceEvidence(op)
		return true, nil
	}
	if !boxrec.IsFundingShape(script) {
		s.host.Log("ls_bbox indexed an output of no known kind", map[string]any{"office": office, "txid": txid})
		return false, nil
	}
	// A sweep, or a published funding tree: every input's outpoint is swept.
	var height uint32
	if tx.MerklePath != nil {
		height = tx.MerklePath.BlockHeight
	}
	firstSight := func() int64 {
		if at, ok := s.sweepSeen[txid]; ok {
			return at
		}
		return now
	}
	var spent []string
	for _, in := range tx.Inputs {
		src := srcTxid(in)
		if src == "" {
			continue
		}
		op := outpoint(src, in.SourceTxOutIndex)
		spent = append(spent, op)
		if r := s.rows[op]; r == nil || !r.sweeps[txid] {
			s.write(Line{K: "s", Op: op, S: txid, Office: office, H: i64(int64(height)), At: i64(firstSight())})
			s.host.Metrics().Inc("bbox_retractions_total", nil, 1)
		}
	}
	// The same mined sweep admitted in another office answers there too,
	// and the journal names it there (one more line of the kind it always
	// held), so a restore reads it back in that office once its tombstone
	// is spent.
	if len(spent) > 0 {
		if set := s.sweepOffices[txid]; set == nil || !set.has[office] {
			s.write(Line{K: "s", Op: spent[0], S: txid, Office: office, H: i64(int64(height)), At: i64(firstSight())})
		}
	}
	e := s.sweeps[txid]
	if e == nil {
		e = &sweepEntry{txid: txid, offices: map[string]bool{}, spent: spent, height: height}
		s.sweeps[txid] = e
	}
	if !e.offices[office] {
		e.offices[office] = true
		for _, op := range e.spent {
			addTo(s.sweepsByOp, office+"|"+op, txid)
		}
	}
	return true, nil
}

// forget removes a held output from the index, in one office or in all
// (office ""); its rows stay.
func (s *Service) forget(txid, office string) {
	if e := s.envelopes[txid]; e != nil && (office == "" || e.office == office) {
		delete(s.envelopes, txid)
		removeFrom(s.envByTo, e.office+"|"+e.to, txid)
	}
	if r := s.receipts[txid]; r != nil && (office == "" || r.office == office) {
		delete(s.receipts, txid)
		removeFrom(s.rcptByBy, r.office+"|"+r.by, txid)
	}
	if sw := s.sweeps[txid]; sw != nil {
		var which []string
		if office == "" {
			for o := range sw.offices {
				which = append(which, o)
			}
		} else {
			which = []string{office}
		}
		for _, o := range which {
			if !sw.offices[o] {
				continue
			}
			delete(sw.offices, o)
			for _, op := range sw.spent {
				removeFrom(s.sweepsByOp, o+"|"+op, txid)
			}
		}
		if len(sw.offices) == 0 {
			delete(s.sweeps, txid)
		}
	}
}

// ---- drops (spec section 8.4)

// drop drops the carrier with commitment c: marks its row (and closes the
// row when c is its winner), takes it out of every answer, and queues its
// output for deletion from storage.
func (s *Service) drop(c, why string) {
	l := s.lines[c]
	r := s.rows[l.Op]
	if r.dropped[c] {
		return
	}
	s.write(Line{K: "d", Op: l.Op, C: c, W: boolp(s.winnerOf(l.Op) == c)})
	s.forget(l.Txid, "")
	s.queueDelete(l.Txid, l.Office)
	s.host.Metrics().Inc("bbox_dropped_total", map[string]string{"why": why}, 1)
	s.host.Log("ls_bbox dropped a carrier", map[string]any{"txid": l.Txid, "outpoint": l.Op, "why": why})
}

// enforceEvidence keeps at most Evidence superseded carriers on an
// outpoint (EvidenceAcknowledged when its winner is acknowledged) and drops
// the rest, highest commitment first.
func (s *Service) enforceEvidence(op string) {
	r := s.rows[op]
	w := s.winnerOf(op)
	limit := Evidence
	if w != "" && s.isAcknowledged(w) {
		limit = EvidenceAcknowledged
	}
	var kept []string
	for _, c := range sortedKeys(r.carriers) {
		if !r.dropped[c] && s.status(c) == StatusSuperseded {
			kept = append(kept, c)
		}
	}
	if len(kept) <= limit {
		return
	}
	over := kept[limit:]
	for i := len(over) - 1; i >= 0; i-- {
		s.drop(over[i], "evidence")
	}
}

// Prune applies retention at the host's time: it drops every carrier and
// sweep past its keep, then deletes what was dropped from storage. What is
// kept: an open envelope, and one dated ahead of the host's clock, always;
// any other carrier or sweep for the retention period from first sight, at
// least 31 days. It returns how many were dropped.
func (s *Service) Prune(ctx context.Context) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.unix()
	n := 0
	old := func(at int64, ok bool) bool { return ok && now-at >= s.retention }
	firstSeen := func(c string) (int64, bool) {
		l := s.lines[c]
		if l == nil || l.At == nil {
			return 0, false
		}
		return *l.At, true
	}
	envs := make([]*envEntry, 0, len(s.envelopes))
	for _, e := range s.envelopes {
		envs = append(envs, e)
	}
	sort.Slice(envs, func(i, j int) bool { return envs[i].txid < envs[j].txid })
	for _, e := range envs {
		if s.open(e, now) || (int64(e.created) > now+WindowAfter && s.status(e.c) != StatusRetracted) { //nolint:gosec // at most MaxTime
			continue
		}
		if old(firstSeen(e.c)) {
			s.drop(e.c, "retention")
			n++
		}
	}
	rcs := make([]*rcptEntry, 0, len(s.receipts))
	for _, r := range s.receipts {
		rcs = append(rcs, r)
	}
	sort.Slice(rcs, func(i, j int) bool { return rcs[i].txid < rcs[j].txid })
	for _, r := range rcs {
		if old(firstSeen(r.c)) {
			s.drop(r.c, "retention")
			n++
		}
	}
	sws := make([]*sweepEntry, 0, len(s.sweeps))
	for _, sw := range s.sweeps {
		sws = append(sws, sw)
	}
	sort.Slice(sws, func(i, j int) bool { return sws[i].txid < sws[j].txid })
	for _, sw := range sws {
		at, ok := s.sweepSeen[sw.txid]
		if old(at, ok) {
			for o := range sw.offices {
				s.queueDelete(sw.txid, o)
			}
			s.forget(sw.txid, "")
			s.host.Metrics().Inc("bbox_dropped_total", map[string]string{"why": "retention"}, 1)
			n++
		}
	}
	s.flush(ctx)
	return n
}

// flush deletes dropped outputs from storage. It runs after every
// admission and after retention, so a dropped output is gone at once. A
// store that cannot delete keeps what was dropped; the queue is not kept
// for it.
func (s *Service) flush(ctx context.Context) {
	if s.storage == nil || len(s.pending) == 0 {
		return
	}
	del, ok := s.storage.(Deleter)
	if !ok {
		if !s.warnedNoDelete {
			s.host.Log("ls_bbox cannot delete dropped outputs: the storage has no DeleteOutput", nil)
		}
		s.warnedNoDelete = true
		s.pending = map[string]pendingDelete{}
		return
	}
	keys := make([]string, 0, len(s.pending))
	for k := range s.pending {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := s.pending[k]
		h, err := chainhash.NewHashFromHex(p.txid)
		if err != nil {
			delete(s.pending, k)
			continue
		}
		if err := del.DeleteOutput(ctx, &transaction.Outpoint{Txid: *h, Index: 0}, p.topic); err != nil {
			s.host.Log("ls_bbox could not delete a dropped output", map[string]any{"txid": p.txid, "err": err.Error()})
			continue
		}
		delete(s.pending, k)
	}
}

// ---- questions (spec sections 7.2, 7.3 and 8.3)

// Classified is a question, validated, and whether this host prices it.
type Classified struct {
	Q      *boxrec.Question
	Priced bool
}

// Classify validates a question and names its class, as the terms route
// and the engine ask it. A question for another service, one no class
// has, or one naming an office the host does not carry is an error.
func (s *Service) Classify(service string, query json.RawMessage) (*Classified, error) {
	if service != boxrec.LookupService {
		return nil, fmt.Errorf("ls_bbox: this service is %s, not %s", boxrec.LookupService, service)
	}
	var raw any
	if len(query) == 0 || json.Unmarshal(query, &raw) != nil {
		return nil, errors.New("ls_bbox: the query is not an object")
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("ls_bbox: the query is not an object")
	}
	q, err := boxrec.ParseQuery(obj)
	if err != nil {
		return nil, fmt.Errorf("ls_bbox: refused: %s: %s", boxrec.Reason(err), strings.TrimPrefix(strings.TrimPrefix(err.Error(), boxrec.ErrQuery.Error()), ": "))
	}
	if !s.offices[q.Office] {
		return nil, fmt.Errorf("ls_bbox: this host does not carry office %s", q.Office)
	}
	return &Classified{Q: q, Priced: !q.Class.Free && s.priced[q.Class.Name]}, nil
}

func cmp(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Answer answers a validated question: the txids of the outputs it names,
// each output 0. paid says the question was paid for (or asked where no
// payment is due); a class this host prices is refused otherwise.
func (s *Service) Answer(c *Classified, paid bool) ([]string, error) {
	q := c.Q
	if c.Priced && !paid {
		return nil, fmt.Errorf("ls_bbox: %s is priced on this host; ask its terms route's /lookup over BRC-104", q.Class.Name)
	}
	s.host.Metrics().Inc("bbox_lookups_total", map[string]string{"class": q.Class.Name}, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.unix()
	key := hex.EncodeToString(q.Key)
	envs := func(keep func(*envEntry) bool) []*envEntry {
		var out []*envEntry
		for t := range s.envByTo[q.Office+"|"+key] {
			if e := s.envelopes[t]; e != nil && keep(e) {
				out = append(out, e)
			}
		}
		return out
	}
	page := func(list []*envEntry) []string {
		sort.Slice(list, func(i, j int) bool {
			if list[i].created != list[j].created {
				return list[i].created < list[j].created
			}
			return cmp(list[i].txid, list[j].txid) < 0
		})
		out := []string{}
		for _, e := range list {
			if q.AfterTxid != "" && !(e.created > q.AfterCreated || (e.created == q.AfterCreated && e.txid > q.AfterTxid)) {
				continue
			}
			if len(out) == q.Class.Page {
				break
			}
			out = append(out, e.txid)
		}
		return out
	}
	switch q.Class.Name {
	case "inbox", "inbox-after":
		return page(envs(func(e *envEntry) bool { return s.open(e, now) })), nil
	case "box", "box-after":
		return page(envs(func(e *envEntry) bool { return e.box == q.Box && s.open(e, now) })), nil
	case "sender", "sender-after":
		from := hex.EncodeToString(q.From)
		return page(envs(func(e *envEntry) bool { return e.from == from && s.open(e, now) })), nil
	case "history", "history-after":
		return page(envs(func(e *envEntry) bool {
			st := s.status(e.c)
			return (st == StatusHeld || st == StatusAcknowledged) && !s.open(e, now) && int64(e.created) <= now+WindowAfter //nolint:gosec // at most MaxTime
		})), nil
	case "receipt":
		t := hashOrder(q.ReceiptFor)
		var list []*rcptEntry
		for x := range s.rcptByBy[q.Office+"|"+key] {
			if r := s.receipts[x]; r != nil && r.acks[t] && s.status(r.c) == StatusHeld {
				list = append(list, r)
			}
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].created != list[j].created {
				return list[i].created < list[j].created
			}
			return cmp(list[i].txid, list[j].txid) < 0
		})
		out := []string{}
		for _, r := range list {
			if len(out) == q.Class.Page {
				break
			}
			out = append(out, r.txid)
		}
		return out, nil
	case "sweep":
		op := outpoint(q.SpentTxid, q.SpentVout)
		var list []*sweepEntry
		for x := range s.sweepsByOp[q.Office+"|"+op] {
			if sw := s.sweeps[x]; sw != nil {
				list = append(list, sw)
			}
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].height != list[j].height {
				return list[i].height < list[j].height
			}
			return cmp(list[i].txid, list[j].txid) < 0
		})
		out := []string{}
		for _, sw := range list {
			if len(out) == q.Class.Page {
				break
			}
			out = append(out, sw.txid)
		}
		return out, nil
	}
	return nil, fmt.Errorf("ls_bbox: no answer for class %s", q.Class.Name)
}

// Lookup implements module.LookupService: asked through the engine, a class
// the host prices is refused here. The answer is a formula the engine
// hydrates from its storage, as the TypeScript engine hydrates one.
func (s *Service) Lookup(_ context.Context, q *lookup.LookupQuestion) (*lookup.LookupAnswer, error) {
	if q == nil {
		return nil, errors.New("ls_bbox: no question")
	}
	c, err := s.Classify(q.Service, q.Query)
	if err != nil {
		return nil, err
	}
	txids, err := s.Answer(c, false)
	if err != nil {
		return nil, err
	}
	ans := &lookup.LookupAnswer{Type: lookup.AnswerTypeFormula, Formulas: make([]lookup.LookupFormula, 0, len(txids))}
	for _, t := range txids {
		h, err := chainhash.NewHashFromHex(t)
		if err != nil {
			return nil, err
		}
		ans.Formulas = append(ans.Formulas, lookup.LookupFormula{Outpoint: &transaction.Outpoint{Txid: *h, Index: 0}})
	}
	return ans, nil
}

// Hydrate is an answer with each output's BEEF, as the engine hydrates
// one, read from the host's storage: for the terms route, which answers
// outside the engine.
func (s *Service) Hydrate(ctx context.Context, txids []string) ([]bmodule.HydratedOutput, error) {
	s.mu.Lock()
	storage := s.storage
	s.mu.Unlock()
	if storage == nil {
		return nil, errors.New("ls_bbox: storage is not restored yet")
	}
	out := make([]bmodule.HydratedOutput, 0, len(txids))
	for _, t := range txids {
		h, err := chainhash.NewHashFromHex(t)
		if err != nil {
			continue
		}
		o, err := storage.FindOutput(ctx, &transaction.Outpoint{Txid: *h, Index: 0}, "")
		if err != nil {
			return nil, err
		}
		if o == nil || o.Beef == nil {
			continue
		}
		out = append(out, bmodule.HydratedOutput{Beef: o.Beef, OutputIndex: 0})
	}
	return out, nil
}

// Restored is whether the index was rebuilt from storage.
func (s *Service) Restored() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storage != nil
}

// ---- restore (spec section 8.5)

// Restore implements module.Restorer: it rebuilds the index. The rows from
// the journal, then every output of this module's topics the host hands
// over (a row handed without its BEEF is read again from storage with it),
// then every carrier and sweep the rows name that the unspent rows did not
// carry (a spent tombstone, say), read from storage. A carrier the rows
// say was dropped is not indexed, and its output is deleted. Statuses are
// functions of the set, so the order storage returns rows in changes
// nothing. A storage failure propagates: a restore that skipped a sweep
// would serve retracted envelopes. The service's lock is held throughout,
// so an admission waits for the restore.
func (s *Service) Restore(ctx context.Context, outputs []bmodule.Output, storage bmodule.RestoreStorage) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clear()
	lines, err := s.journal.Load()
	if err != nil {
		return 0, err
	}
	for i := range lines {
		s.apply(&lines[i])
	}
	s.storage = storage
	seen := map[string]bool{}
	var queue []bmodule.Output
	for _, o := range outputs {
		k := o.Topic + "|" + o.Outpoint.Txid.String()
		if _, ok := s.officeOf(o.Topic); !ok || o.Outpoint.Index != 0 || seen[k] {
			continue
		}
		seen[k] = true
		queue = append(queue, o)
	}
	type named struct{ topic, txid string }
	var names []named
	for _, c := range s.lineOrder {
		if l := s.lines[c]; l != nil && s.offices[l.Office] {
			names = append(names, named{topicOf(l.Office), l.Txid})
		}
	}
	for _, txid := range s.sweepOrder {
		for _, office := range s.sweepOffices[txid].order {
			if s.offices[office] {
				names = append(names, named{topicOf(office), txid})
			}
		}
	}
	indexed := 0
	take := func(o bmodule.Output) error {
		row := o
		if row.Beef == nil {
			full, err := storage.FindOutput(ctx, &o.Outpoint, o.Topic)
			if err != nil {
				return err
			}
			if full != nil {
				row = *full
			}
		}
		var tx *transaction.Transaction
		if row.Beef != nil {
			if t, err := parseFor(row.Beef, o.Outpoint.Txid); err == nil {
				tx = t
			}
		}
		txid := o.Outpoint.Txid.String()
		if tx == nil {
			s.host.Log("ls_bbox could not restore an output without its transaction", map[string]any{"topic": o.Topic, "txid": txid})
			return nil
		}
		office, _ := s.officeOf(o.Topic)
		ok, err := s.index(office, tx, 0)
		if err != nil {
			s.host.Log("ls_bbox could not restore an output", map[string]any{"topic": o.Topic, "txid": txid, "err": err.Error()})
			return nil
		}
		if ok {
			indexed++
		}
		return nil
	}
	for _, o := range queue {
		if err := take(o); err != nil {
			return 0, err
		}
	}
	for _, n := range names {
		k := n.topic + "|" + n.txid
		if seen[k] {
			continue
		}
		seen[k] = true
		h, err := chainhash.NewHashFromHex(n.txid)
		if err != nil {
			continue
		}
		o, err := storage.FindOutput(ctx, &transaction.Outpoint{Txid: *h, Index: 0}, n.topic)
		if err != nil {
			return 0, err
		}
		if o != nil {
			if err := take(*o); err != nil {
				return 0, err
			}
		}
	}
	s.flush(ctx)
	return indexed, nil
}

// GetDocumentation implements module.LookupService.
func (*Service) GetDocumentation() string {
	return strings.Join([]string{
		"# ls_bbox",
		"",
		"Answers every office this host carries. Free classes: `inbox`, `inbox-after`,",
		"`box`, `box-after`, `sender`, `sender-after` (open envelopes to a key, 64 a page,",
		"by created then txid), `receipt` (answered receipts by a key listing a txid, at",
		"most 8) and `sweep` (sweeps spending an outpoint, at most 8). Priceable:",
		"`history`, `history-after` (envelopes to a key no longer open). A question must",
		"carry exactly the members of one class; anything else is refused with an error.",
		"An empty answer means only that this host holds nothing that answers.",
	}, "\n")
}

// GetMetaData implements module.LookupService.
func (*Service) GetMetaData() *overlay.MetaData {
	return &overlay.MetaData{Name: boxrec.LookupService, Description: "Answers bbox envelopes, receipts and sweeps by office and key."}
}
