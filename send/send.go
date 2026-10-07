// Package send is a bbox publisher: it seals envelopes, acknowledges them
// with receipts and retracts them with sweeps (docs/spec.md sections 3 to
// 6 and 9), from one identity's home.
//
// It is built on bcommon's producer package. A Payer pays for funding trees
// from the home's coin pool, and Trees mints them, with Ahead, so the next
// tree is minted before the current one runs out. Payer.Async is on: a tree
// is handed to the settlement leg and recorded at once, so a command never
// waits for a block to mint ahead, and a carrier, which spends only a mined
// tree's output and carries the tree with its proof (spec section 6.3),
// waits for the proof only when it needs one. A tree is recorded with its
// fee coin before it reaches the leg (Trees.Prepare), and a command that
// finds the record of a tree never adopted settles it before it spends
// (RecoverTrees). A funding tree is not published to an office: every
// carrier carries its own, so Trees publishes through a facade that sends
// nothing.
//
// Every object is persisted in the home's state, with the funding output it
// spends marked used, before it is published: a run that stops part way
// publishes those same bytes again (Resume), and never a second carrier on
// the same funding output, which a host would count as superseded.
package send

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"

	"github.com/lightwebinc/bbox/boxrec"
	"github.com/lightwebinc/bbox/internal/limits"
	"github.com/lightwebinc/bbox/internal/state"
	"github.com/lightwebinc/bbox/internal/unicast"
	"github.com/lightwebinc/bbox/reader"
)

// Params are the carrier parameters of every bbox carrier.
var Params = boxrec.Params()

// Profile is the embedded wallet's profile: the fund key under [1, "bbox
// message"] key id fund, and the registered basket.
var Profile = bwallet.Profile{
	FundProtocol:   boxrec.Protocol,
	FundKeyID:      boxrec.KeyFund,
	FundBasket:     "bbox fund",
	Version:        "bbox-1",
	LegacyPoolFile: "bbox-pool.json",
}

// Options are a publisher's choices. None of them is frozen.
type Options struct {
	// TreeCount and TreeSats size the funding trees; Ahead is how few
	// outputs a tree has left when the next is minted ahead.
	TreeCount int
	TreeSats  uint64
	Ahead     uint32
	// Fees is the fee policy of trees, sweeps and payments.
	Fees mint.Fees
	// ObjectBound is the plane's object bound, in bytes.
	ObjectBound int
	// Poll paces proof waits and Wait bounds one.
	Poll time.Duration
	Wait time.Duration
	// PlaneNeed is, in mode plane, how many of the hosts named must answer
	// a carrier by lookup for it to count as published; zero is all of
	// them. A sweep always needs every host.
	PlaneNeed int
}

// Legs are the endpoints a publisher talks to. Objects go to exactly one of
// Facade (mode plane: one submit, which the plane delivers to every
// subscribed host) and Hosts (mode unicast: one submit to each host). In
// mode plane, Direct are the other hosts a sweep is also submitted to, since
// a host off the plane learns of a sweep only by receiving it. Reader
// confirms, by lookup, a host that admitted nothing, and on the plane that
// the hosts named hold what the facade took. Headers, when set, are the
// publisher's own block headers: a kept proof a reorganization left stale
// is replaced by the node's current one before it is used.
type Legs struct {
	Settler publish.Settler
	Asset   *nodeapi.Asset
	Arcade  *publish.Arcade
	Facade  *publish.Facade
	Hosts   *unicast.Set
	Direct  *unicast.Set
	Reader  *reader.Client
	Headers chaintracker.ChainTracker
}

// Engine is one publisher run over one home.
type Engine struct {
	St     *state.State
	Signer *bwallet.Signer
	Pool   *bwallet.Pool
	Legs   Legs
	Opts   Options
	// Now is the clock records are stamped with.
	Now func() time.Time
	// Note receives progress lines.
	Note    func(format string, args ...any)
	Verbose bool

	payer *producer.Payer
	kept  *producer.Kept
	trees *producer.Trees
	// warned is set once a warning about object size was printed, and
	// unconfirmed once a plane publish no host could confirm was noted.
	warned      bool
	unconfirmed bool
}

