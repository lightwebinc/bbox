package module

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	clients "github.com/bsv-blockchain/go-sdk/auth/clients/authhttp"
	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/termsroute"
	"github.com/lightwebinc/bcommon/testchain"

	"github.com/lightwebinc/bbox/boxrec"
)

// The terms route over real HTTP (paid.test.ts): prices and the terms
// document, free questions with and without BRC-104, a priced question
// answered 402 and then answered once paid by go-sdk's own client, and the
// payee ledger in bbox's layout. The route's own hardening (sessions,
// handshake and response budgets, replay, every refusal of a payment,
// acceptance, the ledger across a restart) is bcommon termsroute's and is
// tested there, in Go and in TypeScript.

var payeeKey = func() *ec.PrivateKey { k, _ := ec.PrivateKeyFromBytes(bytes.Repeat([]byte{0x11}, 32)); return k }()

// both are the module's chain and the payment chain as one header source.
type both struct {
	a, b interface {
		IsValidRootForHeight(context.Context, *chainhash.Hash, uint32) (bool, error)
		CurrentHeight(context.Context) (uint32, error)
	}
}

func (x both) IsValidRootForHeight(ctx context.Context, root *chainhash.Hash, h uint32) (bool, error) {
	if ok, _ := x.a.IsValidRootForHeight(ctx, root, h); ok {
		return true, nil
	}
	return x.b.IsValidRootForHeight(ctx, root, h)
}

func (x both) CurrentHeight(ctx context.Context) (uint32, error) { return x.b.CurrentHeight(ctx) }

// payer is a paying client: a key's wallet, and CreateAction paying from a
// mature coinbase of the payment chain.
type payer struct {
	*wallet.CompletedProtoWallet
	t     *testing.T
	chain *testchain.Chain
	key   *ec.PrivateKey
	mu    sync.Mutex
	spent map[string]bool
}

func newPayer(t *testing.T, chain *testchain.Chain, p *party) *payer {
	addr, _ := script.NewAddressFromPublicKey(p.key.PubKey(), true)
	if _, err := chain.Generate(testchain.Maturity+8, addr.AddressString); err != nil {
		t.Fatal(err)
	}
	return &payer{CompletedProtoWallet: p.w, t: t, chain: chain, key: p.key, spent: map[string]bool{}}
}

func (c *payer) coin() *transaction.Transaction {
	c.mu.Lock()
	defer c.mu.Unlock()
	addr, _ := script.NewAddressFromPublicKey(c.key.PubKey(), true)
	lock, _ := p2pkh.Lock(addr)
	tip := c.chain.Height()
	for _, id := range c.chain.Txids() {
		if c.spent[id] {
			continue
		}
		tx := c.chain.Tx(id)
		if tx == nil || len(tx.Inputs) != 1 || tx.Inputs[0].SourceTXID == nil || *tx.Inputs[0].SourceTXID != (chainhash.Hash{}) {
			continue
		}
		mp, h, ok := c.chain.Proof(id)
		if !ok || tip+1-h < testchain.Maturity || !bytes.Equal(*tx.Outputs[0].LockingScript, *lock) {
			continue
		}
		tx.MerklePath = mp
		c.spent[id] = true
		return tx
	}
	c.t.Fatal("no mature coin")
	return nil
}

func (c *payer) CreateAction(_ context.Context, args wallet.CreateActionArgs, _ string) (*wallet.CreateActionResult, error) {
	src := c.coin()
	tx := transaction.NewTransaction()
	unlock, _ := p2pkh.Unlock(c.key, nil)
	if err := tx.AddInputFrom(src.TxID().String(), 0, src.Outputs[0].LockingScript.String(), src.Outputs[0].Satoshis, unlock); err != nil {
		return nil, err
	}
	tx.Inputs[0].SourceTransaction = src
	ls := script.Script(args.Outputs[0].LockingScript)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: args.Outputs[0].Satoshis, LockingScript: &ls})
	addr, _ := script.NewAddressFromPublicKey(c.key.PubKey(), true)
	change, _ := p2pkh.Lock(addr)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: src.Outputs[0].Satoshis - args.Outputs[0].Satoshis - 500, LockingScript: change})
	if err := tx.Sign(); err != nil {
		return nil, err
	}
	beef, err := tx.AtomicBEEF(false)
	if err != nil {
		return nil, err
	}
	return &wallet.CreateActionResult{Txid: *tx.TxID(), Tx: beef}, nil
}

