package send

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/testchain"

	"github.com/lightwebinc/bbox/internal/limits"
	"github.com/lightwebinc/bbox/internal/state"
	"github.com/lightwebinc/bcommon/unicast"
)

// rig is one funded home over a local chain. Each open is the publisher
// the next command builds: the home as its files stand, and nothing an
// earlier publisher held in memory.
type rig struct {
	t     *testing.T
	dir   string
	chain *testchain.Chain
	rpc   *nodeapi.RPC
	asset *nodeapi.Asset
	// headers is the chain's own header source (a bridge's /v1).
	headers *headers.Client

	mu    sync.Mutex
	notes []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	chain := testchain.New(700)
	srv := httptest.NewServer(chain)
	t.Cleanup(srv.Close)
	r := &rig{t: t, dir: t.TempDir(), chain: chain,
		rpc: &nodeapi.RPC{URL: srv.URL + "/rpc", ID: "bbox"}, asset: &nodeapi.Asset{Base: srv.URL}, headers: headers.New(srv.URL)}
	w, err := bwallet.Create(r.dir, Profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := bwallet.FundFromCoinbase(context.Background(), w.Signer(), w.Pool, r.rpc, r.asset, 104, 0); err != nil {
		t.Fatal(err)
	}
	return r
}

// build is a publisher over the home, with trees of four outputs and the
// next minted when ahead or fewer are left. edit changes the state it
// loads.
func (r *rig) build(ahead uint32, edit func(*state.State)) *Engine {
	r.t.Helper()
	w, err := bwallet.Open(r.dir, Profile)
	if err != nil {
		r.t.Fatal(err)
	}
	st, err := state.Load(r.dir, w.Signer().IdentityHex())
	if err != nil {
		r.t.Fatal(err)
	}
	if edit != nil {
		edit(st)
	}
	e, err := New(st, w.Signer(), w.Pool, Legs{Settler: &publish.RPCSettler{RPC: r.rpc}, Chain: r.asset, Headers: r.headers,
		Hosts: &unicast.Set{Hosts: []string{"http://host.invalid"}}},
		Options{TreeCount: 4, TreeSats: limits.DefaultTreeSats, Ahead: ahead, Fees: mint.DefaultFees,
			Poll: 5 * time.Millisecond, Wait: 20 * time.Second})
	if err != nil {
		r.t.Fatal(err)
	}
	e.Verbose = true
	e.Note = func(format string, args ...any) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.notes = append(r.notes, fmt.Sprintf(format, args...))
	}
	return e
}

// open builds a publisher and starts it, as a command that spends does.
func (r *rig) open(ahead uint32, edit func(*state.State)) *Engine {
	r.t.Helper()
	e := r.build(ahead, edit)
	if err := e.Start(context.Background()); err != nil {
		r.t.Fatalf("start: %v\n%s", err, r.said())
	}
	return e
}

// said returns what the publishers noted since the last call.
func (r *rig) said() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := strings.Join(r.notes, "\n")
	r.notes = nil
	return out
}

// use spends the next funding output as a carrier does, and returns it as
// txid.vout.
func (r *rig) use(e *Engine) string {
	r.t.Helper()
	tree, vout, err := e.spend(context.Background())
	if err != nil {
		r.t.Fatalf("spend: %v\n%s", err, r.said())
	}
	e.markSpent(tree, vout)
	if err := e.St.Save(); err != nil {
		r.t.Fatal(err)
	}
	return fmt.Sprintf("%s.%d", tree.TxID(), vout)
}

// landed waits until the chain holds the one tree the home records as
// signed and not adopted, and returns its record. The publisher that
// minted it is then left as it is: a run that stopped.
func (r *rig) landed(e *Engine) state.PendingTree {
	r.t.Helper()
	if len(e.St.PendingTrees) != 1 {
		r.t.Fatalf("the records of trees signed and not adopted: %+v\n%s", e.St.PendingTrees, r.said())
	}
	p := e.St.PendingTrees[0]
	for end := time.Now().Add(20 * time.Second); r.chain.Tx(p.Tree.Txid) == nil; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			r.t.Fatalf("funding tree %s never reached the chain", p.Tree.Txid)
		}
	}
	return p
}

// down is a transport no request passes.
type down struct{}

func (down) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("the facade is down")
}

// unpublishable makes every publish of a funding tree fail.
func unpublishable(e *Engine) {
	e.trees.Facade = &publish.Facade{Base: "http://kept.invalid", HTTP: &http.Client{Transport: down{}}}
}