// New builds an engine over a home's state.
func New(st *state.State, signer *bwallet.Signer, pool *bwallet.Pool, legs Legs, o Options) (*Engine, error) {
	if st.Identity != signer.IdentityHex() {
		return nil, fmt.Errorf("the state belongs to identity %s, not this home's %s", st.Identity, signer.IdentityHex())
	}
	if legs.Settler == nil || legs.Asset == nil {
		return nil, errors.New("a publisher needs a settlement leg and a node (config keys settle and asset)")
	}
	if (legs.Facade == nil) == (legs.Hosts == nil) {
		return nil, errors.New("a publisher needs either a facade (mode plane) or hosts (mode unicast)")
	}
	e := &Engine{St: st, Signer: signer, Pool: pool, Legs: legs, Opts: o, Now: time.Now}
	e.kept = &producer.Kept{Load: e.load}
	e.payer = e.NewPayer()
	// Trees are settled without waiting for a block: a tree minted ahead
	// comes back as soon as the settlement leg takes it, so it is recorded
	// before the command ends, and spend waits for a proof only when a
	// carrier needs it.
	e.payer.Async = true
	lock := func(ctx context.Context) (*script.Script, error) {
		return carrier.FundingLock(ctx, signer, signer.Originator, Params)
	}
	e.trees = &producer.Trees{Payer: e.payer, State: treeState{e}, Identity: signer.IdentityHex(),
		Count: o.TreeCount, Sats: o.TreeSats, Funder: "pool", Lock: lock, Change: signer.FundScript,
		Facade: unicast.Kept(), Topic: "tm_bbox_kept", Ahead: o.Ahead, Prepare: e.prepareTree}
	return e, nil
}

// NewPayer is a Payer over the engine's pool and legs.
func (e *Engine) NewPayer() *producer.Payer {
	p := &producer.Payer{
		Pool: e.Pool, Keys: map[string]*bwallet.Signer{e.Signer.IdentityHex(): e.Signer},
		Kept: e.kept, Settler: e.Legs.Settler, Asset: e.Legs.Asset, Fees: e.Opts.Fees,
		Timeout: e.Opts.Wait, Poll: e.Opts.Poll, Note: e.libraryNote,
	}
	if e.payer != nil {
		p.Tip = e.payer.Tip
	}
	return p
}

// libraryNote passes the library's lines on, except the one that reports a
// funding tree's publication: a bbox tree is never published.
func (e *Engine) libraryNote(format string, args ...any) {
	if strings.HasPrefix(format, "funding tree published") {
		return
	}
	e.note(format, args...)
}

func (e *Engine) note(format string, args ...any) {
	if e.Note != nil {
		e.Note(format, args...)
	}
}

// treeState is the home's funding trees as a producer.TreeState.
type treeState struct{ e *Engine }

func (t treeState) Current() *funding.Tree { return t.e.St.Tree }

func (t treeState) Adopt(tr funding.Tree) error {
	st := t.e.St
	st.Tree = &tr
	st.Trees = append(st.Trees, tr)
	st.Ahead = slices.DeleteFunc(st.Ahead, func(a funding.Tree) bool { return a.Txid == tr.Txid })
	// The tree is recorded as adopted: its record as a tree signed and not
	// yet adopted goes in the same save.
	st.DropPendingTree(tr.Txid)
	return st.Save()
}

// load rebuilds a funding tree the home keeps.
func (e *Engine) load(txid string) (*transaction.Transaction, error) {
	st := e.St
	var trees []funding.Tree
	if st.Tree != nil {
		trees = append(trees, *st.Tree)
	}
	for _, t := range append(append(trees, st.Trees...), st.Ahead...) {
		if t.Txid == txid {
			return funding.Rebuild(t.RawHex, t.BumpHex, t.BeefHex)
		}
	}
	return nil, fmt.Errorf("transaction %s is not a funding tree this home keeps", txid)
}

