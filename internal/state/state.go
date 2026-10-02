// Package state is an identity's durable record in its home: the funding
// trees its carriers spend, the trees signed and not yet adopted, the
// carriers persisted before they are published, what it sent and
// acknowledged, the sweeps it built, the envelopes it read and the payments
// it took.
//
// It is written before anything leaves the machine. A carrier is persisted
// with the funding output it spends marked used before it is submitted, so
// a run that stops part way republishes the same bytes and never makes a
// second carrier on the same funding output (spec section 8.2: a host
// answers one carrier per funding output, over its life). The file is
// written atomically (a temporary file, fsync, rename, fsync of the
// directory) at mode 0600, and the home is locked while a command writes.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
)

// Version is the state file's format version.
const Version = 1

// File is the state file's name in the home.
const File = "state.json"

// The kinds of object a home publishes.
const (
	KindEnvelope = "envelope"
	KindReceipt  = "receipt"
	KindSweep    = "sweep"
)

// State is one identity's state.
type State struct {
	Version int `json:"version"`
	// Identity is the identity key, compressed hex.
	Identity string `json:"identity"`
	// Offices are the offices this home created (office new).
	Offices []string `json:"offices,omitempty"`

	// Tree is the funding tree carriers spend now, Trees every tree minted,
	// and Ahead the trees minted ahead and never adopted when a run ended:
	// on the chain, in no other list, kept so a sweep finds them.
	Tree  *funding.Tree  `json:"tree,omitempty"`
	Trees []funding.Tree `json:"trees,omitempty"`
	Ahead []funding.Tree `json:"ahead,omitempty"`
	// PendingTrees are the funding trees signed for a pool coin that the
	// home has not adopted yet, each with the coin it spends: the record
	// made before a tree reaches the settlement leg, so a run that stops
	// before the tree is adopted leaves what the next command needs to ask
	// the node what became of it. Adopting a tree drops its record in the
	// same save.
	PendingTrees []PendingTree `json:"pendingTrees,omitempty"`

	// Outbox holds every object persisted and not yet confirmed
	// published, oldest first. A command that finds one publishes the
	// same bytes before it builds anything new.
	Outbox []Pending `json:"outbox,omitempty"`
	// Sent are the envelopes this identity sent; Receipts the receipts it
	// published; Sweeps the sweeps it built.
	Sent     []Sent    `json:"sent,omitempty"`
	Receipts []Receipt `json:"receipts,omitempty"`
	Sweeps   []Sweep   `json:"sweeps,omitempty"`
	// Received are the envelopes to this identity it has read.
	Received []Received `json:"received,omitempty"`
	// Settled are the txids of payments to this identity as a host's payee
	// that payee settle internalized.
	Settled []string `json:"settled,omitempty"`
	// Unsettleable are payments to this identity as a payee that the
	// network refused for good (the payer spent the inputs elsewhere):
	// settle reports each once and then passes it over.
	Unsettleable []Unsettleable `json:"unsettleable,omitempty"`
	// LastSentMs is when the last envelope was built, Unix milliseconds,
	// for the send rate.
	LastSentMs int64 `json:"lastSentMs,omitempty"`

	// Payments are the payments this identity made for priced questions
	// that have not mined yet, each as the BEEF the host was handed. A
	// payment is recorded before it is sent: from then on the host holds a
	// transaction that spends the coin.
	Payments []Payment `json:"payments,omitempty"`

	// Taking is the coin pool as it stood when a command that spends from
	// it last saved this state: the record made before a coin is taken. A
	// coin leaves the pool's own file the moment it is taken, before the
	// transaction that spends it is recorded here, so a run that stops in
	// between leaves a coin in neither. The next command finds it in this
	// list and in no transaction the home records, and puts it back when
	// the node shows it unspent. It is empty whenever no command is
	// running.
	Taking []bwallet.Output `json:"taking,omitempty"`

	path       string
	beforeSave func()
}

// OnSave sets what runs before every Save: a publisher records the pool
// with it. Nil clears it.
func (s *State) OnSave(f func()) { s.beforeSave = f }

// PendingTree is a funding tree signed for a pool coin and not yet
// adopted, keyed by the tree's txid.
type PendingTree struct {
	// Tree is the record the tree is adopted as, without a proof.
	Tree funding.Tree `json:"tree"`
	// Coin is the fee coin the tree spends, as the pool held it.
	Coin bwallet.Output `json:"coin"`
	// ReturnedOnce is set when a command found the tree absent from the
	// node and the coin unspent, and put the coin back: the record is kept
	// for one more command, in case the tree reaches the node after all.
	ReturnedOnce bool `json:"returned_once,omitempty"`
}

// KeepPendingTree records p, replacing a record of the same tree.
func (s *State) KeepPendingTree(p PendingTree) {
	for i := range s.PendingTrees {
		if s.PendingTrees[i].Tree.Txid == p.Tree.Txid {
			s.PendingTrees[i] = p
			return
		}
	}
	s.PendingTrees = append(s.PendingTrees, p)
}

