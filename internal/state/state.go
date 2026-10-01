// Package state is an identity's durable record in its home: the funding
// trees its carriers spend, the carriers persisted before they are
// published, what it sent and acknowledged, the sweeps it built, the
// envelopes it read and the payments it took.
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

	path string
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
	RawHex  string   `json:"rawHex"`
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