// FeeError words the library's refusals to find a fee input.
func FeeError(err error) error {
	var nc *producer.NoCoinError
	var nk *producer.NoKeyError
	switch {
	case errors.As(err, &nc):
		switch {
		case nc.Held > 0 && nc.Minting > 0:
			return fmt.Errorf("fee input: %w: the other coins are change from %d transaction(s) whose proofs have not arrived, and %d funding tree(s) minted ahead hold a coin and have not settled; they become spendable once mined, so run the command again", nc.Err, nc.Held, nc.Minting)
		case nc.Held > 0:
			return fmt.Errorf("fee input: %w: the other coins are change from %d transaction(s) whose proofs have not arrived; they become spendable once mined", nc.Err, nc.Held)
		case nc.Minting > 0:
			return fmt.Errorf("fee input: %w: %d funding tree(s) minted ahead hold a coin and have not settled; the change returns once one does, so run the command again", nc.Err, nc.Minting)
		}
		return fmt.Errorf("fee input: %w (run `bbox fund`)", nc.Err)
	case errors.As(err, &nk):
		return fmt.Errorf("wallet output %s is locked to a key this home does not hold", nk.Outpoint)
	}
	return err
}

// Start reads the chain tip for coinbase maturity, collects the proofs of
// change the pool holds back, and finishes whatever a previous run
// persisted and did not finish: a sweep in flight, and every object not
// confirmed published. Each is tried whatever became of the one before it:
// a sweep that cannot finish does not keep an envelope from the hosts. The
// first failure is returned; the others are noted.
func (e *Engine) Start(ctx context.Context) error {
	h, err := e.Legs.Asset.BestHeader(ctx)
	if err != nil {
		return fmt.Errorf("node tip: %w", err)
	}
	e.payer.Tip = h.Height
	CollectChange(ctx, e.Pool, e.Legs.Asset)
	// What a run that stopped took from the pool and never recorded goes
	// back; from here every save records the pool before a coin is taken.
	Journal(e.St, e.Pool, Reconcile(ctx, e.St, e.Pool, e.Legs.Asset, e.note))
	if err := e.St.Save(); err != nil {
		return err
	}
	// A tree a run signed and stopped short of adopting is settled before
	// anything is spent.
	if err := e.RecoverTrees(ctx); err != nil {
		return err
	}
	var first error
	var refusedErr *SweepRefusedError
	if err := e.FinishSweep(ctx); errors.As(err, &refusedErr) {
		e.note("%v", err)
	} else if err != nil {
		if ctx.Err() != nil {
			return err
		}
		first = err
	}
	if err := e.Resume(ctx); err != nil {
		if first == nil {
			first = err
		} else {
			e.note("%v", err)
		}
	}
	return first
}

// CollectChange gives every unproven coin in the pool its proof, once its
// transaction has mined: change from a payment the payee broadcast, or
// from anything else published before it mined.
func CollectChange(ctx context.Context, pool *bwallet.Pool, asset *nodeapi.Asset) int {
	n := 0
	for _, id := range pool.UnprovenTxids() {
		mp, h, err := asset.Proof(ctx, id)
		if err != nil {
			continue
		}
		if k, err := pool.Prove(id, mp.Hex(), h); err == nil {
			n += k
		}
	}
	return n
}

// Resume publishes every object a previous run persisted: the same bytes,
// so a host that already holds one answers a duplicate. Every object is
// tried; one that cannot be published stays in the outbox, and the first
// failure is returned.
func (e *Engine) Resume(ctx context.Context) error {
	var first error
	for _, p := range slices.Clone(e.St.Outbox) {
		e.note("%s %s: publishing what a previous run persisted", p.Kind, p.Txid)
		if err := e.publish(ctx, p); err != nil {
			if ctx.Err() != nil {
				return err
			}
			if first == nil {
				first = err
			} else {
				e.note("%v", err)
			}
			continue
		}
		e.St.Done(p.Txid)
		if err := e.St.Save(); err != nil {
			return err
		}
	}
	return first
}

// Close collects a tree still being minted ahead, so its change reaches the
// pool, and records every tree the library holds and never adopted in the
// state's Ahead list, so a sweep finds it.
func (e *Engine) Close(ctx context.Context) error {
	_ = e.trees.Wait(ctx)
	for _, p := range e.trees.Held() {
		e.St.Ahead = producer.Index(e.St.Ahead, &p)
	}
	// The run ended: nothing is being taken.
	Unjournal(e.St)
	return e.St.Save()
}