// PendingTreeOf finds the record of the tree txid.
func (s *State) PendingTreeOf(txid string) *PendingTree {
	for i := range s.PendingTrees {
		if s.PendingTrees[i].Tree.Txid == txid {
			return &s.PendingTrees[i]
		}
	}
	return nil
}

// DropPendingTree drops the record of the tree txid, and reports whether
// there was one.
func (s *State) DropPendingTree(txid string) bool {
	n := len(s.PendingTrees)
	s.PendingTrees = slices.DeleteFunc(s.PendingTrees, func(p PendingTree) bool { return p.Tree.Txid == txid })
	return len(s.PendingTrees) != n
}

// MintedAhead finds txid among the trees minted ahead and not adopted.
func (s *State) MintedAhead(txid string) *funding.Tree {
	for i := range s.Ahead {
		if s.Ahead[i].Txid == txid {
			return &s.Ahead[i]
		}
	}
	return nil
}

// UnsettledTrees are the records of trees the home records nowhere else:
// those a command has yet to ask the node about. A tree minted ahead and
// recorded in Ahead keeps its record until it is adopted, and is settled.
func (s *State) UnsettledTrees() []PendingTree {
	var out []PendingTree
	for _, p := range s.PendingTrees {
		if s.MintedAhead(p.Tree.Txid) == nil {
			out = append(out, p)
		}
	}
	return out
}

// Payment is a payment this identity made for a priced question.
type Payment struct {
	Txid string `json:"txid"`
	// Beef is the payment as the host was handed it, hex.
	Beef string `json:"beef"`
}

// KeepPayments drops every payment whose transaction is not among unmined,
// the transactions whose change the pool still holds unproven: once a
// payment mined, its change is an ordinary coin and the record is done
// with. It reports whether anything was dropped.
func (s *State) KeepPayments(unmined []string) bool {
	n := len(s.Payments)
	s.Payments = slices.DeleteFunc(s.Payments, func(p Payment) bool { return !slices.Contains(unmined, p.Txid) })
	return len(s.Payments) != n
}

// Pending is one object persisted before it is published.
type Pending struct {
	Kind string `json:"kind"`
	// Office is the office whose topic it goes to; Offices, for a sweep,
	// every office it retracts carriers in.
	Office  string   `json:"office,omitempty"`
	Offices []string `json:"offices,omitempty"`
	Txid    string   `json:"txid"`
	// Beef is the object's Atomic BEEF, hex.
	Beef string `json:"beef"`
	// Confirm is what a lookup asks to confirm a host holds it, when the
	// host's answer to the submission admitted nothing: for an envelope
	// the recipient, box and created; for a receipt the acknowledging key
	// and one acknowledged txid; for a sweep one outpoint it spends.
	To       string `json:"to,omitempty"`
	Box      string `json:"box,omitempty"`
	Created  uint64 `json:"created,omitempty"`
	By       string `json:"by,omitempty"`
	AckTxid  string `json:"ackTxid,omitempty"`
	Outpoint string `json:"outpoint,omitempty"`
}

// Sent is an envelope this identity sent.
type Sent struct {
	Txid    string `json:"txid"`
	Office  string `json:"office"`
	To      string `json:"to"`
	Box     string `json:"box"`
	Created uint64 `json:"created"`
	Expires uint64 `json:"expires,omitempty"`
	// Tree and Vout are the funding output the carrier spends: a mined
	// spend of it retracts the envelope.
	Tree string `json:"tree"`
	Vout uint32 `json:"vout"`
	// Paid is the satoshis of a payment inside it, and PaymentTxid the
	// payment transaction, never broadcast by the sender.
	Paid        uint64 `json:"paid,omitempty"`
	PaymentTxid string `json:"paymentTxid,omitempty"`
	// Swept is the sweep that retracts it.
	Swept string `json:"swept,omitempty"`
}

// Receipt is a receipt this identity published.
type Receipt struct {
	Txid   string   `json:"txid"`
	Office string   `json:"office"`
	Acks   []string `json:"acks"`
	Tree   string   `json:"tree"`
	Vout   uint32   `json:"vout"`
}

// Sweep is one mined transaction spending funding-tree outputs: output 0 a
// funding-shaped tombstone, so a host admits it, and every carrier that
// spent one of Vouts retracted once it mines.
type Sweep struct {
	Tree    string   `json:"tree"`
	Vouts   []uint32 `json:"vouts"`
	Offices []string `json:"offices"`
	Txid    string   `json:"txid"`
	// Adopted is set for a spend this home did not build, or built and
	// gave up on: the node named it as the spender of an output, and the
	// home took it as the sweep it is.
	Adopted bool   `json:"adopted,omitempty"`
	RawHex  string `json:"rawHex"`
	// BumpHex and Height are set once it mined.
	BumpHex   string `json:"bumpHex,omitempty"`
	Height    uint32 `json:"height,omitempty"`
	Submitted bool   `json:"submitted,omitempty"`
	// Done is set once it is mined and handed to the outbox.
	Done bool `json:"done,omitempty"`
	// Fee is the pool coin that pays its fee, as the pool held it, so a
	// sweep the network refuses can give it back.
	Fee *bwallet.Output `json:"fee,omitempty"`
	// Failed is why the network refused it, once it did: it is no longer
	// in flight, retracts nothing, and the outputs it named may be swept
	// again.
	Failed string `json:"failed,omitempty"`
}

