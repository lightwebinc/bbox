package vectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/big"
	"sort"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/pushdrop"

	"github.com/lightwebinc/bbox/boxrec"
)

// originator is the wallet originator the vectors sign under.
const originator = "vectors.example.com"

// OtherKey is a third test key, 32 bytes of 0x44: an identity that is
// neither the sender nor the recipient. Public, never for real use.
var OtherKey = bytes.Repeat([]byte{0x44}, 32)

// Headers is the test chain's block headers: a merkle root per height. It
// is the chain tracker the vectors are admitted against.
type Headers map[uint32]chainhash.Hash

// IsValidRootForHeight holds root to the one the test chain has at height.
func (h Headers) IsValidRootForHeight(_ context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	want, ok := h[height]
	return ok && root != nil && *root == want, nil
}

// CurrentHeight is the highest height held.
func (h Headers) CurrentHeight(context.Context) (uint32, error) {
	var top uint32
	for k := range h {
		top = max(top, k)
	}
	return top, nil
}

// chain mines transactions into blocks of two at successive heights: the
// transaction at offset 1 beside a fixed sibling.
type chain struct {
	height  uint32
	headers Headers
}

func (c *chain) mine(tx *transaction.Transaction) *transaction.Transaction {
	c.height++
	sibling := chainhash.Hash(bytes.Repeat([]byte{0x33}, 32))
	isTxid := true
	tx.MerklePath = transaction.NewMerklePath(c.height, [][]*transaction.PathElement{{
		{Offset: 0, Hash: &sibling},
		{Offset: 1, Hash: tx.TxID(), Txid: &isTxid},
	}})
	c.root(tx)
	return tx
}

// mineWide proves tx at offset 2 of a block of four with a path that also
// holds the leaf at offset 0, which the proof does not need.
func (c *chain) mineWide(tx *transaction.Transaction) *transaction.Transaction {
	c.height++
	h := func(b byte) *chainhash.Hash { v := chainhash.Hash(bytes.Repeat([]byte{b}, 32)); return &v }
	l0, l1, l3 := h(0x30), h(0x31), h(0x34)
	pair := chainhash.DoubleHashH(append(append([]byte{}, l0[:]...), l1[:]...))
	isTxid := true
	tx.MerklePath = transaction.NewMerklePath(c.height, [][]*transaction.PathElement{
		{{Offset: 0, Hash: l0}, {Offset: 2, Hash: tx.TxID(), Txid: &isTxid}, {Offset: 3, Hash: l3}},
		{{Offset: 0, Hash: &pair}},
	})
	c.root(tx)
	return tx
}

func (c *chain) root(tx *transaction.Transaction) {
	root, err := tx.MerklePath.ComputeRoot(tx.TxID())
	if err != nil {
		panic(err)
	}
	c.headers[c.height] = *root
}

// unproven is a fixed height nothing is mined at: a proof there verifies
// against no header the test chain holds.
const unproven = 999

// identity is one test identity's wallet and keys.
type identity struct {
	priv     *ec.PrivateKey
	w        wallet.Interface
	id       *ec.PublicKey
	env      *ec.PrivateKey // [1, "bbox message"] envelope, counterparty anyone
	fee      *ec.PrivateKey // [1, "bbox message"] fund, counterparty self
	feeLock  *script.Script
	envelope *ec.PublicKey
}

func newIdentity(k []byte) (*identity, error) {
	p := &identity{priv: priv(k)}
	var err error
	if p.w, err = wallet.NewCompletedProtoWallet(p.priv); err != nil {
		return nil, err
	}
	p.id = p.priv.PubKey()
	d := wallet.NewKeyDeriver(p.priv)
	if p.env, err = d.DerivePrivateKey(boxrec.Protocol, boxrec.KeyEnvelope, pushdrop.Anyone()); err != nil {
		return nil, err
	}
	if p.envelope, err = boxrec.EnvelopeDerivation.ExpectedLockingKey(p.id); err != nil {
		return nil, err
	}
	if !p.envelope.IsEqual(p.env.PubKey()) {
		return nil, fmt.Errorf("the envelope key does not match its reader derivation")
	}
	if p.fee, err = d.DerivePrivateKey(boxrec.Protocol, boxrec.KeyFund, wallet.Counterparty{Type: wallet.CounterpartyTypeSelf}); err != nil {
		return nil, err
	}
	addr, err := script.NewAddressFromPublicKey(p.fee.PubKey(), false)
	if err != nil {
		return nil, err
	}
	p.feeLock, err = p2pkh.Lock(addr)
	return p, err
}

func (p *identity) feeUnlocker() transaction.UnlockingScriptTemplate {
	u, err := p2pkh.Unlock(p.fee, nil)
	if err != nil {
		panic(err)
	}
	return u
}

// unlocker spends an output locked to the party's key under d with one
// signature, as bcommon's derivation unlocker does.
func (p *identity) unlocker(d pushdrop.Derivation) transaction.UnlockingScriptTemplate {
	return d.Unlocker(context.Background(), p.w, originator)
}

// lock writes a PushDrop of fields under d through the party's wallet.
func (p *identity) lock(d pushdrop.Derivation, sign bool, fields ...[]byte) *script.Script {
	s, err := d.Lock(context.Background(), p.w, originator, fields, sign)
	if err != nil {
		panic(err)
	}
	return s
}

// txEnv is everything the transaction and payment vectors are built from.
type txEnv struct {
	ctx       context.Context
	s, r, o   *identity // sender, recipient, a third identity
	c         *chain
	coin      *transaction.Transaction
	tree      *transaction.Transaction // the sender's funding tree
	rtree     *transaction.Transaction // the recipient's funding tree
	misc      *transaction.Transaction // outputs a carrier must not spend
	envs      []*sealed
	carriers  []*transaction.Transaction // one per envelope, tree output i
	receipt   *transaction.Transaction
	sweep     *transaction.Transaction
	payEnv    *sealed
	pay       *paymentBuild
	held      map[transaction.Outpoint]bool
	refusedBy map[string][]byte
}

