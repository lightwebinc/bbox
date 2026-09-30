// Package send is a bbox publisher: it seals envelopes, acknowledges them
// with receipts and retracts them with sweeps (docs/spec.md sections 3 to
// 6 and 9), from one identity's home.
//
// It is built on bcommon's producer package. A Payer pays for funding trees
// from the home's coin pool, and Trees mints them, with Ahead, so the next
// tree is minted and mined before the current one runs out. Payer.Async is
// off: a tree is settled and waited for, because a carrier spends only a
// mined tree's output and carries the tree with its proof (spec section
// 6.3). A funding tree is not published to an office: every carrier carries
// its own, so Trees publishes through a facade that sends nothing.
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
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"

	"github.com/lightwebinc/bbox/internal/boxrec"
	"github.com/lightwebinc/bbox/internal/limits"
	"github.com/lightwebinc/bbox/internal/reader"
	"github.com/lightwebinc/bbox/internal/state"
	"github.com/lightwebinc/bbox/internal/unicast"
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
}

// Legs are the endpoints a publisher talks to. Objects go to exactly one of
// Facade (mode plane: one submit, which the plane delivers to every
// subscribed host) and Hosts (mode unicast: one submit to each host). In
// mode plane, Direct are the other hosts a sweep is also submitted to, since
// a host off the plane learns of a sweep only by receiving it. Reader
// confirms, by lookup, a host that admitted nothing.
type Legs struct {
	Settler publish.Settler
	Asset   *nodeapi.Asset
	Arcade  *publish.Arcade
	Facade  *publish.Facade
	Hosts   *unicast.Set
	Direct  *unicast.Set
	Reader  *reader.Client
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
	// warned is set once a warning about object size was printed.
	warned bool
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
	lock := func(ctx context.Context) (*script.Script, error) {
		return carrier.FundingLock(ctx, signer, signer.Originator, Params)
	}
	e.trees = &producer.Trees{Payer: e.payer, State: treeState{e}, Identity: signer.IdentityHex(),
		Count: o.TreeCount, Sats: o.TreeSats, Funder: "pool", Lock: lock, Change: signer.FundScript,
		Facade: unicast.Kept(), Topic: "tm_bbox_kept", Ahead: o.Ahead}
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
		if nc.Held > 0 {
			return fmt.Errorf("fee input: %w: the other coins are change from %d transaction(s) whose proofs have not arrived; they become spendable once mined", nc.Err, nc.Held)
		}
		return fmt.Errorf("fee input: %w (run `bbox fund`)", nc.Err)
	case errors.As(err, &nk):
		return fmt.Errorf("wallet output %s is locked to a key this home does not hold", nk.Outpoint)
	}
	return err
}

// Start reads the chain tip for coinbase maturity, collects the proofs of
// change the pool holds back, and publishes whatever a previous run
// persisted and did not confirm.
func (e *Engine) Start(ctx context.Context) error {
	h, err := e.Legs.Asset.BestHeader(ctx)
	if err != nil {
		return fmt.Errorf("node tip: %w", err)
	}
	e.payer.Tip = h.Height
	CollectChange(ctx, e.Pool, e.Legs.Asset)
	if err := e.FinishSweep(ctx); err != nil {
		return err
	}
	return e.Resume(ctx)
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
// so a host that already holds one answers a duplicate.
func (e *Engine) Resume(ctx context.Context) error {
	for len(e.St.Outbox) > 0 {
		p := e.St.Outbox[0]
		e.note("%s %s: publishing what a previous run persisted", p.Kind, p.Txid)
		if err := e.publish(ctx, p); err != nil {
			return err
		}
		e.St.Done(p.Txid)
		if err := e.St.Save(); err != nil {
			return err
		}
	}
	return nil
}

// Close collects a tree still being minted ahead, so its change reaches the
// pool, and records one never adopted in the state's Ahead list, so a sweep
// finds it.
func (e *Engine) Close(ctx context.Context) error {
	_ = e.trees.Wait(ctx)
	if p := e.trees.Prepared(); p != nil {
		e.St.Ahead = producer.Index(e.St.Ahead, p)
	}
	return e.St.Save()
}

// spend returns the funding tree the next carrier spends and the output,
// the tree's proof cut to the minimal one a host admits.
func (e *Engine) spend(ctx context.Context) (*transaction.Transaction, uint32, error) {
	tree, vout, err := e.trees.Spend(ctx, 1)
	if err != nil {
		return nil, 0, FeeError(fmt.Errorf("funding tree: %w", err))
	}
	if tree.MerklePath == nil {
		return nil, 0, fmt.Errorf("funding tree %s has no proof: a carrier spends only a mined tree", tree.TxID())
	}
	mp, err := Minimal(tree.MerklePath, tree.TxID())
	if err != nil {
		return nil, 0, fmt.Errorf("funding tree %s: %w", tree.TxID(), err)
	}
	tree.MerklePath = mp
	return tree, vout, nil
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

// Seal encrypts the plaintext to the recipient under BRC-78 through the
// sender's wallet (protocol [2, "message encryption"], a random key id) and
// completes and signs the BRC-169 envelope (spec section 4).
func Seal(ctx context.Context, w wallet.Interface, originator string, from, to, plaintext []byte, created uint64) ([]byte, error) {
	recipient, err := guard.ParsePubKey(to)
	if err != nil {
		return nil, fmt.Errorf("recipient: %w", err)
	}
	var keyID [32]byte
	if _, err := rand.Read(keyID[:]); err != nil {
		return nil, err
	}
	res, err := w.Encrypt(ctx, wallet.EncryptArgs{
		EncryptionArgs: wallet.EncryptionArgs{
			ProtocolID:   wallet.Protocol{SecurityLevel: 2, Protocol: "message encryption"},
			KeyID:        base64.StdEncoding.EncodeToString(keyID[:]),
			Counterparty: wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: recipient},
		},
		Plaintext: plaintext,
	}, originator)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	brc78 := append(append(append(append([]byte{}, boxrec.BRC78Version...), from...), to...), keyID[:]...)
	brc78 = append(brc78, res.Ciphertext...)
	doc := &boxrec.Object{}
	doc.Set(boxrec.MemberVersion, boxrec.MetanetHandles)
	party := func(k []byte) *boxrec.Object {
		o := &boxrec.Object{}
		o.Set(boxrec.MemberIdentity, hex.EncodeToString(k))
		return o
	}
	doc.Set(boxrec.MemberRecipient, party(to))
	doc.Set(boxrec.MemberSender, party(from))
	doc.Set(boxrec.MemberCreated, boxrec.RFC3339(created))
	return boxrec.Seal(ctx, w, originator, doc, brc78)
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
	content, err := Seal(ctx, e.Signer, e.Signer.Originator, from, l.To, l.Plaintext, l.Created)
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
	}
	return nil
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

// confirm is the lookup that shows a host holds p (spec section 9): the
// box class for an envelope, the receipt class for a receipt, the sweep
// class for a sweep.
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
		return e.Legs.Reader.Held(ctx, host, q, p.Txid)
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