// Two trees a run signed and did not adopt are both on the chain while the
// current tree has outputs left. Both are held, in the order of their
// records, the current tree is spent to its last output, and each held tree
// is switched to once, from its first output: no output is stranded and
// none is spent twice.
func TestTwoRecoveredTreesAreHeldInOrder(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	// The first run mints the current tree, spends one output, and stops
	// while the tree it minted ahead is on its way.
	a := r.open(3, nil)
	spent := map[string]bool{r.use(a): true}
	current := a.St.Tree.Txid
	one := r.landed(a)
	// The second run knows nothing of that tree, mints another ahead, and
	// stops the same way.
	b := r.open(3, func(st *state.State) { st.DropPendingTree(one.Tree.Txid) })
	spent[r.use(b)] = true
	two := r.landed(b)
	if one.Tree.Txid == two.Tree.Txid || one.Coin.Outpoint() == two.Coin.Outpoint() {
		t.Fatalf("the two trees: %s for %s, %s for %s", one.Tree.Txid, one.Coin.Outpoint(), two.Tree.Txid, two.Coin.Outpoint())
	}
	r.said()

	// The next command finds both records.
	c := r.open(0, func(st *state.State) { st.KeepPendingTree(one) })
	want := []string{two.Tree.Txid, one.Tree.Txid}
	held := func() (out []string) {
		for _, h := range c.trees.Held() {
			out = append(out, h.Txid)
		}
		return out
	}
	ahead := func(st *state.State) (out []string) {
		for _, h := range st.Ahead {
			out = append(out, h.Txid)
		}
		return out
	}
	said := r.said()
	if c.St.Tree.Txid != current || fmt.Sprint(held()) != fmt.Sprint(want) || fmt.Sprint(ahead(c.St)) != fmt.Sprint(want) ||
		len(c.St.PendingTrees) != 2 || strings.Count(said, "is recovered and waits for the switch") != 2 {
		t.Fatalf("after the recovery: current %s, held %v, ahead %v, records %d\n%s", c.St.Tree.Txid, held(), ahead(c.St), len(c.St.PendingTrees), said)
	}
	for _, p := range []state.PendingTree{one, two} {
		for _, o := range c.Pool.Outputs() {
			if o.Outpoint() == p.Coin.Outpoint() {
				t.Fatalf("the spent fee coin %s is in the pool", o.Outpoint())
			}
		}
	}
	// Ten outputs are left: two on the current tree and four on each held
	// tree, in that order.
	var order []string
	for i := 0; i < 10; i++ {
		op := r.use(c)
		if spent[op] {
			t.Fatalf("funding output %s is spent twice\n%s", op, r.said())
		}
		spent[op] = true
		if id := op[:64]; len(order) == 0 || order[len(order)-1] != id {
			order = append(order, id)
		}
	}
	if fmt.Sprint(order) != fmt.Sprint(append([]string{current}, want...)) {
		t.Fatalf("the trees were spent in the order %v\n%s", order, r.said())
	}
	if len(c.St.Trees) != 3 || len(c.St.Ahead) != 0 || len(c.St.PendingTrees) != 0 || len(held()) != 0 {
		t.Fatalf("after every output: %d tree(s) adopted, ahead %v, records %d, held %v", len(c.St.Trees), ahead(c.St), len(c.St.PendingTrees), held())
	}
	for _, tr := range c.St.Trees {
		if tr.Remaining() != 0 {
			t.Fatalf("funding tree %s has %d output(s) stranded", tr.Txid, tr.Remaining())
		}
	}
	if said := r.said(); strings.Contains(said, "minted ahead by an earlier run") {
		t.Fatalf("a tree the library held was switched to behind its back:\n%s", said)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Held trees a command recorded and did not reach are switched to by the
// next commands, each of which holds nothing in memory, in the same order.
func TestHeldTreesAreSwitchedToByLaterCommands(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	a := r.open(3, nil)
	r.use(a)
	current := a.St.Tree.Txid
	one := r.landed(a)
	b := r.open(3, func(st *state.State) { st.DropPendingTree(one.Tree.Txid) })
	r.use(b)
	two := r.landed(b)
	// The command that recovers both spends nothing and ends.
	c := r.open(0, func(st *state.State) { st.KeepPendingTree(one) })
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var order []string
	for i := 0; i < 10; i++ {
		d := r.open(0, nil)
		if id := r.use(d)[:64]; len(order) == 0 || order[len(order)-1] != id {
			order = append(order, id)
		}
		if err := d.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	last := r.open(0, nil)
	if fmt.Sprint(order) != fmt.Sprint([]string{current, two.Tree.Txid, one.Tree.Txid}) ||
		len(last.St.Trees) != 3 || len(last.St.Ahead) != 0 || len(last.St.PendingTrees) != 0 {
		t.Fatalf("the trees were spent in the order %v: %d adopted, %d ahead, %d record(s)\n%s",
			order, len(last.St.Trees), len(last.St.Ahead), len(last.St.PendingTrees), r.said())
	}
}

// A publish that fails once the library adopted a new tree leaves nothing
// to do: a funding tree is published to no host, so the tree is the
// current one and the spend goes on.
func TestASpendWhoseTreeIsAdoptedAndNotPublished(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	e := r.open(0, nil)
	unpublishable(e)
	op := r.use(e)
	said := r.said()
	if e.St.Tree == nil || op != e.St.Tree.Txid+".0" || len(e.St.Trees) != 1 || len(e.St.PendingTrees) != 0 {
		t.Fatalf("the spend answered %s: tree %+v, %d adopted, %d record(s)\n%s", op, e.St.Tree, len(e.St.Trees), len(e.St.PendingTrees), said)
	}
	if !strings.Contains(said, "nothing is left to do for it") || strings.Contains(said, "record is kept") {
		t.Fatalf("what the spend said:\n%s", said)
	}
	// The same tree answers the next spend, and no other is minted.
	if next := r.use(e); next != e.St.Tree.Txid+".1" || len(e.St.Trees) != 1 {
		t.Fatalf("the next spend answered %s with %d tree(s) adopted", next, len(e.St.Trees))
	}
}

// A publish that fails once the library adopted a recovered tree is not a
// recovery that failed: the tree is adopted, its record is dropped, its fee
// coin is out of the pool, and nothing says the record is kept.
func TestARecoveryWhoseTreeIsAdoptedAndNotPublished(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	// The leg takes the first tree and the run never hears so: it puts the
	// coin back in the pool and keeps the record, and the tree lands.
	a := r.open(0, nil)
	a.payer.Settler = lost{a.payer.Settler}
	if _, _, err := a.spend(context.Background()); err == nil {
		t.Fatal("a spend whose settlement leg lost its answer")
	}
	p := r.landed(a)
	r.said()

	b := r.build(0, nil)
	inPool := func() (coin, change bool) {
		for _, o := range b.Pool.Outputs() {
			coin = coin || o.Outpoint() == p.Coin.Outpoint()
			change = change || o.TxID == p.Tree.Txid
		}
		return coin, change
	}
	if coin, _ := inPool(); !coin || b.St.Tree != nil {
		t.Fatalf("the home the run left: fee coin in the pool %v, tree %+v", coin, b.St.Tree)
	}
	unpublishable(b)
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("start: %v\n%s", err, r.said())
	}
	said := r.said()
	coin, change := inPool()
	if b.St.Tree == nil || b.St.Tree.Txid != p.Tree.Txid || len(b.St.Trees) != 1 || len(b.St.PendingTrees) != 0 || coin || !change {
		t.Fatalf("after the recovery: tree %+v, %d record(s), fee coin in the pool %v, change in the pool %v\n%s", b.St.Tree, len(b.St.PendingTrees), coin, change, said)
	}
	for _, line := range []string{"its fee coin " + p.Coin.Outpoint() + " is spent and is taken out of the pool", "is recovered", "nothing is left to do for it"} {
		if !strings.Contains(said, line) {
			t.Fatalf("the recovery did not say %q:\n%s", line, said)
		}
	}
	if strings.Contains(said, "record is kept") || strings.Contains(said, "could not be settled") {
		t.Fatalf("the recovery of an adopted tree speaks of a record:\n%s", said)
	}
	// The home on disk agrees, and the tree is spent from.
	if c := r.open(0, nil); c.St.Tree == nil || c.St.Tree.Txid != p.Tree.Txid || len(c.St.PendingTrees) != 0 || r.use(c) != p.Tree.Txid+".0" {
		t.Fatalf("the next command: %+v\n%s", c.St.Tree, r.said())
	}
}

// lost is a settlement leg that takes a transaction and reports a failure.
type lost struct{ publish.Settler }

func (l lost) Submit(ctx context.Context, tx *transaction.Transaction) error {
	if err := l.Settler.Submit(ctx, tx); err != nil {
		return err
	}
	return errors.New("the answer was lost")
}

// chainOnly is a chain view that is nothing but nodeapi.Chain: a
// WhatsOnChain view, or anything else an application holds, with no node's
// asset API behind it to be found by a type assertion.
type chainOnly struct{ nodeapi.Chain }

// A publisher with no node: arcade settles, and a bare chain view answers
// transactions, proofs and spends. It mints its trees, minting the next
// ahead, and spends past the first into the second, as an application that
// imports this package without a node does.
func TestAPublisherNeedsNoNode(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	view := chainOnly{r.asset}
	settler, arcade, err := publish.ParseSettler("arcade:"+r.asset.Base+"/arcade", publish.SettleOptions{Spends: view})
	if err != nil {
		t.Fatal(err)
	}
	w, err := bwallet.Open(r.dir, Profile)
	if err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(r.dir, w.Signer().IdentityHex())
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(st, w.Signer(), w.Pool, Legs{Settler: settler, Arcade: arcade, Chain: view, Headers: r.headers,
		Hosts: &unicast.Set{Hosts: []string{"http://host.invalid"}}},
		Options{TreeCount: 4, TreeSats: limits.DefaultTreeSats, Ahead: 2, Fees: mint.NetworkFees,
			Poll: 5 * time.Millisecond, Wait: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	e.Verbose = true
	e.Note = func(format string, args ...any) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.notes = append(r.notes, fmt.Sprintf(format, args...))
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("start: %v\n%s", err, r.said())
	}
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		op := r.use(e)
		if seen[op] {
			t.Fatalf("funding output %s is spent twice\n%s", op, r.said())
		}
		seen[op] = true
	}
	if len(e.St.Trees) != 2 {
		t.Fatalf("%d tree(s) adopted after six spends of four-output trees\n%s", len(e.St.Trees), r.said())
	}
	for _, tr := range e.St.Trees {
		if r.chain.Tx(tr.Txid) == nil {
			t.Fatalf("funding tree %s never reached the chain", tr.Txid)
		}
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