func newTxEnv(built []*sealed, refused map[string][]byte) (*txEnv, error) {
	e := &txEnv{ctx: context.Background(), c: &chain{height: 800, headers: Headers{}}, envs: built, refusedBy: refused}
	var err error
	if e.s, err = newIdentity(SenderKey); err != nil {
		return nil, err
	}
	if e.r, err = newIdentity(RecipientKey); err != nil {
		return nil, err
	}
	if e.o, err = newIdentity(OtherKey); err != nil {
		return nil, err
	}
	return e, e.build()
}

func (e *txEnv) build() error {
	// The coin: outputs 0 to 3 pay the sender's fund key and 4 and 5 the
	// recipient's, from nowhere, mined.
	e.coin = transaction.NewTransaction()
	nowhere := chainhash.Hash(bytes.Repeat([]byte{0x11}, 32))
	e.coin.AddInput(&transaction.TransactionInput{SourceTXID: &nowhere, UnlockingScript: &script.Script{},
		SequenceNumber: transaction.MaxTxInSequenceNum})
	for i := range 6 {
		lock := e.s.feeLock
		if i >= 4 {
			lock = e.r.feeLock
		}
		e.coin.AddOutput(&transaction.TransactionOutput{Satoshis: 50000, LockingScript: lock})
	}
	e.c.mine(e.coin)

	// The funding trees: the sender's of six outputs, the recipient's of
	// three, each output 1000.
	p := boxrec.Params()
	sflock, err := carrier.FundingLock(e.ctx, e.s.w, originator, p)
	if err != nil {
		return err
	}
	if !bytes.Equal(*sflock, boxrec.FundingScript(e.s.envelope)) {
		return fmt.Errorf("bcommon's funding lock is not the funding script")
	}
	if e.tree, err = mint.FundingTree(sflock, 6, 1000, mint.Input{Tx: e.coin, Vout: 0, Unlocker: e.s.feeUnlocker()}, e.s.feeLock, mint.DefaultFees); err != nil {
		return err
	}
	e.c.mine(e.tree)
	rflock, err := carrier.FundingLock(e.ctx, e.r.w, originator, p)
	if err != nil {
		return err
	}
	if e.rtree, err = mint.FundingTree(rflock, 3, 1000, mint.Input{Tx: e.coin, Vout: 4, Unlocker: e.r.feeUnlocker()}, e.r.feeLock, mint.DefaultFees); err != nil {
		return err
	}
	e.c.mine(e.rtree)

	// misc: a mined parent of outputs no carrier may be funded from, each
	// spent by one signature: a funding-shaped output under the sender's
	// signature key, a bare pay-to-public-key under the envelope key, the
	// funding tag under the envelope key with a field signature, and
	// another tag under the envelope key.
	e.misc = transaction.NewTransaction()
	e.misc.AddInputFromTx(e.coin, 3, e.s.feeUnlocker())
	sigKey := mustKey(boxrec.SignatureDerivation, e.s.id)
	bare := script.Script(append(append([]byte{33}, e.s.envelope.Compressed()...), script.OpCHECKSIG))
	e.misc.AddOutput(out(1000, scr(boxrec.FundingScript(sigKey))))
	e.misc.AddOutput(out(1000, &bare))
	e.misc.AddOutput(out(1000, e.s.lock(boxrec.EnvelopeDerivation, true, boxrec.TagFunding)))
	e.misc.AddOutput(out(1000, e.s.lock(boxrec.EnvelopeDerivation, false, []byte{'b', 'b', 0x03})))
	e.misc.AddOutput(out(45500, e.s.feeLock))
	if err := e.misc.Sign(); err != nil {
		return err
	}
	e.c.mine(e.misc)

	// The payment rides in the third envelope, so it is built first.
	if err := e.buildPayment(); err != nil {
		return err
	}

	// One carrier per envelope, on sender tree outputs 0 to 2.
	for i, s := range append(append([]*sealed{}, e.envs...)[:2], e.payEnv) {
		k, err := carrier.Mint(e.ctx, e.s.w, originator, p, s.record, e.tree, uint32(i))
		if err != nil {
			return err
		}
		e.carriers = append(e.carriers, k)
	}

	// The recipient acknowledges the first two on its tree's output 0.
	acks := [][32]byte{carrier.Commitment(e.carriers[0]), carrier.Commitment(e.carriers[1])}
	sort.Slice(acks, func(i, j int) bool { return bytes.Compare(acks[i][:], acks[j][:]) < 0 })
	rec, err := (&boxrec.Receipt{Office: Office, By: e.r.id.Compressed(), Acks: acks, Created: t0 + 3600}).Encode()
	if err != nil {
		return err
	}
	if e.receipt, err = carrier.Mint(e.ctx, e.r.w, originator, p, rec, e.rtree, 0); err != nil {
		return err
	}

	// The sender sweeps tree outputs 0 and 1, retracting the first two
	// envelopes; the tree pays the fee and the tombstone takes the rest.
	if e.sweep, err = carrier.Sweep(e.ctx, e.s.w, originator, p, e.tree, []uint32{0, 1}, nil, 0, nil, e.s.feeLock, 1, 250); err != nil {
		return err
	}
	e.c.mine(e.sweep)

	// What the topic holds once the admitted cases are admitted: the
	// published funding tree's output 0, each carrier's record output and
	// the sweep's tombstone.
	e.held = map[transaction.Outpoint]bool{}
	for _, tx := range append([]*transaction.Transaction{e.tree, e.receipt, e.sweep}, e.carriers...) {
		e.held[transaction.Outpoint{Txid: *tx.TxID(), Index: 0}] = true
	}
	return nil
}

