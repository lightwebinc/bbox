package module

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	bmodule "github.com/lightwebinc/bcommon/module"

	"github.com/lightwebinc/bbox/boxrec"
)

// Reasons are every label a host counts a refused submission by (spec
// sections 3.1, 4.6, 5 and 8.1). The record rule magic is not among them:
// an output claims a record only by its magic, so admission never reaches
// it.
var Reasons = []string{
	"too-large", "cbor", "key-type", "missing", "type", "range", "identity", "office", "box", "expires", "acks",
	"content-json", "content-shape", "content-cipher", "content-signature",
	"carrier-shape", "beef", "mineable", "unlock", "lock", "signature", "funding", "unmined", "not-bbox",
}

// AdmitKinds are what an admitted transaction was.
var AdmitKinds = []string{"envelope", "receipt", "sweep", "spend"}

// RecentAdmissions is how many admitted txids a manager remembers to count
// each once.
const RecentAdmissions = 4096

// Refused is raised for every refusal. The engine counts it as a failed
// topic for this submission and stores nothing.
type Refused struct {
	Reason string
	Detail string
}

func (r *Refused) Error() string { return "bbox: refused, " + r.Reason + ": " + r.Detail }

// ErrNoSubmission is a host that hands the manager a parsed BEEF without
// the submission's bytes (bcommon module.WithSubmission): the manager
// decides on the wire form (the counts a BEEF declares, which proofs it
// carries), which a parsed and merged BEEF no longer shows.
var ErrNoSubmission = errors.New("bbox: the host did not hand the manager the submission's bytes (bcommon module.WithSubmission)")

// EngineVerified is the chain tracker the managers read a sweep's proof
// with. The engine has verified a submission's proofs against the host's
// headers before it calls a topic manager; what the sweep rule still needs
// is that the proof is present and names the transaction, which
// MerklePath.Verify checks before it asks a tracker. This tracker answers
// yes to what the engine already checked, as the TypeScript module's does.
type EngineVerified struct{}

var _ chaintracker.ChainTracker = EngineVerified{}

// IsValidRootForHeight implements chaintracker.ChainTracker.
func (EngineVerified) IsValidRootForHeight(context.Context, *chainhash.Hash, uint32) (bool, error) {
	return true, nil
}

// CurrentHeight implements chaintracker.ChainTracker.
func (EngineVerified) CurrentHeight(context.Context) (uint32, error) { return 0, nil }

// TopicManager is tm_bbox_<name>_<suffix>: the topic manager of one office
// (spec section 8.1), one for each configured office. The rules are
// boxrec.Admit's; this is the engine seam: it runs them on the submission
// and turns the verdict into admittance instructions.
//
// The engine records every transaction a topic manager returns
// instructions for as applied, empty instructions included: a later
// submission of the same txid is a duplicate no topic manager is shown
// again, and every held output it spends is marked spent. A refusal
// returned as empty instructions would therefore be permanent for that
// txid; anyone could suppress a genuine carrier by submitting it first in a
// different BEEF. So every refusal is raised, and the engine records
// nothing and marks nothing spent: a refused transaction offered again is
// decided again.
type TopicManager struct {
	Topic  string
	Office string
	host   bmodule.Host
	h      boxrec.Host

	mu     sync.Mutex
	recent map[string]bool
	order  []string
}

var _ bmodule.TopicManager = (*TopicManager)(nil)

// NewTopicManager makes the manager of one topic. headers nil reads a
// sweep's proof as EngineVerified does; maxBEEF zero is the floor.
func NewTopicManager(topic string, host bmodule.Host, headers chaintracker.ChainTracker, maxBEEF int) (*TopicManager, error) {
	if !strings.HasPrefix(topic, boxrec.TopicPrefix) {
		return nil, fmt.Errorf("bbox: %s is not a tm_bbox_ topic", topic)
	}
	office := topic[len(boxrec.TopicPrefix):]
	if err := boxrec.CheckOffice(office); err != nil {
		return nil, err
	}
	if maxBEEF == 0 {
		maxBEEF = boxrec.DefaultMaxBEEF
	}
	if maxBEEF < boxrec.DefaultMaxBEEF {
		return nil, fmt.Errorf("bbox: a BEEF bound of %d is under the %d-byte floor", maxBEEF, boxrec.DefaultMaxBEEF)
	}
	if headers == nil {
		headers = EngineVerified{}
	}
	return &TopicManager{Topic: topic, Office: office, host: host, h: boxrec.Host{Office: office, Headers: headers, MaxBEEF: maxBEEF}, recent: map[string]bool{}}, nil
}