// spend returns the funding tree the next carrier spends and the output,
// the tree's proof cut to the minimal one a host admits.
func (e *Engine) spend(ctx context.Context) (*transaction.Transaction, uint32, error) {
	if err := e.promoteAhead(); err != nil {
		return nil, 0, err
	}
	// A tree an earlier run minted ahead is the successor already: the
	// library knows only a tree minted ahead in this run, and would mint
	// another on every run that spends near the end of the tree, each
	// taking a coin (and, from a pool of one coin, leaving nothing proven
	// for the next fee until it mines).
	if e.hasAhead() {
		e.trees.Ahead = 0
	} else {
		e.trees.Ahead = e.Opts.Ahead
	}
	tree, vout, err := e.trees.Spend(ctx, 1)
	if e.unpublished(err) {
		// The tree is adopted and is the current one, which Spend answers.
		tree, vout, err = e.trees.Spend(ctx, 1)
	}
	if err != nil {
		return nil, 0, FeeError(fmt.Errorf("funding tree: %w", err))
	}
	if tree.MerklePath == nil {
		if err := e.prove(ctx, tree); err != nil {
			return nil, 0, err
		}
	} else if err := e.reproveTree(ctx, tree); err != nil {
		return nil, 0, err
	}
	mp, err := Minimal(tree.MerklePath, tree.TxID())
	if err != nil {
		return nil, 0, fmt.Errorf("funding tree %s: %w", tree.TxID(), err)
	}
	tree.MerklePath = mp
	return tree, vout, nil
}

// hasAhead reports whether an earlier run recorded a tree minted ahead for
// this identity with outputs left.
func (e *Engine) hasAhead() bool {
	for _, t := range e.St.Ahead {
		if t.IdentityKeyHex == e.Signer.IdentityHex() && t.Remaining() > 0 && (e.St.Tree == nil || t.Txid != e.St.Tree.Txid) {
			return true
		}
	}
	return false
}

// promoteAhead makes a tree minted ahead by an earlier run the current one
// when the current tree is used up: the library keeps a tree minted ahead
// in memory only, and a run that ended recorded it in Ahead.
func (e *Engine) promoteAhead() error {
	st := e.St
	if st.Tree != nil && st.Tree.Remaining() > 0 {
		return nil
	}
	var held []funding.Tree
	if e.trees != nil {
		held = e.trees.Held()
	}
	for _, t := range st.Ahead {
		// A tree the library holds, minted ahead or recovered in this run,
		// is switched to by the library, which then lets go of it. It holds
		// any number, and each is left to it: one adopted here would be
		// adopted again, from its first output, when the library reached it.
		if slices.ContainsFunc(held, func(h funding.Tree) bool { return h.Txid == t.Txid }) {
			continue
		}
		if t.IdentityKeyHex == e.Signer.IdentityHex() && t.Remaining() > 0 {
			// Adopt saves the home: a tree that is the current one only in
			// memory would have its outputs spent with nothing recording
			// which.
			if err := (treeState{e}).Adopt(t); err != nil {
				return fmt.Errorf("switching to funding tree %s: %w", short(t.Txid), err)
			}
			e.note("switching to funding tree %s, minted ahead by an earlier run", t.Txid)
			return nil
		}
	}
	return nil
}

// reproveTree holds a funding tree's kept proof to the publisher's headers
// and, when a reorganization left it stale, replaces it with the node's
// current one, in the home too: a carrier carrying a proof of an orphaned
// block is refused by every host.
func (e *Engine) reproveTree(ctx context.Context, tree *transaction.Transaction) error {
	id := tree.TxID().String()
	changed, err := e.current(ctx, tree)
	if err != nil {
		return fmt.Errorf("funding tree %s: %w", short(id), err)
	}
	if !changed {
		return nil
	}
	mp := tree.MerklePath
	e.note("funding tree %s: its kept proof named a block that is no longer in the header source's chain; the node's current proof, at height %d, replaces it", short(id), mp.BlockHeight)
	e.kept.Prove(id, mp)
	fix := func(t *funding.Tree) {
		if t != nil && t.Txid == id {
			t.BumpHex, t.Height, t.BeefHex = mp.Hex(), mp.BlockHeight, ""
		}
	}
	fix(e.St.Tree)
	for i := range e.St.Trees {
		fix(&e.St.Trees[i])
	}
	for i := range e.St.Ahead {
		fix(&e.St.Ahead[i])
	}
	return e.St.Save()
}