type paidRig struct {
	*engine
	srv    *httptest.Server
	client *party
	payer  *payer
	read   string
	dir    string
}

// newPaidRig is a module with one open envelope to the client and one the
// client acknowledged (history answers it), its terms route on a test
// server, and a client that pays from the payment chain.
func newPaidRig(t *testing.T, prices string) *paidRig {
	t.Helper()
	chain := newTestChain(11000)
	payChain := testchain.New(700)
	node := httptest.NewServer(payChain)
	t.Cleanup(node.Close)
	p, err := termsroute.ParsePrices(prices, Classes, "BBOX_PRICES")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c := baseConfig(office)
	c.Prices, c.Listen, c.PayeeKey, c.StateDir = p, "127.0.0.1:0", payeeKey, dir
	if c.Accept, err = termsroute.ParseAcceptConfig("BBOX", env(map[string]string{"BBOX_ARCADE_URL": node.URL + "/arcade", "BBOX_ASSET_URL": node.URL})); err != nil {
		t.Fatal(err)
	}
	c.Accept.Policy.Wait, c.Accept.Policy.Poll = 2*time.Second, 50*time.Millisecond
	host := newFakeHost()
	m, err := Mount(host, c, []string{topic}, &MemoryJournal{}, both{chain, payChain}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	st := newMemStorage()
	if _, err := m.LS.Restore(context.Background(), nil, st); err != nil {
		t.Fatal(err)
	}
	e := &engine{t: t, host: host, m: m, ls: m.LS, storage: st, journal: &MemoryJournal{}, chain: chain, clock: &clock{now: time.Now().Unix()}}
	client := newParty("client")
	tree := fundingTree(t, alice, 2, chain)
	n := time.Now().Unix()
	// One open envelope, and one the client has acknowledged: history answers it.
	open := mintCarrier(t, tree, 0, alice, envelopeRecord(t, letter{office: office, from: alice, to: client, box: "inbox", created: u64(n - 60)}), lockTime(0))
	read := mintCarrier(t, tree, 1, alice, envelopeRecord(t, letter{office: office, from: alice, to: client, box: "inbox", created: u64(n - 120)}), lockTime(0))
	rc := mintCarrier(t, fundingTree(t, client, 1, chain), 0, client, receiptRecord(t, office, client, [][32]byte{commitment(read)}, u64(n)), lockTime(0))
	for _, x := range []*transaction.Transaction{open, read, rc} {
		zero(t, e.submitTx(x, ""))
	}
	srv := httptest.NewServer(m.Front.Front)
	t.Cleanup(srv.Close)
	return &paidRig{engine: e, srv: srv, client: client, payer: newPayer(t, payChain, client), read: idOf(read), dir: dir}
}

func askBody(q map[string]string) []byte {
	b, _ := json.Marshal(map[string]any{"service": boxrec.LookupService, "query": q})
	return b
}

func post(t *testing.T, url string, body []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// answerTxids are the txids of an output-list answer's outputs.
func answerTxids(t *testing.T, body []byte) []string {
	t.Helper()
	var a struct {
		Type    string `json:"type"`
		Outputs []struct {
			Beef        []int  `json:"beef"`
			OutputIndex uint32 `json:"outputIndex"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(body, &a); err != nil || a.Type != "output-list" {
		t.Fatalf("not an output list: %s", body)
	}
	out := []string{}
	for _, o := range a.Outputs {
		b := make([]byte, len(o.Beef))
		for i, x := range o.Beef {
			b[i] = byte(x)
		}
		tx, err := parse(b)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, idOf(tx))
	}
	return out
}

func TestPricesAndTermsDocument(t *testing.T) {
	p, err := termsroute.ParsePrices("history-after=7, history=5", Classes, "BBOX_PRICES")
	if err != nil || p["history-after"] != 7 || p["history"] != 5 || len(p) != 2 {
		t.Fatal(p, err)
	}
	if p, err := termsroute.ParsePrices("", Classes, "BBOX_PRICES"); err != nil || len(p) != 0 {
		t.Fatal(p, err)
	}
	for _, bad := range []string{"inbox=1", "history", "history=-1", "history=01", "history=1.5", "history=1,history=2", "nosuch=1", "history=9007199254740992"} {
		if _, err := termsroute.ParsePrices(bad, Classes, "BBOX_PRICES"); err == nil {
			t.Errorf("%q", bad)
		}
	}
	p, _ = termsroute.ParsePrices("history-after=7,history=5", Classes, "BBOX_PRICES")
	b, _ := json.Marshal(termsroute.TermsDocument(boxrec.LookupService, Classes, p))
	if string(b) != `{"service":"ls_bbox","terms":1,"classes":[{"class":"history","satoshis":5},{"class":"history-after","satoshis":7}]}` {
		t.Fatal(string(b))
	}
}

func TestClassAndAfterPricedTogether(t *testing.T) {
	for _, bad := range []string{"history=5", "history-after=5", "history=5,history-after=0", "history=0,history-after=5"} {
		if _, err := termsroute.ParsePrices(bad, Classes, "BBOX_PRICES"); err == nil || !strings.Contains(err.Error(), "priced together") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	for _, good := range []string{"history=5,history-after=6", "history=0", "history-after=0", "history=0,history-after=0"} {
		if _, err := termsroute.ParsePrices(good, Classes, "BBOX_PRICES"); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
}

func TestTermsAndFreeQuestions(t *testing.T) {
	r := newPaidRig(t, "history=5,history-after=5")
	res, err := http.Get(r.srv.URL + "/ls_bbox/terms")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	p, _ := termsroute.ParsePrices("history=5,history-after=5", Classes, "BBOX_PRICES")
	want, _ := json.Marshal(termsroute.TermsDocument(boxrec.LookupService, Classes, p))
	if res.StatusCode != 200 || !bytes.Equal(bytes.TrimSpace(body), want) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	code, body := post(t, r.srv.URL+"/lookup", askBody(map[string]string{"office": office, "to": r.client.hex}), nil)
	if code != 200 || len(answerTxids(t, body)) != 1 {
		t.Fatalf("%d %s", code, body)
	}
	for _, q := range []map[string]string{{"office": office2, "to": r.client.hex}, {"office": office, "to": r.client.hex, "extra": "x"}} {
		if code, body := post(t, r.srv.URL+"/lookup", askBody(q), nil); code != 400 {
			t.Fatalf("%v: %d %s", q, code, body)
		}
	}
	if code, _ := post(t, r.srv.URL+"/lookup", []byte("nope"), nil); code != 400 {
		t.Fatal(code)
	}
	if res, _ := http.Get(r.srv.URL + "/nosuch"); res.StatusCode != 404 {
		t.Fatal(res.StatusCode)
	}
	if code, body := post(t, r.srv.URL+"/lookup", askBody(map[string]string{"office": office, "history": r.client.hex}), nil); code != 401 {
		t.Fatalf("a priced class is asked over BRC-104: %d %s", code, body)
	}
	// A class and its -after form are priced together: the walk cannot be
	// had page by page for nothing.
	if code, _ := post(t, r.srv.URL+"/lookup", askBody(map[string]string{"office": office, "history": r.client.hex, "after": "1:" + strings.Repeat("00", 32)}), nil); code != 401 {
		t.Fatal(code)
	}
	if code, _ := post(t, r.srv.URL+"/lookup", askBody(map[string]string{"office": office, "to": r.client.hex}), map[string]string{"x-bsv-auth-nonce": "x"}); code != 400 {
		t.Fatal(code)
	}
	// The engine's own route refuses a priced class with an error that says
	// where to ask.
	if _, err := r.ls.Lookup(context.Background(), question(map[string]string{"office": office, "history": r.client.hex})); err == nil || !strings.Contains(err.Error(), "priced on this host") {
		t.Fatal(err)
	}

	none := newPaidRig(t, "")
	if res, _ := http.Get(none.srv.URL + "/ls_bbox/terms"); res.StatusCode != 404 {
		t.Fatal("a host that prices nothing serves no terms")
	}
	code, body = post(t, none.srv.URL+"/lookup", askBody(map[string]string{"office": office, "history": none.client.hex}), nil)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	eq(t, answerTxids(t, body), []string{none.read}, "unpriced history")
}

func TestOverBRC104FreeAndPaid(t *testing.T) {
	r := newPaidRig(t, "history=5,history-after=5")
	af := clients.New(r.payer, clients.WithoutLogging())
	fetch := func(q map[string]string) (*http.Response, []byte) {
		res, err := af.Fetch(context.Background(), r.srv.URL+"/lookup", &clients.SimplifiedFetchRequestOptions{Method: http.MethodPost, Headers: map[string]string{"content-type": "application/json"}, Body: askBody(q)})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res, body
	}
	free, _ := fetch(map[string]string{"office": office, "to": r.client.hex})
	if free.StatusCode != 200 || free.Header.Get("x-bsv-auth-identity-key") != payeeKey.PubKey().ToDERHex() {
		t.Fatalf("not signed by the payee: %d %v", free.StatusCode, free.Header)
	}
	paid, body := fetch(map[string]string{"office": office, "history": r.client.hex})
	if paid.StatusCode != 200 || paid.Header.Get("x-bsv-payment-satoshis-paid") != "5" {
		t.Fatalf("%d %s %v", paid.StatusCode, body, paid.Header)
	}
	eq(t, answerTxids(t, body), []string{r.read}, "paid history")
	// A second question is a second payment.
	again, _ := fetch(map[string]string{"office": office, "history": r.client.hex})
	if again.StatusCode != 200 {
		t.Fatal(again.StatusCode)
	}
	if n := r.host.count("bbox_payments_total", map[string]string{"decision": "requested", "reason": "no-payment"}); n != 2 {
		t.Fatalf("requested %v", n)
	}
	// Each payment is a ledger line in bbox's layout: the host's decision
	// before at, as the TypeScript module has always written it, and each
	// reads back as the payee's settle reads it.
	text, err := os.ReadFile(r.dir + "/" + termsroute.LedgerFile)
	if err != nil || bytes.Count(text, []byte("\n")) != 2 {
		t.Fatalf("ledger: %v\n%s", err, text)
	}
	for i, line := range bytes.Split(bytes.TrimSpace(text), []byte("\n")) {
		var l map[string]any
		if err := json.Unmarshal(line, &l); err != nil || l["class"] != "history" || l["senderIdentityKey"] != r.client.hex || l["satoshis"] != float64(5) {
			t.Fatalf("line %d: %s", i, line)
		}
		if d := bytes.Index(line, []byte(`"decision":`)); d < 0 || d > bytes.Index(line, []byte(`"at":`)) {
			t.Fatalf("line %d is not the spread layout: %s", i, line)
		}
		if _, ok := termsroute.ParseLedgerLine(line); !ok {
			t.Fatalf("line %d does not read back: %s", i, line)
		}
	}
}

func TestRouteAnswersAsTheEngine(t *testing.T) {
	r := newPaidRig(t, "")
	code, body := post(t, r.srv.URL+"/lookup", askBody(map[string]string{"office": office, "to": r.client.hex}), nil)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	eq(t, answerTxids(t, body), r.answer(map[string]string{"office": office, "to": r.client.hex}), "the route answers what the engine does")
}