// Unsettleable is a payment the network refused, and why.
type Unsettleable struct {
	Txid string `json:"txid"`
	Why  string `json:"why"`
}

// IsUnsettleable reports whether txid is a payment recorded as refused.
func (s *State) IsUnsettleable(txid string) bool {
	return slices.ContainsFunc(s.Unsettleable, func(u Unsettleable) bool { return u.Txid == txid })
}

// Received is an envelope to this identity it has read.
type Received struct {
	Txid    string `json:"txid"`
	Office  string `json:"office"`
	From    string `json:"from"`
	Box     string `json:"box"`
	Created uint64 `json:"created"`
	Expires uint64 `json:"expires,omitempty"`
	// Paid is the satoshis the envelope's payment offers, when it passed
	// the recipient's checks; Internalized the payment's txid once taken.
	// Beef is the envelope's carrier, hex, kept while its payment is not
	// taken: once the envelope is acknowledged no free question answers
	// it, and the payment is still the recipient's to take.
	Paid         uint64 `json:"paid,omitempty"`
	Internalized string `json:"internalized,omitempty"`
	Beef         string `json:"beef,omitempty"`
	// Acked is the receipt that acknowledges it.
	Acked string `json:"acked,omitempty"`
}

// ErrNoState is a home with no state file yet.
var ErrNoState = errors.New("state: no state in this home")

// Load reads the home's state, or a new one for identity when the home has
// none yet.
func Load(home, identity string) (*State, error) {
	p := filepath.Join(home, File)
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &State{Version: Version, Identity: identity, path: p}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("state %s: %w", p, err)
	}
	if s.Version != Version {
		return nil, fmt.Errorf("state %s: version %d, this bbox reads %d", p, s.Version, Version)
	}
	if identity != "" && s.Identity != identity {
		return nil, fmt.Errorf("state %s: belongs to identity %s, not this home's %s", p, s.Identity, identity)
	}
	s.path = p
	return &s, nil
}

// Save writes the state atomically at mode 0600.
func (s *State) Save() error {
	if s.beforeSave != nil {
		s.beforeSave()
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(s.path, append(raw, '\n'), 0o600)
}

// AddOffice records an office this home created, once.
func (s *State) AddOffice(office string) {
	if !slices.Contains(s.Offices, office) {
		s.Offices = append(s.Offices, office)
	}
}

// SentByTxid finds a sent envelope.
func (s *State) SentByTxid(txid string) *Sent {
	for i := range s.Sent {
		if s.Sent[i].Txid == txid {
			return &s.Sent[i]
		}
	}
	return nil
}

// ReceivedByTxid finds a received envelope.
func (s *State) ReceivedByTxid(txid string) *Received {
	for i := range s.Received {
		if s.Received[i].Txid == txid {
			return &s.Received[i]
		}
	}
	return nil
}

// Remember records a received envelope, or updates the record it has.
func (s *State) Remember(r Received) *Received {
	if old := s.ReceivedByTxid(r.Txid); old != nil {
		old.Office, old.From, old.Box, old.Created, old.Expires = r.Office, r.From, r.Box, r.Created, r.Expires
		if r.Paid != 0 {
			old.Paid = r.Paid
		}
		if r.Beef != "" && old.Internalized == "" {
			old.Beef = r.Beef
		}
		return old
	}
	s.Received = append(s.Received, r)
	return &s.Received[len(s.Received)-1]
}

// Done removes a published object from the outbox.
func (s *State) Done(txid string) {
	s.Outbox = slices.DeleteFunc(s.Outbox, func(p Pending) bool { return p.Txid == txid })
}

// InFlight is the sweep built and not yet finished, if there is one. A
// sweep the network refused is finished: it failed.
func (s *State) InFlight() *Sweep {
	for i := range s.Sweeps {
		if !s.Sweeps[i].Done && s.Sweeps[i].Failed == "" {
			return &s.Sweeps[i]
		}
	}
	return nil
}

// WriteAtomic writes data to path through a temporary file in the same
// directory, synced and renamed, then syncs the directory, so a crash
// leaves either the old file or the new one.
func WriteAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	if err = f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ErrLocked is a lock another process holds.
var ErrLocked = errors.New("state: locked by another bbox process")

// Lock takes an exclusive lock on path (created at 0600), without waiting.
// The lock is the kernel's and ends with the process, so a crash never
// leaves it held. Release it with the returned function.
func Lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