func mustKey(d pushdrop.Derivation, identity *ec.PublicKey) *ec.PublicKey {
	k, err := d.ExpectedLockingKey(identity)
	if err != nil {
		panic(err)
	}
	return k
}

func scr(b []byte) *script.Script { s := script.Script(b); return &s }

func out(sats uint64, s *script.Script) *transaction.TransactionOutput {
	return &transaction.TransactionOutput{Satoshis: sats, LockingScript: s}
}

// flagUnlocker signs a single-signature input with key under a sighash type
// other than the one a carrier uses.
type flagUnlocker struct {
	key  *ec.PrivateKey
	flag sighash.Flag
}

func (u flagUnlocker) Sign(tx *transaction.Transaction, i uint32) (*script.Script, error) {
	d, err := tx.CalcInputSignatureHash(i, u.flag)
	if err != nil {
		return nil, err
	}
	sig, err := u.key.Sign(d)
	if err != nil {
		return nil, err
	}
	return scr(push(append(sig.Serialize(), byte(u.flag)))), nil
}

func (u flagUnlocker) EstimateLength(*transaction.Transaction, uint32) uint32 { return 74 }

// signAll signs every input of tx again after a change to it.
func signAll(tx *transaction.Transaction) *transaction.Transaction {
	if err := tx.Sign(); err != nil {
		panic(err)
	}
	return tx
}

// carrierFrom builds a carrier by hand: one input spending src:vout, the
// given outputs, the carrier's locktime and sequence, signed.
func carrierFrom(src *transaction.Transaction, vout uint32, u transaction.UnlockingScriptTemplate, outs ...*transaction.TransactionOutput) *transaction.Transaction {
	tx := transaction.NewTransaction()
	tx.LockTime = carrier.LockTime
	tx.AddInputFromTx(src, vout, u)
	tx.Inputs[0].SequenceNumber = carrier.Sequence
	for _, o := range outs {
		tx.AddOutput(o)
	}
	return signAll(tx)
}

// derInt is a DER INTEGER's content for v.
func derInt(v *big.Int) []byte {
	b := v.Bytes()
	if len(b) == 0 || b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return b
}

// highSBytes rewrites a strict low-S DER signature to its high-S twin, which
// verifies all the same: S becomes n - S.
func highSBytes(der []byte) []byte {
	sig, err := ec.ParseDERSignature(der)
	if err != nil {
		panic(err)
	}
	r, s := derInt(sig.R), derInt(new(big.Int).Sub(ec.S256().N, sig.S))
	body := append(append([]byte{0x02, byte(len(r))}, r...), append([]byte{0x02, byte(len(s))}, s...)...)
	return append([]byte{0x30, byte(len(body))}, body...)
}

// cloneTx copies tx, keeping each input's source transaction so the copy's
// BEEF carries the same ancestry.
func cloneTx(tx *transaction.Transaction) *transaction.Transaction {
	cp := &transaction.Transaction{Version: tx.Version, LockTime: tx.LockTime, MerklePath: tx.MerklePath}
	for _, in := range tx.Inputs {
		c := *in
		cp.Inputs = append(cp.Inputs, &c)
	}
	for _, o := range tx.Outputs {
		c := *o
		cp.Outputs = append(cp.Outputs, &c)
	}
	return cp
}

// rewriteUnlock replaces input i's unlocking script with f of its one push.
func rewriteUnlock(tx *transaction.Transaction, i int, f func(sig []byte) []byte) *transaction.Transaction {
	cp := cloneTx(tx)
	ch, err := tx.Inputs[i].UnlockingScript.Chunks()
	if err != nil {
		panic(err)
	}
	cp.Inputs[i].UnlockingScript = scr(f(ch[0].Data))
	cp.MerklePath = nil
	return cp
}

// spendFrom returns tx with input 0's source replaced by src, which must be
// the same transaction under another proof (or none).
func spendFrom(tx, src *transaction.Transaction) *transaction.Transaction {
	cp := cloneTx(tx)
	cp.Inputs[0].SourceTransaction = src
	return cp
}

func push(d []byte) []byte {
	s := &script.Script{}
	if err := s.AppendPushData(d); err != nil {
		panic(err)
	}
	return *s
}

// fieldSig rebuilds a one-field signed PushDrop with its field signature
// replaced by f(signature), canonically otherwise.
func fieldSig(s *script.Script, key *ec.PublicKey, f func([]byte) []byte) *script.Script {
	ch, err := s.Chunks()
	if err != nil {
		panic(err)
	}
	return scr(boxrec.PushDropScript(key, [][]byte{ch[2].Data}, f(ch[3].Data)))
}

// v1BEEF writes a BEEF V1 by hand: the BUMPs, then each transaction with
// the index of its BUMP (-1 for none), in the order given; the last is the
// subject.
func v1BEEF(bumps []*transaction.MerklePath, txs []*transaction.Transaction, bumpOf []int) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, uint32(0xEFBE0001))
	b.Write(varInt(uint64(len(bumps))))
	for _, m := range bumps {
		b.Write(m.Bytes())
	}
	b.Write(varInt(uint64(len(txs))))
	for i, tx := range txs {
		b.Write(tx.Bytes())
		if bumpOf[i] < 0 {
			b.WriteByte(0)
			continue
		}
		b.WriteByte(1)
		b.Write(varInt(uint64(bumpOf[i])))
	}
	return b.Bytes()
}

func varInt(n uint64) []byte {
	switch {
	case n < 0xfd:
		return []byte{byte(n)}
	case n <= 0xffff:
		return []byte{0xfd, byte(n), byte(n >> 8)}
	}
	panic("varInt: too large for a vector")
}