// prove waits for a funding tree's proof: a carrier spends only a mined
// tree and carries it with its proof (spec section 6.3). Trees are settled
// without waiting, so a tree minted ahead never holds a command up; the
// first tree, and one needed before its block, is waited for here.
func (e *Engine) prove(ctx context.Context, tree *transaction.Transaction) error {
	id := tree.TxID().String()
	mp, height, err := producer.Proofs{Arcade: e.Legs.Arcade, Asset: e.Legs.Asset}.Of(ctx, id)
	if err != nil {
		e.note("funding tree %s: waiting for it to mine", id)
		if mp, height, err = e.NewPayer().Await(ctx, "funding tree", tree); err != nil {
			return fmt.Errorf("funding tree %s: %w (it is recorded; the next command waits again)", id, err)
		}
	}
	tree.MerklePath = mp
	e.kept.Prove(id, mp)
	CollectChange(ctx, e.Pool, e.Legs.Asset)
	for _, t := range append([]*funding.Tree{e.St.Tree}, ptrs(e.St.Trees)...) {
		if t != nil && t.Txid == id {
			t.BumpHex, t.Height, t.BeefHex = mp.Hex(), height, ""
		}
	}
	return e.St.Save()
}

func ptrs(ts []funding.Tree) []*funding.Tree {
	out := make([]*funding.Tree, len(ts))
	for i := range ts {
		out[i] = &ts[i]
	}
	return out
}

// carrierFor mints the carrier of payload on the next funding output, holds
// it to the host's rules for office, and returns it with its Atomic BEEF.
func (e *Engine) carrierFor(ctx context.Context, office string, payload []byte) (*boxrec.Admission, []byte, *transaction.Transaction, uint32, error) {
	tree, vout, err := e.spend(ctx)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	k, err := carrier.Mint(ctx, e.Signer, e.Signer.Originator, Params, payload, tree, vout)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("carrier: %w", err)
	}
	beef, err := k.AtomicBEEF(false)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	// The publisher's own copy of the host's rules: a carrier a host would
	// refuse is never persisted or published.
	a, err := boxrec.Admit(ctx, beef, nil, boxrec.Host{Office: office})
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("the carrier breaks the host's rules (%s): %w", boxrec.Reason(err), err)
	}
	if len(beef) > e.Opts.ObjectBound {
		return nil, nil, nil, 0, fmt.Errorf("the carrier's BEEF is %d bytes, over the object bound of %d (shrink the content or -tree-count)", len(beef), e.Opts.ObjectBound)
	}
	if e.Legs.Facade != nil && !e.warned && len(beef) > limits.PlaneContentWarn+3*limits.FragmentPayload {
		e.warned = true
		e.note("warning: the carrier's BEEF is %d bytes, about %d packets on the plane (docs/limits.md)", len(beef), (len(beef)+limits.FragmentPayload-1)/limits.FragmentPayload)
	}
	return a, beef, tree, vout, nil
}

// markSpent records that vout of the current tree is used.
func (e *Engine) markSpent(tree *transaction.Transaction, vout uint32) {
	st := e.St
	if st.Tree != nil && st.Tree.Txid == tree.TxID().String() {
		st.Tree.Next = vout + 1
		st.Trees = producer.Index(st.Trees, st.Tree)
	}
}

// Letter is one envelope before it is sealed.
type Letter struct {
	Office  string
	To      []byte
	Box     string
	Created uint64
	Expires uint64
	// Plaintext is the canonical plaintext (spec section 4.5).
	Plaintext []byte
	// Pay is the payment it carries, already in Plaintext.
	Pay *Pay
}

