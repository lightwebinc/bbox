package send

import (
	"path/filepath"

	"github.com/lightwebinc/bbox/internal/state"
	"github.com/lightwebinc/bcommon/unicast"
)

// A home is one identity's directory: its wallet (bcommon's bwallet, under
// Profile) and the state a publisher persists every object in before it is
// published. These names let a program outside this module open a home and
// read what the engine returns; the types are bbox's own.
type (
	// State is a home's state: what was sent, acknowledged, swept and
	// read, and what is still to be published.
	State = state.State
	// Sent is one envelope sent, as Engine.Envelope returns it.
	Sent = state.Sent
	// Receipt is one receipt sent, as Engine.Receipt returns it.
	Receipt = state.Receipt
	// Sweep is one retraction, as Engine.Retract returns it.
	Sweep = state.Sweep
	// Unicast is a set of hosts submitted to one by one (Legs.Hosts and
	// Legs.Direct).
	Unicast = unicast.Set
)

// ErrLocked is a home whose lock another process holds.
var ErrLocked = state.ErrLocked

// LockHome takes a home's lock: one process that spends from its pool or
// writes its state at a time. It is taken before the wallet is opened,
// since the wallet's pool is read when it is opened and written back whole.
// The returned function releases it.
func LockHome(dir string) (unlock func(), err error) {
	return state.Lock(filepath.Join(dir, "lock"))
}

// LoadState reads a home's state for the identity (lowercase hex), or
// starts an empty one when the home has none. The caller holds the lock.
func LoadState(dir, identity string) (*State, error) {
	return state.Load(dir, identity)
}