type admitJSON struct {
	Kind        string   `json:"kind"`
	Outputs     []int    `json:"outputsToAdmit"`
	Retain      []int    `json:"coinsToRetain"`
	Commitment  string   `json:"commitment,omitempty"`
	Record      string   `json:"record,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	FundingTxid string   `json:"fundingTxid,omitempty"`
	FundingVout *int     `json:"fundingVout,omitempty"`
	Spent       []string `json:"spent,omitempty"`
}

type coinJSON struct {
	InputIndex int    `json:"inputIndex"`
	Txid       string `json:"txid"`
	Vout       uint32 `json:"vout"`
}

type txJSON struct {
	Name  string `json:"name"`
	Note  string `json:"note"`
	Rule  string `json:"rule,omitempty"`
	Txid  string `json:"txid"`
	RawTx string `json:"rawTx"`
	BEEF  string `json:"beef"`
	// Coins are the previous coins the case is admitted with: the inputs
	// that spend an output the topic holds.
	Coins []coinJSON `json:"previousCoins"`
	// Scripts says whether every input of the transaction itself passes
	// the script interpreter: a refusal whose scripts verify is one only
	// the topic manager's rules catch.
	Scripts bool       `json:"scriptsVerify"`
	Verdict string     `json:"verdict"`
	Reason  string     `json:"reason,omitempty"`
	Admits  *admitJSON `json:"admits,omitempty"`
}

type txCase struct {
	name, note, rule string
	tx               *transaction.Transaction
	beef             []byte // the BEEF as submitted; the Atomic BEEF of tx when nil
	reason           string // "" admits
	held             []int  // overrides the computed previous coins when not nil
}

// scriptsVerify runs the interpreter over tx's own inputs, each against the
// output it spends.
func scriptsVerify(ctx context.Context, tx *transaction.Transaction) bool {
	cp := tx.ShallowClone()
	cp.MerklePath = nil
	ok, err := spv.Verify(ctx, cp, &spv.GullibleHeadersClient{}, nil)
	return err == nil && ok
}

// heldInputs is the previous coins the engine hands the topic manager for
// tx: the inputs that spend an output the topic holds.
func (e *txEnv) heldInputs(tx *transaction.Transaction) []int {
	held := []int{}
	for i, in := range tx.Inputs {
		if e.held[transaction.Outpoint{Txid: *in.SourceTXID, Index: in.SourceTxOutIndex}] {
			held = append(held, i)
		}
	}
	return held
}

func admission(a *boxrec.Admission) *admitJSON {
	j := &admitJSON{Kind: a.Kind.String(), Outputs: append([]int{}, a.Outputs...), Retain: append([]int{}, a.Retain...)}
	if c := a.Carrier; c != nil {
		v := int(c.Funding.Index)
		j.Commitment, j.Record, j.Owner = h(c.Commitment[:]), h(c.Record), h(c.Owner)
		j.FundingTxid, j.FundingVout = c.Funding.Txid.String(), &v
	}
	for _, o := range a.Spent {
		j.Spent = append(j.Spent, fmt.Sprintf("%s.%d", o.Txid.String(), o.Index))
	}
	return j
}

// transactionVectors builds every transaction case and admits it, and
// fails unless each is admitted or refused exactly as the case says and
// every input of every case passes the script interpreter.
func (e *txEnv) transactionVectors() (map[string]any, error) {
	s, r := e.s, e.r
	env := boxrec.EnvelopeDerivation
	k1 := e.carriers[0]
	rec1 := e.envs[0].record
	lock1 := k1.Outputs[0].LockingScript
	sig1 := func() []byte { ch, _ := lock1.Chunks(); return ch[3].Data }()
	sUnlock := s.unlocker(env)
	var cases []txCase
	add := func(c txCase) { cases = append(cases, c) }

	// Admitted.
	add(txCase{name: "carrier-envelope-note", note: "the envelope-note record's carrier, spending sender funding-tree output 0; that output is held (the published funding tree admitted it), so the carrier retains it", tx: k1})
	add(txCase{name: "carrier-envelope-refs", note: "the envelope-refs record's carrier, spending sender funding-tree output 1", tx: e.carriers[1]})
	add(txCase{name: "carrier-envelope-payment", note: "the payment envelope's carrier (payment-v1.json), spending sender funding-tree output 2; a host sees only the record", tx: e.carriers[2]})
	add(txCase{name: "carrier-receipt", note: "the recipient's receipt acknowledging carrier-envelope-note and carrier-envelope-refs, spending recipient funding-tree output 0", tx: e.receipt})
	v1, err := k1.BEEF()
	if err != nil {
		return nil, err
	}
	add(txCase{name: "carrier-beef-v1", note: "carrier-envelope-note submitted as BEEF V1 (BRC-62) rather than Atomic BEEF: the same two transactions and one proof", tx: k1, beef: v1})
	far := carrierFrom(e.tree, 0, sUnlock, out(1000, lock1))
	far.LockTime, far.Inputs[0].SequenceNumber = 0xffffffff, 0xfffffffe
	add(txCase{name: "carrier-locktime-max", note: "the envelope-note record on the same funding output under nLockTime 0xFFFFFFFF and nSequence 0xFFFFFFFE: any locktime from 2100 and any non-final sequence is admitted, and a second carrier on one funding output is admitted and left to the index to supersede", tx: signAll(far)})
	add(txCase{name: "funding-tree", note: "the sender's funding tree, published after it mined: output 0 is funding-shaped, so it is admitted as a sweep would be, and its input's outpoint is recorded", tx: e.tree})
	add(txCase{name: "sweep", note: "a mined sweep of sender funding-tree outputs 0 and 1: its tombstone, output 0, is admitted and both outpoints are recorded, which retracts carrier-envelope-note and carrier-envelope-refs; it spends the held output 0 of the published funding tree, which it retains", tx: e.sweep})
	spend := transaction.NewTransaction()
	spend.AddInputFromTx(e.sweep, 0, sUnlock)
	spend.AddOutput(out(e.sweep.Outputs[0].Satoshis-200, s.feeLock))
	add(txCase{name: "spend-tombstone", note: "an unmined transaction spending the sweep's tombstone, which the topic holds: it claims nothing and is not a sweep, so it admits nothing and retains the tombstone", tx: signAll(spend)})

	// Carrier shape.
	two := carrierFrom(e.tree, 0, sUnlock, out(1000, lock1))
	two.AddInputFromTx(e.tree, 3, sUnlock)
	add(txCase{name: "carrier-two-inputs", note: "carrier-envelope-note's output with a second funding input", rule: "carrier shape", tx: signAll(two), reason: "carrier-shape"})
	add(txCase{name: "carrier-two-outputs", note: "the record output with 999 and a second output of 1", rule: "carrier shape",
		tx: carrierFrom(e.tree, 0, sUnlock, out(999, lock1), out(1, s.feeLock)), reason: "carrier-shape"})
	add(txCase{name: "carrier-value-short", note: "the one output carries 999 of the input's 1000", rule: "carrier shape",
		tx: carrierFrom(e.tree, 0, sUnlock, out(999, lock1)), reason: "carrier-shape"})
	add(txCase{name: "carrier-two-claiming-outputs", note: "one output claims an envelope and another a receipt", rule: "carrier shape",
		tx: carrierFrom(e.tree, 0, sUnlock, out(500, lock1), out(500, e.receipt.Outputs[0].LockingScript)), reason: "carrier-shape"})

	// The BEEF: exactly the carrier and its proven funding tree.
	bare := cloneTx(e.tree)
	bare.MerklePath = nil
	add(txCase{name: "carrier-beef-tree-unproven", note: "carrier-envelope-note (the same txid) with its funding tree unproven, so its BEEF also carries the tree's mined parent: SPV passes, and the carrier is refused rather than recorded, so this BEEF cannot suppress the genuine one", rule: "carrier shape",
		tx: spendFrom(k1, bare), reason: "beef"})
	wide := e.c.mineWide(cloneTx(e.tree))
	add(txCase{name: "carrier-beef-proof-extra-leaf", note: "carrier-envelope-note (the same txid) with its funding tree proven by a path that also holds a leaf the proof does not need", rule: "carrier shape",
		tx: spendFrom(k1, wide), reason: "beef"})
	add(txCase{name: "carrier-beef-extra-transaction", note: "carrier-envelope-note's BEEF with an unrelated mined transaction and its proof riding along", rule: "carrier shape",
		tx: k1, beef: v1BEEF([]*transaction.MerklePath{e.misc.MerklePath, e.tree.MerklePath}, []*transaction.Transaction{e.misc, e.tree, k1}, []int{0, 1, -1}), reason: "beef"})
	add(txCase{name: "carrier-beef-no-funding-tree", note: "carrier-envelope-note's BEEF without its funding tree: the value the input spends cannot be read", rule: "carrier shape",
		tx: k1, beef: v1BEEF(nil, []*transaction.Transaction{k1}, []int{-1}), reason: "beef"})
	badRec := fields{
		boxrec.EKMagic: boxrec.MagicEnvelope, boxrec.EKOffice: Office, boxrec.EKTo: e.envs[0].env.To, boxrec.EKBox: "inbox",
		boxrec.EKFrom: e.envs[0].env.From, boxrec.EKCreated: uint64(0), boxrec.EKExpires: uint64(0), boxrec.EKContent: e.envs[0].env.Content,
	}.encode()
	add(txCase{name: "carrier-beef-before-record", note: "breaks the beef rule (funding tree unproven) and the record rules (created 0): refused for the BEEF", rule: "carrier shape",
		tx: spendFrom(carrierFrom(e.tree, 0, sUnlock, out(1000, s.lock(env, true, badRec))), bare), reason: "beef"})

	// The record's own rules, then the office.
	add(txCase{name: "carrier-record-refused", note: "the envelope record has created 0; the record's own rules refuse it before any later rule",
		tx: carrierFrom(e.tree, 0, sUnlock, out(1000, s.lock(env, true, badRec))), reason: "range"})
	badAcks := fields{
		boxrec.RKMagic: boxrec.MagicReceipt, boxrec.RKOffice: Office, boxrec.RKBy: r.id.Compressed(),
		boxrec.RKAcks: acksValue([][32]byte{carrier.Commitment(k1), carrier.Commitment(k1)}), boxrec.RKCreated: uint64(t0 + 3600),
	}.encode()
	add(txCase{name: "carrier-receipt-record-refused", note: "a receipt acknowledging one commitment twice",
		tx: carrierFrom(e.rtree, 0, r.unlocker(env), out(1000, r.lock(env, true, badAcks))), reason: "acks"})
	other := *e.envs[0].env
	other.Office = OtherOffice
	otherRec, err := other.Encode()
	if err != nil {
		return nil, err
	}
	otherCarrier := carrierFrom(e.tree, 0, sUnlock, out(1000, s.lock(env, true, otherRec)))
	add(txCase{name: "carrier-other-office", note: "a well-formed envelope carrier whose record names another office than the topic's", tx: otherCarrier, reason: "office"})
	rOther := &boxrec.Receipt{Office: OtherOffice, By: r.id.Compressed(), Acks: sortedCommitments(1, 1), Created: t0 + 3600}
	rOtherRec, err := rOther.Encode()
	if err != nil {
		return nil, err
	}
	add(txCase{name: "carrier-receipt-other-office", note: "a well-formed receipt carrier whose record names another office",
		tx: carrierFrom(e.rtree, 0, r.unlocker(env), out(1000, r.lock(env, true, rOtherRec))), reason: "office"})
	finalOther := cloneTx(otherCarrier)
	finalOther.Inputs[0].SequenceNumber = transaction.MaxTxInSequenceNum
	add(txCase{name: "carrier-office-before-mineable", note: "breaks the office rule and the mineable rule (a final input): refused for the office",
		tx: signAll(finalOther), reason: "office"})

	// Mineability.
	low := carrierFrom(e.tree, 0, sUnlock, out(1000, lock1))
	low.LockTime = carrier.LockTime - 1
	add(txCase{name: "carrier-locktime-below", note: "nLockTime one second before 2100", rule: "carrier shape", tx: signAll(low), reason: "mineable"})
	final := carrierFrom(e.tree, 0, sUnlock, out(1000, lock1))
	final.Inputs[0].SequenceNumber = transaction.MaxTxInSequenceNum
	add(txCase{name: "carrier-final-input", note: "the input's nSequence is 0xFFFFFFFF", rule: "carrier shape", tx: signAll(final), reason: "mineable"})
	add(txCase{name: "carrier-mineable-before-unlock", note: "nLockTime one second before 2100 and the signature pushed by OP_PUSHDATA1: refused as mineable", rule: "carrier shape",
		tx: rewriteUnlock(signAll(low), 0, func(sg []byte) []byte { return append([]byte{script.OpPUSHDATA1, byte(len(sg))}, sg...) }), reason: "mineable"})

	// Unlocking, after finality and before the lock, as bcommon
	// carrier.Validate orders them.
	add(txCase{name: "carrier-unlock-high-s", note: "carrier-envelope-note with its input signature's high-S twin, which verifies: another txid, same record", rule: "signatures",
		tx: rewriteUnlock(k1, 0, func(sg []byte) []byte { return push(append(highSBytes(sg[:len(sg)-1]), sg[len(sg)-1])) }), reason: "unlock"})
	add(txCase{name: "carrier-unlock-pushdata1", note: "carrier-envelope-note with its signature pushed by OP_PUSHDATA1", rule: "carrier shape",
		tx: rewriteUnlock(k1, 0, func(sg []byte) []byte { return append([]byte{script.OpPUSHDATA1, byte(len(sg))}, sg...) }), reason: "unlock"})
	add(txCase{name: "carrier-unlock-extra-push", note: "carrier-envelope-note with OP_0 pushed before its signature", rule: "carrier shape",
		tx: rewriteUnlock(k1, 0, func(sg []byte) []byte { return append([]byte{script.Op0}, push(sg)...) }), reason: "unlock"})
	add(txCase{name: "carrier-unlock-sighash-single", note: "the input signed SIGHASH_SINGLE|FORKID (0x43), which verifies", rule: "signatures",
		tx: carrierFrom(e.tree, 0, flagUnlocker{s.env, sighash.SingleForkID}, out(1000, lock1)), reason: "unlock"})
	add(txCase{name: "carrier-unlock-sighash-anyonecanpay", note: "the input signed SIGHASH_ALL|ANYONECANPAY|FORKID (0xc1), which verifies", rule: "signatures",
		tx: carrierFrom(e.tree, 0, flagUnlocker{s.env, sighash.AllForkID | sighash.AnyOneCanPay}, out(1000, lock1)), reason: "unlock"})
	add(txCase{name: "carrier-unlock-before-lock", note: "breaks the lock rule (record output under the signature key) and the unlock rule (signature pushed by OP_PUSHDATA1): refused for the unlocking script", rule: "carrier shape",
		tx: rewriteUnlock(carrierFrom(e.tree, 0, sUnlock, out(1000, s.lock(boxrec.SignatureDerivation, true, rec1))), 0, func(sg []byte) []byte {
			return append([]byte{script.OpPUSHDATA1, byte(len(sg))}, sg...)
		}), reason: "unlock"})

	// Script canonicality.
	add(txCase{name: "carrier-lock-signature-key", note: "the record output locked and signed under the sender's key for key id signature, not envelope", rule: "script canonicality",
		tx: carrierFrom(e.tree, 0, sUnlock, out(1000, s.lock(boxrec.SignatureDerivation, true, rec1))), reason: "lock"})
	add(txCase{name: "carrier-lock-recipient-key", note: "the sender's record locked and signed under the recipient's envelope key: the lock is bound to the record's from", rule: "script canonicality",
		tx: carrierFrom(e.tree, 0, sUnlock, out(1000, r.lock(env, true, rec1))), reason: "lock"})
	if len(rec1) <= 255 || len(rec1) > 65535 {
		return nil, fmt.Errorf("the note record is %d bytes; the non-minimal push case assumes OP_PUSHDATA2", len(rec1))
	}
	wideLock := append(append(append([]byte{33}, s.envelope.Compressed()...), script.OpCHECKSIG, script.OpPUSHDATA4, byte(len(rec1)), byte(len(rec1)>>8), 0, 0),
		append(append(append([]byte{}, rec1...), push(sig1)...), script.Op2DROP)...)
	add(txCase{name: "carrier-lock-non-minimal-push", note: "the record pushed with OP_PUSHDATA4 where OP_PUSHDATA2 is minimal; same fields, same signature", rule: "script canonicality",
		tx: carrierFrom(e.tree, 0, sUnlock, out(1000, scr(wideLock))), reason: "lock"})
	dd := append(append([]byte{}, (*lock1)[:len(*lock1)-1]...), script.OpDROP, script.OpDROP)
	add(txCase{name: "carrier-lock-drop-drop", note: "OP_DROP OP_DROP where OP_2DROP is the fewest", rule: "script canonicality",
		tx: carrierFrom(e.tree, 0, sUnlock, out(1000, scr(dd))), reason: "lock"})
	add(txCase{name: "carrier-lock-unsigned", note: "the record output carries no field signature", rule: "script canonicality",
		tx: carrierFrom(e.tree, 0, sUnlock, out(1000, scr(boxrec.PushDropScript(s.envelope, [][]byte{rec1}, nil)))), reason: "lock"})
	add(txCase{name: "carrier-lock-extra-field", note: "a third push after the record and its signature, dropped canonically", rule: "script canonicality",
		tx: carrierFrom(e.tree, 0, sUnlock, out(1000, scr(boxrec.PushDropScript(s.envelope, [][]byte{rec1, sig1}, []byte{0x62, 0x62})))), reason: "lock"})
	add(txCase{name: "carrier-lock-trailing-opcode", note: "the canonical script followed by OP_NOP", rule: "script canonicality",
		tx: carrierFrom(e.tree, 0, sUnlock, out(1000, scr(append(append([]byte{}, *lock1...), script.OpNOP)))), reason: "lock"})

	// Signatures.
	add(txCase{name: "carrier-field-signature-high-s", note: "the field signature's high-S twin, which verifies; the script is otherwise canonical (bcommon carrier.Validate alone accepts it)", rule: "signatures",
		tx: carrierFrom(e.tree, 0, sUnlock, out(1000, fieldSig(lock1, s.envelope, highSBytes))), reason: "signature"})
	signBy := func(k *ec.PrivateKey, msg []byte) func([]byte) []byte {
		return func([]byte) []byte {
			d := sha256.Sum256(msg)
			sg, _ := k.Sign(d[:])
			return sg.Serialize()
		}
	}
	add(txCase{name: "carrier-field-signature-wrong-key", note: "a strict signature over the record by the third identity's key", rule: "signatures",
		tx: carrierFrom(e.tree, 0, sUnlock, out(1000, fieldSig(lock1, s.envelope, signBy(e.o.priv, rec1)))), reason: "signature"})
	add(txCase{name: "carrier-field-signature-identity-key", note: "a strict signature over the record by the sender's identity key rather than its envelope key", rule: "signatures",
		tx: carrierFrom(e.tree, 0, sUnlock, out(1000, fieldSig(lock1, s.envelope, signBy(s.priv, rec1)))), reason: "signature"})

	// Funding.
	add(txCase{name: "carrier-funding-bare-p2pk", note: "spends a bare pay-to-public-key output under the sender's envelope key instead of a funding output", rule: "carrier shape",
		tx: carrierFrom(e.misc, 1, sUnlock, out(1000, lock1)), reason: "funding"})
	add(txCase{name: "carrier-funding-signature-key", note: "spends a funding-shaped output locked to the sender's signature key", rule: "carrier shape",
		tx: carrierFrom(e.misc, 0, s.unlocker(boxrec.SignatureDerivation), out(1000, lock1)), reason: "funding"})
	add(txCase{name: "carrier-funding-signed-tag", note: "spends an output under the envelope key whose one field is the funding tag with a field signature", rule: "script canonicality",
		tx: carrierFrom(e.misc, 2, sUnlock, out(1000, lock1)), reason: "funding"})
	add(txCase{name: "carrier-funding-other-tag", note: "spends an output under the envelope key whose one field is 62 62 03", rule: "carrier shape",
		tx: carrierFrom(e.misc, 3, sUnlock, out(1000, lock1)), reason: "funding"})

	// Content, last: each record passes the record rules and its content
	// breaks one content rule (refusal-v1.json).
	for _, c := range []struct{ name, reason string }{
		{"content-white-space", "content-json"},
		{"content-payment-object", "content-shape"},
		{"content-cipher-wrong-recipient", "content-cipher"},
		{"content-signature-identity-key", "content-signature"},
	} {
		rec, ok := e.refusedBy[c.name]
		if !ok {
			return nil, fmt.Errorf("no refusal vector %s", c.name)
		}
		add(txCase{name: "carrier-" + c.name, note: "a carrier whose record is refusal-v1.json's " + c.name + ": every carrier rule holds, and the content rules refuse it",
			tx: carrierFrom(e.tree, 0, sUnlock, out(1000, s.lock(env, true, rec))), reason: c.reason})
	}

	// Transactions that claim nothing.
	unminedTree := cloneTx(e.tree)
	unminedTree.MerklePath = nil
	add(txCase{name: "funding-tree-unmined", note: "the sender's funding tree with no proof", rule: "sweep", tx: unminedTree, reason: "unmined"})
	unminedSweep := cloneTx(e.sweep)
	unminedSweep.MerklePath = nil
	add(txCase{name: "sweep-unmined", note: "the sweep with no proof: nothing unmined retracts. It spends the held output 0 of the published funding tree, and is still judged as a sweep rather than a spend: refused, so it retains nothing, and decided again when it arrives mined", rule: "sweep", tx: unminedSweep, reason: "unmined"})
	stray := cloneTx(e.sweep)
	stray.MerklePath = transaction.NewMerklePath(unproven, e.sweep.MerklePath.Path)
	add(txCase{name: "sweep-proof-unknown-height", note: "the sweep with a proof at a height the headers do not hold", rule: "sweep", tx: stray, reason: "unmined"})
	late := transaction.NewTransaction()
	late.AddInputFromTx(e.tree, 4, sUnlock)
	late.AddOutput(out(700, s.feeLock))
	late.AddOutput(out(100, scr(boxrec.FundingScript(s.envelope))))
	add(txCase{name: "sweep-tombstone-second", note: "a mined spend of funding-tree output 4 whose funding-shaped output is output 1, not 0: it claims nothing, is not a sweep and spends nothing held", rule: "sweep",
		tx: e.c.mine(signAll(late)), reason: "not-bbox"})
	tomb := boxrec.FundingScript(s.envelope)
	loose := append(append(append([]byte{}, tomb[:35]...), script.OpPUSHDATA1, 3), tomb[36:]...)
	odd := transaction.NewTransaction()
	odd.AddInputFromTx(e.tree, 5, sUnlock)
	odd.AddOutput(out(800, scr(loose)))
	add(txCase{name: "sweep-tombstone-pushdata1", note: "a mined spend of funding-tree output 5 whose output 0 pushes the funding tag with OP_PUSHDATA1: not a well-formed funding output", rule: "script canonicality",
		tx: e.c.mine(signAll(odd)), reason: "not-bbox"})
	add(txCase{name: "spend-not-held", note: "spend-tombstone offered with no previous coins: the topic does not hold what it spends", rule: "sweep",
		tx: signAll(spend), held: []int{}, reason: "not-bbox"})

	var list []txJSON
	for _, c := range cases {
		beef := c.beef
		if beef == nil {
			if beef, err = c.tx.AtomicBEEF(false); err != nil {
				return nil, fmt.Errorf("%s: beef: %w", c.name, err)
			}
		}
		held := c.held
		if held == nil {
			held = e.heldInputs(c.tx)
		}
		a, err := boxrec.Admit(e.ctx, beef, held, boxrec.Host{Office: Office, Headers: e.c.headers})
		j := txJSON{Name: c.name, Note: c.note, Rule: c.rule, Txid: c.tx.TxID().String(), RawTx: c.tx.Hex(),
			BEEF: h(beef), Coins: []coinJSON{}, Scripts: scriptsVerify(e.ctx, c.tx)}
		for _, i := range held {
			in := c.tx.Inputs[i]
			j.Coins = append(j.Coins, coinJSON{InputIndex: i, Txid: in.SourceTXID.String(), Vout: in.SourceTxOutIndex})
		}
		if !j.Scripts {
			return nil, fmt.Errorf("%s: its own scripts do not verify", c.name)
		}
		switch {
		case c.reason == "" && err != nil:
			return nil, fmt.Errorf("%s: refused: %w", c.name, err)
		case c.reason != "" && err == nil:
			return nil, fmt.Errorf("%s: admitted, want %s", c.name, c.reason)
		case c.reason != "" && boxrec.Reason(err) != c.reason:
			return nil, fmt.Errorf("%s: reason %s, want %s (%v)", c.name, boxrec.Reason(err), c.reason, err)
		case err == nil:
			if a.Txid != [32]byte(*c.tx.TxID()) {
				return nil, fmt.Errorf("%s: admitted another subject", c.name)
			}
			j.Verdict, j.Admits = "admit", admission(a)
		default:
			j.Verdict, j.Reason = "refuse", c.reason
		}
		list = append(list, j)
	}
	var headers []map[string]any
	for ht := uint32(801); ht <= e.c.height; ht++ {
		headers = append(headers, map[string]any{"height": ht, "merkleRoot": e.c.headers[ht].String()})
	}
	return map[string]any{
		"description":             "Transactions admitted or refused by the topic manager's rules (docs/spec.md section 8.1), in the order given there. Each case is the transaction as raw hex and as the BEEF submitted (Atomic BEEF unless the case says otherwise: an unmined transaction's with its ancestry to a mined parent, a mined transaction's with its own proof and no parents), and the previous coins it is admitted with: the inputs that spend an output the topic holds once the admitted cases are admitted, as the overlay engine hands them to the topic manager (inputIndex), each with the outpoint it spends (txid in display order). Admission runs against the office and the headers below, with a BEEF bound of maxBeef bytes. A refusal is raised, never answered with empty instructions. admits lists the admittance instructions (outputsToAdmit, coinsToRetain) and what the lookup service records: a carrier's commitment (hash byte order), record, owner and funding outpoint, and a sweep's spent outpoints. scriptsVerify says every input of the transaction passes the script interpreter against the output it spends, so the engine's SPV does not refuse a case for its scripts: each refusal is made by the rule the case names and by no earlier one. rule names the frozen-list row a refusal exercises. Signatures are deterministic (RFC 6979), so the bytes are reproducible.",
		"office":                  Office,
		"topic":                   boxrec.TopicPrefix + Office,
		"maxBeef":                 boxrec.DefaultMaxBEEF,
		"originator":              originator,
		"senderIdentityKey":       h(s.id.Compressed()),
		"recipientIdentityKey":    h(r.id.Compressed()),
		"otherIdentityKey":        h(e.o.id.Compressed()),
		"senderEnvelopeKey":       h(s.envelope.Compressed()),
		"recipientEnvelopeKey":    h(r.envelope.Compressed()),
		"senderFundingScript":     h(boxrec.FundingScript(s.envelope)),
		"recipientFundingScript":  h(boxrec.FundingScript(r.envelope)),
		"headersNote":             "merkleRoot is in display byte order, as a txid is printed",
		"headers":                 headers,
		"transactions":            list,
		"fundKeyNote":             "fee coins pay P2PKH to each identity's [1, \"bbox message\"] key id fund under counterparty self, the embedded wallet's funding key; no admission rule reads which key a fee coin pays",
		"senderFundKey":           h(s.fee.PubKey().Compressed()),
		"recipientFundKey":        h(r.fee.PubKey().Compressed()),
		"sweepRetractsOutpoints":  []string{fmt.Sprintf("%s.0", e.tree.TxID().String()), fmt.Sprintf("%s.1", e.tree.TxID().String())},
		"receiptAcknowledgesTxid": []string{k1.TxID().String(), e.carriers[1].TxID().String()},
	}, nil
}