// Envelope seals l, mints its carrier, persists it and publishes it. The
// record is written to the state before anything leaves the machine.
func (e *Engine) Envelope(ctx context.Context, l Letter) (sent *state.Sent, err error) {
	defer func() {
		if sent == nil {
			l.Pay.Abort()
		}
	}()
	from := e.Signer.Identity.Compressed()
	content, err := boxrec.SealEnvelope(ctx, e.Signer, e.Signer.Originator, from, l.To, l.Plaintext, l.Created)
	if err != nil {
		return nil, err
	}
	if len(content) > boxrec.MaxContent {
		return nil, fmt.Errorf("the sealed envelope is %d bytes, over the content bound of %d (reference larger content with -ref; docs/limits.md)", len(content), boxrec.MaxContent)
	}
	env := &boxrec.Envelope{Office: l.Office, To: l.To, Box: l.Box, From: from, Created: l.Created, Expires: l.Expires, Content: content}
	record, err := env.Encode()
	if err != nil {
		return nil, err
	}
	a, beef, tree, vout, err := e.carrierFor(ctx, l.Office, record)
	if err != nil {
		return nil, err
	}
	txid := chainhash.Hash(a.Txid).String()
	s := state.Sent{Txid: txid, Office: l.Office, To: hex.EncodeToString(l.To), Box: l.Box, Created: l.Created,
		Expires: l.Expires, Tree: tree.TxID().String(), Vout: vout}
	if l.Pay != nil {
		s.Paid, s.PaymentTxid = l.Pay.Sats, l.Pay.Tx.TxID().String()
	}
	p := state.Pending{Kind: state.KindEnvelope, Office: l.Office, Txid: txid, Beef: hex.EncodeToString(beef),
		To: s.To, Box: l.Box, Created: l.Created}
	e.markSpent(tree, vout)
	e.St.Sent = append(e.St.Sent, s)
	e.St.Outbox = append(e.St.Outbox, p)
	e.St.LastSentMs = e.Now().UnixMilli()
	if err := e.St.Save(); err != nil {
		return nil, err
	}
	l.Pay.keep()
	if err := e.publish(ctx, p); err != nil {
		return &s, err
	}
	e.St.Done(txid)
	return &s, e.St.Save()
}

// Receipt acknowledges envelopes in office (display txids, at most 64),
// mints the receipt carrier, persists it and publishes it.
func (e *Engine) Receipt(ctx context.Context, office string, txids []string) (*state.Receipt, error) {
	if len(txids) == 0 || len(txids) > boxrec.MaxAcks {
		return nil, fmt.Errorf("a receipt acknowledges 1 to %d envelopes, not %d", boxrec.MaxAcks, len(txids))
	}
	acks := make([][32]byte, 0, len(txids))
	seen := map[[32]byte]bool{}
	for _, id := range txids {
		h, err := reader.Hash(id)
		if err != nil {
			return nil, err
		}
		if !seen[h] {
			seen[h] = true
			acks = append(acks, h)
		}
	}
	sort.Slice(acks, func(i, j int) bool { return string(acks[i][:]) < string(acks[j][:]) })
	r := &boxrec.Receipt{Office: office, By: e.Signer.Identity.Compressed(), Acks: acks, Created: uint64(e.Now().Unix())} //nolint:gosec // a clock after 1970
	record, err := r.Encode()
	if err != nil {
		return nil, err
	}
	a, beef, tree, vout, err := e.carrierFor(ctx, office, record)
	if err != nil {
		return nil, err
	}
	txid := chainhash.Hash(a.Txid).String()
	rc := state.Receipt{Txid: txid, Office: office, Acks: append([]string(nil), txids...), Tree: tree.TxID().String(), Vout: vout}
	p := state.Pending{Kind: state.KindReceipt, Office: office, Txid: txid, Beef: hex.EncodeToString(beef),
		By: e.Signer.IdentityHex(), AckTxid: txids[0]}
	e.markSpent(tree, vout)
	e.St.Receipts = append(e.St.Receipts, rc)
	for _, id := range txids {
		if r := e.St.ReceivedByTxid(id); r != nil {
			r.Acked = txid
		}
	}
	e.St.Outbox = append(e.St.Outbox, p)
	if err := e.St.Save(); err != nil {
		return nil, err
	}
	if err := e.publish(ctx, p); err != nil {
		return &rc, err
	}
	e.St.Done(txid)
	return &rc, e.St.Save()
}

// Payment builds a BRC-29 payment of sats to the identity toHex from the
// pool, unbroadcast (spec section 10): the sender never broadcasts it; the
// recipient internalizes it, which broadcasts it. It returns the plaintext
// member and the transaction. The fee coin is spent, and the change is
// held in the pool until the payment mines.
func (e *Engine) Payment(ctx context.Context, toHex string, sats uint64) (*Pay, error) {
	prefix, suffix := token(), token()
	dest, err := e.Signer.PaymentDestination(ctx, toHex, prefix, suffix)
	if err != nil {
		return nil, err
	}
	payer := e.NewPayer()
	fee, err := payer.TakeAtLeast(ctx, sats+e.Opts.Fees.Floor)
	if err != nil {
		return nil, FeeError(fmt.Errorf("payment: %w", err))
	}
	change, err := e.Signer.FundScript()
	if err != nil {
		payer.GiveBack()
		return nil, err
	}
	tx, err := mint.Payment(ctx, dest, sats, fee, change, e.Opts.Fees)
	if err != nil {
		payer.GiveBack()
		return nil, err
	}
	beef, err := funding.BEEF(tx)
	if err != nil {
		payer.GiveBack()
		return nil, err
	}
	out := &boxrec.Object{}
	out.Set("outputIndex", boxrec.Int(0))
	out.Set("derivationSuffix", suffix)
	out.Set("satoshis", boxrec.Int(sats)) //nolint:gosec // bounded by the pool
	m := &boxrec.Object{}
	m.Set("beef", base64.StdEncoding.EncodeToString(beef))
	m.Set("derivationPrefix", prefix)
	m.Set("outputs", []any{out})
	return &Pay{Member: m, Tx: tx, Sats: sats, payer: payer}, nil
}