// Admit applies the rules to the submission's bytes with the held inputs,
// as the TypeScript manager's identifyAdmissibleOutputs does.
func (t *TopicManager) Admit(ctx context.Context, raw []byte, held []int) (overlay.AdmittanceInstructions, error) {
	a, err := boxrec.Admit(ctx, raw, held, t.h)
	if err != nil {
		reason := boxrec.Reason(err)
		if reason == "other" {
			return overlay.AdmittanceInstructions{}, err
		}
		t.host.Metrics().Inc("bbox_refused_total", map[string]string{"reason": reason}, 1)
		detail := strings.TrimPrefix(err.Error(), "boxrec: ")
		t.host.Log("tm_bbox refused", map[string]any{"topic": t.Topic, "reason": reason, "detail": detail})
		return overlay.AdmittanceInstructions{}, &Refused{Reason: reason, Detail: detail}
	}
	t.count(a.Kind.String(), hex.EncodeToString(a.Txid[:]))
	ai := overlay.AdmittanceInstructions{OutputsToAdmit: make([]uint32, 0, len(a.Outputs)), CoinsToRetain: make([]uint32, 0, len(a.Retain))}
	for _, o := range a.Outputs {
		ai.OutputsToAdmit = append(ai.OutputsToAdmit, uint32(o)) //nolint:gosec // bounded by the transaction's outputs
	}
	for _, r := range a.Retain {
		ai.CoinsToRetain = append(ai.CoinsToRetain, uint32(r)) //nolint:gosec // bounded by the transaction's inputs
	}
	return ai, nil
}

// IdentifyAdmissibleOutputs implements bcommon's module.TopicManager over
// the submission's bytes in ctx.
func (t *TopicManager) IdentifyAdmissibleOutputs(ctx context.Context, _ *transaction.Beef, _ *chainhash.Hash, previousCoins []uint32) (overlay.AdmittanceInstructions, error) {
	raw, ok := bmodule.Submission(ctx)
	if !ok {
		return overlay.AdmittanceInstructions{}, ErrNoSubmission
	}
	held := make([]int, 0, len(previousCoins))
	for _, c := range previousCoins {
		held = append(held, int(c))
	}
	return t.Admit(ctx, raw, held)
}

// count counts an admission once per transaction. The engine shows a
// manager a txid it already applied only when two submissions of it race
// past its duplicate check (in the lab: a sweep a client sends to the
// facade and then directly to the same host), so bbox_admitted_total
// counts distinct transactions and bbox_admitted_repeats_total the rest.
// The memory is the last RecentAdmissions txids.
func (t *TopicManager) count(kind, txid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.recent[txid] {
		t.host.Metrics().Inc("bbox_admitted_repeats_total", map[string]string{"kind": kind}, 1)
		return
	}
	t.recent[txid] = true
	t.order = append(t.order, txid)
	if len(t.order) > RecentAdmissions {
		delete(t.recent, t.order[0])
		t.order = t.order[1:]
	}
	t.host.Metrics().Inc("bbox_admitted_total", map[string]string{"kind": kind}, 1)
}

// GetDocumentation implements module.TopicManager.
func (t *TopicManager) GetDocumentation() string {
	return strings.Join([]string{
		"# " + t.Topic,
		"",
		"The bbox office " + t.Office + ": envelope and receipt carriers, each spending one",
		"output of its owner's mined funding tree, and the tombstones of mined sweeps,",
		"each admitted on its own validity (spec section 8.1). A transaction that spends",
		"held outputs and claims nothing is accepted for its inputs alone. Every refusal",
		"is raised, so nothing refused is recorded.",
	}, "\n")
}

// GetMetaData implements module.TopicManager.
func (t *TopicManager) GetMetaData() *overlay.MetaData {
	return &overlay.MetaData{Name: t.Topic, Description: "Admits the bbox office " + t.Office + "."}
}
