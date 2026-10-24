package module

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/guard"
	bmodule "github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/termsroute"

	"github.com/lightwebinc/bbox/boxrec"
)

// The module's test rig, the twin of the TypeScript tests' testutil.ts and
// testmint.ts: a host whose counters and log read back, the golden
// vectors, a local chain and parties that mint funding trees, envelopes,
// receipts, carriers and sweeps, an in-memory store behaving as the Go
// engine's where the module depends on it, and the engine's admission path
// over it with the module's managers and lookup service mounted.

// fakeHost records what the module counts and logs.
type fakeHost struct {
	mu       sync.Mutex
	counters map[string]float64
	gauges   map[string]func() float64
	lines    []logLine
}

type logLine struct {
	msg   string
	extra map[string]any
}

func newFakeHost() *fakeHost {
	return &fakeHost{counters: map[string]float64{}, gauges: map[string]func() float64{}}
}

func mkey(name string, labels map[string]string) string {
	ks := make([]string, 0, len(labels))
	for k := range labels {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	parts := make([]string, 0, len(ks))
	for _, k := range ks {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	if len(parts) == 0 {
		return name
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

func (h *fakeHost) Log(msg string, extra map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, logLine{msg, extra})
}

func (h *fakeHost) Metrics() bmodule.Metrics { return h }

func (h *fakeHost) Inc(name string, labels map[string]string, by float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counters[mkey(name, labels)] += by
}

func (h *fakeHost) Preset(name string, labels map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.counters[mkey(name, labels)]; !ok {
		h.counters[mkey(name, labels)] = 0
	}
}

func (h *fakeHost) Gauge(name string, fn func() float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gauges[name] = fn
}

// count is a counter's value; a counter never preset or counted is -1.
func (h *fakeHost) count(name string, labels map[string]string) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.counters[mkey(name, labels)]
	if !ok {
		return -1
	}
	return v
}

func (h *fakeHost) has(name string, labels map[string]string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.counters[mkey(name, labels)]
	return ok
}

func (h *fakeHost) logged(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, l := range h.lines {
		if strings.Contains(l.msg, msg) {
			n++
		}
	}
	return n
}

// ---- the golden vectors (testdata/vectors, written by the Go codec)

type txVector struct {
	Name          string `json:"name"`
	Txid          string `json:"txid"`
	Beef          string `json:"beef"`
	PreviousCoins []struct {
		InputIndex int    `json:"inputIndex"`
		Txid       string `json:"txid"`
		Vout       uint32 `json:"vout"`
	} `json:"previousCoins"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	Admits  *struct {
		Kind           string   `json:"kind"`
		OutputsToAdmit []uint32 `json:"outputsToAdmit"`
		CoinsToRetain  []uint32 `json:"coinsToRetain"`
		Commitment     string   `json:"commitment"`
		Record         string   `json:"record"`
		FundingTxid    string   `json:"fundingTxid"`
		FundingVout    uint32   `json:"fundingVout"`
		Spent          []string `json:"spent"`
	} `json:"admits"`
}

type txVectors struct {
	Office                  string   `json:"office"`
	Topic                   string   `json:"topic"`
	SenderIdentityKey       string   `json:"senderIdentityKey"`
	RecipientIdentityKey    string   `json:"recipientIdentityKey"`
	ReceiptAcknowledgesTxid []string `json:"receiptAcknowledgesTxid"`
	SweepRetractsOutpoints  []string `json:"sweepRetractsOutpoints"`
	MaxBeef                 int      `json:"maxBeef"`
	Headers                 []struct {
		Height     uint32 `json:"height"`
		MerkleRoot string `json:"merkleRoot"`
	} `json:"headers"`
	Transactions []txVector `json:"transactions"`
}

func readVectors(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("../testdata/vectors/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func loadTxVectors(t *testing.T) *txVectors {
	t.Helper()
	v := &txVectors{}
	readVectors(t, "transaction-v1.json", v)
	return v
}

func (v *txVectors) byName(t *testing.T, name string) *txVector {
	t.Helper()
	for i := range v.Transactions {
		if v.Transactions[i].Name == name {
			return &v.Transactions[i]
		}
	}
	t.Fatalf("no transaction vector %s", name)
	return nil
}

func (x *txVector) beef(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString(x.Beef)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (x *txVector) held() []int {
	out := []int{}
	for _, c := range x.PreviousCoins {
		out = append(out, c.InputIndex)
	}
	return out
}

// ---- a local chain: blocks of two, each a minted transaction beside a fixed sibling

type testChain struct {
	mu     sync.Mutex
	height uint32
	roots  map[uint32]chainhash.Hash
}

func newTestChain(first uint32) *testChain {
	return &testChain{height: first, roots: map[uint32]chainhash.Hash{}}
}

// withVectors adds the golden vectors' headers.
func (c *testChain) withVectors(t *testing.T, v *txVectors) *testChain {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, h := range v.Headers {
		r, err := chainhash.NewHashFromHex(h.MerkleRoot)
		if err != nil {
			t.Fatal(err)
		}
		c.roots[h.Height] = *r
	}
	return c
}

func (c *testChain) IsValidRootForHeight(_ context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.roots[height]
	return ok && r.Equal(*root), nil
}

func (c *testChain) CurrentHeight(context.Context) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var top uint32
	for h := range c.roots {
		if h > top {
			top = h
		}
	}
	return top, nil
}

var sibling = func() chainhash.Hash {
	var h chainhash.Hash
	for i := range h {
		h[i] = 0x55
	}
	return h
}()

// mine gives tx a proof in a new block and returns it.
func (c *testChain) mine(t *testing.T, tx *transaction.Transaction) *transaction.Transaction {
	t.Helper()
	c.mu.Lock()
	h := c.height
	c.height++
	c.mu.Unlock()
	txid := tx.TxID()
	yes := true
	sib := sibling
	tx.MerklePath = transaction.NewMerklePath(h, [][]*transaction.PathElement{{
		{Offset: 0, Hash: &sib},
		{Offset: 1, Hash: txid, Txid: &yes},
	}})
	root, err := tx.MerklePath.ComputeRoot(txid)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.roots[h] = *root
	c.mu.Unlock()
	return tx
}

// ---- parties

func sha(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// party is one identity: its key, its wallet, and its envelope key as any
// reader derives it.
type party struct {
	label  string
	key    *ec.PrivateKey
	id     []byte
	hex    string
	w      *wallet.CompletedProtoWallet
	envKey *ec.PublicKey
}

func newParty(label string) *party { return partyFromKey(label, sha("bbox/test/party/"+label)) }

func partyFromKey(label string, k []byte) *party {
	priv, _ := ec.PrivateKeyFromBytes(k)
	w, err := wallet.NewCompletedProtoWallet(priv)
	if err != nil {
		panic(err)
	}
	pub := priv.PubKey()
	env, err := boxrec.EnvelopeDerivation.ExpectedLockingKey(pub)
	if err != nil {
		panic(err)
	}
	id := pub.Compressed()
	return &party{label: label, key: priv, id: id, hex: hex.EncodeToString(id), w: w, envKey: env}
}

var nowhere struct {
	mu sync.Mutex
	n  int
}

// fromNowhere is a transaction of the given outputs whose one unsigned
// input spends an outpoint no one holds.
func fromNowhere(outs ...*transaction.TransactionOutput) *transaction.Transaction {
	nowhere.mu.Lock()
	n := nowhere.n
	nowhere.n++
	nowhere.mu.Unlock()
	src, _ := chainhash.NewHash(sha(fmt.Sprintf("bbox/test/nowhere/%d/%d", n, time.Now().UnixNano())))
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{SourceTXID: src, SourceTxOutIndex: 0, UnlockingScript: &script.Script{}, SequenceNumber: transaction.MaxTxInSequenceNum})
	for _, o := range outs {
		tx.AddOutput(o)
	}
	return tx
}

func scriptOf(b []byte) *script.Script { s := script.Script(b); return &s }

// fundingTree is a mined funding tree of n outputs of 1000 satoshis for
// owner.
func fundingTree(t *testing.T, owner *party, n int, chain *testChain) *transaction.Transaction {
	t.Helper()
	lock := boxrec.FundingScript(owner.envKey)
	outs := make([]*transaction.TransactionOutput, n)
	for i := range outs {
		outs[i] = &transaction.TransactionOutput{Satoshis: 1000, LockingScript: scriptOf(lock)}
	}
	return chain.mine(t, fromNowhere(outs...))
}

// letter is an envelope's fields.
type letter struct {
	office          string
	from, to        *party
	box             string
	created, expiry uint64
	body            string
}

// envelopeRecord is an envelope record: the plaintext sealed to the
// recipient in a signed BRC-169 envelope.
func envelopeRecord(t *testing.T, l letter) []byte {
	t.Helper()
	body := l.body
	if body == "" {
		body = "hello"
	}
	doc := &boxrec.Object{}
	doc.Set(boxrec.PlainBody, body)
	plain, err := boxrec.Canonical(doc)
	if err != nil {
		t.Fatal(err)
	}
	content, err := boxrec.SealEnvelope(context.Background(), l.from.w, "", l.from.id, l.to.id, plain, l.created)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := (&boxrec.Envelope{Office: l.office, To: l.to.id, Box: l.box, From: l.from.id, Created: l.created, Expires: l.expiry, Content: content}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// receiptRecord is a receipt record acknowledging commitments (hash byte
// order).
func receiptRecord(t *testing.T, office string, by *party, acks [][32]byte, created uint64) []byte {
	t.Helper()
	sorted := append([][32]byte(nil), acks...)
	sort.Slice(sorted, func(i, j int) bool { return bytes.Compare(sorted[i][:], sorted[j][:]) < 0 })
	rec, err := (&boxrec.Receipt{Office: office, By: by.id, Acks: sorted, Created: created}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// mintCarrier is a carrier of record spending output vout of owner's
// tree: one input signed under owner's envelope key, one output locked
// and signed under it and carrying the whole value.
func mintCarrier(t *testing.T, tree *transaction.Transaction, vout uint32, owner *party, record []byte, lockTime uint32) *transaction.Transaction {
	t.Helper()
	ctx := context.Background()
	lock, err := boxrec.EnvelopeDerivation.Lock(ctx, owner.w, "", [][]byte{record}, true)
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	tx.LockTime = lockTime
	tx.AddInputFromTx(tree, vout, boxrec.EnvelopeDerivation.Unlocker(ctx, owner.w, ""))
	tx.Inputs[0].SequenceNumber = 0
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: tree.Outputs[vout].Satoshis, LockingScript: lock})
	if err := tx.Sign(); err != nil {
		t.Fatal(err)
	}
	return tx
}

// mintSweep is a mined sweep of outputs vouts of owner's tree: its
// tombstone at output 0.
func mintSweep(t *testing.T, tree *transaction.Transaction, vouts []uint32, owner *party, chain *testChain) *transaction.Transaction {
	t.Helper()
	ctx := context.Background()
	tx := transaction.NewTransaction()
	var value uint64
	for _, v := range vouts {
		tx.AddInputFromTx(tree, v, boxrec.EnvelopeDerivation.Unlocker(ctx, owner.w, ""))
		value += tree.Outputs[v].Satoshis
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: value, LockingScript: scriptOf(boxrec.FundingScript(owner.envKey))})
	if err := tx.Sign(); err != nil {
		t.Fatal(err)
	}
	return chain.mine(t, tx)
}

// atomicOf is a transaction's Atomic BEEF, as a publisher submits it.
func atomicOf(t *testing.T, tx *transaction.Transaction) []byte {
	t.Helper()
	b, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// commitment is C = txid(K), hash byte order.
func commitment(tx *transaction.Transaction) [32]byte { return [32]byte(*tx.TxID()) }

func cOf(tx *transaction.Transaction) string {
	c := commitment(tx)
	return hex.EncodeToString(c[:])
}

func idOf(tx *transaction.Transaction) string { return tx.TxID().String() }

// byC sorts transactions by commitment, lowest first.
func byC(txs []*transaction.Transaction) []*transaction.Transaction {
	out := append([]*transaction.Transaction(nil), txs...)
	sort.Slice(out, func(i, j int) bool { return cOf(out[i]) < cOf(out[j]) })
	return out
}

// ---- the engine's storage, in memory

// memStorage is the engine's storage in memory, as the Go engine's store
// behaves where the module depends on it: FindOutput matches spent and
// unspent outputs, the unspent rows are what a restore is handed, and
// DeleteOutput deletes one output and leaves the applied transactions.
type memStorage struct {
	mu      sync.Mutex
	outputs map[string]*bmodule.Output
	applied map[string]bool
	n       int
	// noDelete, when set, hides DeleteOutput behind cannotDelete.
	// meanwhile, when set, runs once at the first FindOutput.
	meanwhile func()
}

func newMemStorage() *memStorage {
	return &memStorage{outputs: map[string]*bmodule.Output{}, applied: map[string]bool{}}
}

func skey(topic string, op *transaction.Outpoint) string { return topic + "|" + op.String() }

func (m *memStorage) FindOutput(_ context.Context, op *transaction.Outpoint, topic string) (*bmodule.Output, error) {
	m.mu.Lock()
	f := m.meanwhile
	m.meanwhile = nil
	m.mu.Unlock()
	if f != nil {
		f()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.outputs))
	for k := range m.outputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o := m.outputs[k]
		if o.Outpoint == *op && (topic == "" || o.Topic == topic) {
			c := *o
			return &c, nil
		}
	}
	return nil, nil
}

func (m *memStorage) DeleteOutput(_ context.Context, op *transaction.Outpoint, topic string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.outputs, skey(topic, op))
	return nil
}

// find is an output of topic, spent or not; nil when the store does not
// hold it.
func (m *memStorage) find(txid string, vout uint32, topic string) *bmodule.Output {
	h, _ := chainhash.NewHashFromHex(txid)
	m.mu.Lock()
	defer m.mu.Unlock()
	if o := m.outputs[skey(topic, &transaction.Outpoint{Txid: *h, Index: vout})]; o != nil {
		c := *o
		return &c
	}
	return nil
}

func (m *memStorage) isApplied(txid, topic string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applied[topic+"|"+txid]
}

func (m *memStorage) markSpent(txid string, vout uint32, topic string) {
	h, _ := chainhash.NewHashFromHex(txid)
	m.mu.Lock()
	defer m.mu.Unlock()
	if o := m.outputs[skey(topic, &transaction.Outpoint{Txid: *h, Index: vout})]; o != nil {
		o.Spent = true
	}
}

// utxos are the unspent rows of topic, in the order they were stored.
func (m *memStorage) utxos(topic string) []bmodule.Output {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []bmodule.Output
	for _, o := range m.outputs {
		if o.Topic == topic && !o.Spent {
			out = append(out, *o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score < out[j].Score })
	return out
}

// cannotDelete is a store with no DeleteOutput.
type cannotDelete struct{ m *memStorage }

func (c cannotDelete) FindOutput(ctx context.Context, op *transaction.Outpoint, topic string) (*bmodule.Output, error) {
	return c.m.FindOutput(ctx, op, topic)
}

// ---- the module and the engine's admission path

const (
	office  = "example_office_qzxkvbmwtr"
	office2 = "other_office_abcdefghij"
)

var (
	topic  = boxrec.TopicPrefix + office
	topic2 = boxrec.TopicPrefix + office2
)

// baseConfig is the configuration the tests use.
func baseConfig(offices ...string) *Config {
	if len(offices) == 0 {
		offices = []string{office, office2}
	}
	return &Config{Offices: offices, StateDir: "/nonexistent", RetentionDays: 31, MaxBEEF: boxrec.DefaultMaxBEEF, Prices: map[string]uint64{},
		Sessions: termsroute.SessionBound{Max: 100, TTL: 600 * time.Second}, Budget: termsroute.DefaultBudget, Response: termsroute.DefaultResponseBudget}
}

// clock is a clock the tests move, in seconds.
type clock struct {
	mu  sync.Mutex
	now int64
}

func (c *clock) time() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Unix(c.now, 0)
}

func (c *clock) set(s int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = s
}

// engine is the module and the engine's admission path over a store.
type engine struct {
	t       *testing.T
	host    *fakeHost
	m       *Mounted
	ls      *Service
	storage *memStorage
	journal *MemoryJournal
	chain   *testChain
	clock   *clock
	// naive returns every refusal as empty instructions, as a manager that
	// does not raise would; calls counts the managers' calls.
	naive bool
	calls int
	// spv verifies every submission's scripts and proofs against chain
	// before any manager runs, as the engine does.
	spv bool
}

type engineOpts struct {
	naive   bool
	spv     bool
	journal *MemoryJournal
	chain   *testChain
	storage *memStorage
	config  *Config
	now     int64
}

func newEngine(t *testing.T, o engineOpts) *engine {
	t.Helper()
	host := newFakeHost()
	if o.chain == nil {
		o.chain = newTestChain(9000)
	}
	if o.journal == nil {
		o.journal = &MemoryJournal{}
	}
	if o.storage == nil {
		o.storage = newMemStorage()
	}
	if o.config == nil {
		o.config = baseConfig()
	}
	cl := &clock{now: o.now}
	if cl.now == 0 {
		cl.now = time.Now().Unix()
	}
	var topics []string
	for _, off := range o.config.Offices {
		topics = append(topics, boxrec.TopicPrefix+off)
	}
	m, err := Mount(host, o.config, topics, o.journal, o.chain, cl.time)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.LS.Restore(context.Background(), nil, o.storage); err != nil {
		t.Fatal(err)
	}
	return &engine{t: t, host: host, m: m, ls: m.LS, storage: o.storage, journal: o.journal, chain: o.chain, clock: cl, naive: o.naive, spv: o.spv}
}

// admit is the engine's admission path: the outputs admitted, or an error
// when the manager refused. A transaction the topic applied is a duplicate
// the manager is never shown. The order is the Go engine's: outputs
// stored, held outputs it spends marked spent (retained) or deleted, the
// lookup services told, then the applied transaction recorded. The
// engine's own SPV is not what a module's tests exercise.
func (e *engine) admit(raw []byte, t string) ([]uint32, error) {
	ctx := bmodule.WithSubmission(context.Background(), raw)
	beef, tx, txid, err := guard.ParseBEEF(raw, guard.DefaultBound)
	if err != nil || tx == nil {
		return nil, fmt.Errorf("the engine refused the BEEF: %v", err)
	}
	st := e.storage
	if st.isApplied(txid.String(), t) {
		return []uint32{}, nil
	}
	if e.spv {
		if ok, err := spv.Verify(context.Background(), tx, e.chain, nil); err != nil || !ok {
			return nil, fmt.Errorf("the engine's SPV refused it: %v", err)
		}
	}
	tm := e.m.Module.Topics[t]
	if tm == nil {
		return nil, fmt.Errorf("no manager for %s", t)
	}
	st.mu.Lock()
	var coins []uint32
	held := map[uint32]*bmodule.Output{}
	for i, in := range tx.Inputs {
		if in.SourceTXID == nil {
			continue
		}
		if o := st.outputs[skey(t, &transaction.Outpoint{Txid: *in.SourceTXID, Index: in.SourceTxOutIndex})]; o != nil {
			coins = append(coins, uint32(i)) //nolint:gosec // test
			held[uint32(i)] = o              //nolint:gosec // test
		}
	}
	st.mu.Unlock()
	e.calls++
	ai, err := tm.IdentifyAdmissibleOutputs(ctx, beef, txid, coins)
	if err != nil {
		if _, refused := err.(*Refused); !refused || !e.naive {
			return nil, err
		}
		ai.OutputsToAdmit, ai.CoinsToRetain = nil, coins
	}
	atomicBytes, err := beef.AtomicBytes(txid)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	retain := map[uint32]bool{}
	for _, c := range ai.CoinsToRetain {
		retain[c] = true
	}
	for vin, o := range held {
		if retain[vin] {
			o.Spent = true
			o.ConsumedBy = append(o.ConsumedBy, &transaction.Outpoint{Txid: *txid, Index: 0})
		} else {
			delete(st.outputs, skey(t, &o.Outpoint))
		}
	}
	for _, vout := range ai.OutputsToAdmit {
		op := transaction.Outpoint{Txid: *txid, Index: vout}
		st.n++
		st.outputs[skey(t, &op)] = &bmodule.Output{Outpoint: op, Topic: t, Beef: atomicBytes, Score: float64(st.n),
			OutputScript: tx.Outputs[vout].LockingScript, Satoshis: tx.Outputs[vout].Satoshis}
	}
	st.mu.Unlock()
	for _, vout := range ai.OutputsToAdmit {
		if err := e.ls.OutputAdmittedByTopic(ctx, &bmodule.OutputAdmittedByTopic{Mode: bmodule.AdmitWholeTx, Outpoint: transaction.Outpoint{Txid: *txid, Index: vout}, Topic: t, AtomicBEEF: atomicBytes}); err != nil {
			return nil, err
		}
	}
	st.mu.Lock()
	st.applied[t+"|"+txid.String()] = true
	st.mu.Unlock()
	if ai.OutputsToAdmit == nil {
		return []uint32{}, nil
	}
	return ai.OutputsToAdmit, nil
}

// submit submits to topic t (the first office's when ""): the outputs
// admitted, or nil when the engine or a manager refused.
func (e *engine) submit(raw []byte, t string) []uint32 {
	e.t.Helper()
	if t == "" {
		t = topic
	}
	got, err := e.admit(raw, t)
	if err != nil {
		return nil
	}
	return got
}

func (e *engine) submitTx(tx *transaction.Transaction, t string) []uint32 {
	e.t.Helper()
	return e.submit(atomicOf(e.t, tx), t)
}

// answer is an engine answer: the txid of each output the engine hydrates
// from its storage, as the Go engine hydrates a formula.
func (e *engine) answer(q map[string]string) []string {
	e.t.Helper()
	got, err := lookupTxids(e.ls, e.storage, q)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

func question(q any) *lookup.LookupQuestion {
	b, _ := json.Marshal(q)
	return &lookup.LookupQuestion{Service: boxrec.LookupService, Query: b}
}

// lookupTxids asks ls a question through the engine's route and hydrates
// the formula from st, as the engine does: an output st does not hold is
// left out.
func lookupTxids(ls *Service, st *memStorage, q any) ([]string, error) {
	a, err := ls.Lookup(context.Background(), question(q))
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, f := range a.Formulas {
		if st != nil {
			o, _ := st.FindOutput(context.Background(), f.Outpoint, "")
			if o == nil || o.Beef == nil {
				continue
			}
			tx, err := parseFor(o.Beef, f.Outpoint.Txid)
			if err != nil {
				return nil, err
			}
			out = append(out, idOf(tx))
			continue
		}
		out = append(out, f.Outpoint.Txid.String())
	}
	return out, nil
}

func eq(t *testing.T, got, want []string, msg string) {
	t.Helper()
	if got == nil {
		got = []string{}
	}
	if want == nil {
		want = []string{}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s:\n got  %v\n want %v", msg, got, want)
	}
}

func ids(txs ...*transaction.Transaction) []string {
	out := make([]string, 0, len(txs))
	for _, tx := range txs {
		out = append(out, idOf(tx))
	}
	return out
}

func lockTime(n uint32) uint32 { return carrier.LockTime + n }