// Pay is a payment built for an envelope and not yet sent: its fee coin is
// reserved. Envelope keeps it once the envelope is persisted (the coin is
// spent and the change held until the payment mines), and gives the coin
// back when the envelope is not.
type Pay struct {
	Member *boxrec.Object
	Tx     *transaction.Transaction
	Sats   uint64
	payer  *producer.Payer
}

// Abort gives the payment's fee coin back to the pool.
func (p *Pay) Abort() {
	if p != nil && p.payer != nil {
		p.payer.GiveBack()
		p.payer = nil
	}
}

func (p *Pay) keep() {
	if p != nil && p.payer != nil {
		p.payer.Change(p.Tx, 0, nil)
		p.payer = nil
	}
}

// token is a BRC-29 derivation token: 16 random bytes in standard padded
// base64, the one spelling a wallet re-encodes remittance bytes to.
func token() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(b[:])
}

// Topic is an office's topic.
func Topic(office string) string {
	t, err := boxrec.Topic(office)
	if err != nil {
		return ""
	}
	return t
}

// Retries are the waits between the tries of a submission to the facade.
var Retries = limits.Retries

// publish submits a persisted object by the configured mode.
func (e *Engine) publish(ctx context.Context, p state.Pending) error {
	beef, err := hex.DecodeString(p.Beef)
	if err != nil {
		return fmt.Errorf("%s %s: persisted BEEF: %w", p.Kind, p.Txid, err)
	}
	offices := p.Offices
	if len(offices) == 0 {
		offices = []string{p.Office}
	}
	for _, office := range offices {
		topic := Topic(office)
		what := fmt.Sprintf("%s %s", p.Kind, short(p.Txid))
		confirm := e.confirm(p, office)
		if e.Legs.Hosts != nil {
			need := 0
			if p.Kind == state.KindSweep {
				need = len(e.Legs.Hosts.Hosts)
			}
			o := e.Legs.Hosts.Send(ctx, what, topic, beef, need, confirm)
			if err := o.Err(); err != nil {
				return fmt.Errorf("%s: publish: %w; it is persisted and the next command publishes it", what, err)
			}
			continue
		}
		if err := e.submitFacade(ctx, what, topic, beef); err != nil {
			return err
		}
		if p.Kind == state.KindSweep && e.Legs.Direct != nil {
			o := e.Legs.Direct.Send(ctx, what, topic, beef, len(e.Legs.Direct.Hosts), confirm)
			if err := o.Err(); err != nil {
				return fmt.Errorf("%s: a sweep reaches every host named, off the plane too: %w; it is persisted and the next command publishes it", what, err)
			}
		}
		// The facade's answer is not a host's: it says "nothing admitted"
		// for a duplicate and for a refusal alike, and the plane can lose
		// an object on the way to any host. An object counts as published
		// only once a lookup at the hosts named answers it: a sweep at
		// every one, a carrier at the quorum. Until then it stays in the
		// outbox. A publisher that names no host has nothing to ask.
		rd := e.Legs.Reader
		if rd == nil || confirm == nil {
			if !e.unconfirmed {
				e.unconfirmed = true
				e.note("%s: no host is named (hosts) or no header source is set, so nothing confirms that a host holds what the facade took", what)
			}
			continue
		}
		held, missing := e.confirmOnPlane(ctx, what, topic, beef, confirm)
		need := e.Opts.PlaneNeed
		if p.Kind == state.KindSweep || need <= 0 || need > len(rd.Hosts) {
			need = len(rd.Hosts)
		}
		if held < need {
			return fmt.Errorf("%s: %d of %d host(s) named answer it and %d must (%s lack it or could not be asked); it is persisted and the next command publishes it", what, held, len(rd.Hosts), need, strings.Join(missing, ", "))
		}
	}
	return nil
}

// confirmOnPlane confirms an object at every host the publisher names,
// once the facade took it, and offers it again, directly, to a host the
// plane did not deliver it to (spec section 9). An object is answered by a
// free class as soon as a host holds it, so a host that still lacks it
// after PlaneWait was missed. A direct copy that races the plane's is a
// duplicate and harmless. It returns how many hosts answer the object, and
// those that do not.
func (e *Engine) confirmOnPlane(ctx context.Context, what, topic string, beef []byte, confirm unicast.Confirm) (int, []string) {
	rd := e.Legs.Reader
	n := 0
	var missing []string
	for _, host := range rd.Hosts {
		held, asked := false, true
		for i := 0; ; i++ {
			ok, err := confirm(ctx, host)
			if err != nil {
				e.note("%s: host %s could not be asked whether it holds it: %v", what, host, err)
				asked = false
				break
			}
			if ok {
				held = true
				break
			}
			if i == len(limits.PlaneWait) {
				break
			}
			select {
			case <-ctx.Done():
				return n, append(missing, host)
			case <-time.After(limits.PlaneWait[i]):
			}
		}
		if held {
			n++
			continue
		}
		if !asked {
			missing = append(missing, host)
			continue
		}
		set := &unicast.Set{Hosts: []string{host}, Need: 1, Retries: Retries, HTTP: rd.HTTP, Note: e.Note}
		if err := set.Send(ctx, what, topic, beef, 1, confirm).Err(); err != nil {
			e.note("%s: host %s lacks it after the plane's delivery and did not take it directly: %v", what, host, err)
			missing = append(missing, host)
			continue
		}
		e.note("%s: host %s lacked it after the plane's delivery; offered directly and confirmed", what, host)
		n++
	}
	return n, missing
}

func (e *Engine) submitFacade(ctx context.Context, what, topic string, beef []byte) error {
	var err error
	for i := 0; ; i++ {
		var res publish.Result
		res, err = e.Legs.Facade.Submit(ctx, topic, beef)
		if err == nil {
			if res.Duplicate {
				e.note("%s: the facade admitted nothing: it holds this already, or refused it", what)
			}
			return nil
		}
		if i == len(Retries) || ctx.Err() != nil {
			break
		}
		e.note("%s: publish failed (%v); retrying", what, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(Retries[i]):
		}
	}
	return fmt.Errorf("%s: publish: %w; it is persisted and the next command publishes it", what, err)
}

// confirm is the lookup that shows a host took p (spec section 9): the
// box class for an envelope, or the receipt class naming it, since an
// envelope its recipient acknowledged is no longer open; the receipt class
// for a receipt; the sweep class for a sweep.
func (e *Engine) confirm(p state.Pending, office string) unicast.Confirm {
	if e.Legs.Reader == nil {
		return nil
	}
	var q reader.Query
	switch p.Kind {
	case state.KindEnvelope:
		q = reader.Query{"office": office, "to": p.To, "box": p.Box, "after": fmt.Sprintf("%d:%s", p.Created, strings.Repeat("0", 64))}
	case state.KindReceipt:
		q = reader.Query{"office": office, "by": p.By, "receiptFor": p.AckTxid}
	case state.KindSweep:
		q = reader.Query{"office": office, "spent": p.Outpoint}
	default:
		return nil
	}
	return func(ctx context.Context, host string) (bool, error) {
		ok, err := e.Legs.Reader.Held(ctx, host, q, p.Txid)
		if ok || err != nil || p.Kind != state.KindEnvelope {
			return ok, err
		}
		to, err := hex.DecodeString(p.To)
		if err != nil {
			return false, fmt.Errorf("persisted envelope %s: to: %w", p.Txid, err)
		}
		return e.Legs.Reader.Acknowledged(ctx, host, office, to, p.Txid)
	}
}

func short(txid string) string {
	if len(txid) > 12 {
		return txid[:12]
	}
	return txid
}

// alreadyKnown reports a node's answer for a transaction it already holds.
func alreadyKnown(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already")
}
