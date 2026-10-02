package limits

import (
	"fmt"
	"time"

	"github.com/lightwebinc/bbox/internal/boxrec"
)

// The policy limits of docs/limits.md, as the bbox command applies them. None
// is a wire format or a signed bound (docs/frozen.md): the record bounds
// every reader accepts are boxrec's, and these are what a client chooses to
// send and to ask, so a later version may change any of them without a new
// record version. A cap refuses the setting before anything is sent, with a
// message naming the setting and the limit; a warning is printed and the
// command goes on.

// The plane's packets: an object larger than one is carried as fragments of
// FragmentPayload bytes (the IPv6 minimum MTU of 1280 less the IPv6, UDP and
// object fragment headers).
const (
	FragmentPayload = 1128
	// PlaneContentWarn is the envelope content above which a sender on the
	// plane is warned: with the default tree its carrier is 8 packets, the
	// size at which no object was lost in the measurements.
	PlaneContentWarn = 6144
	// OpenObjectBound is BRC-149's bound on an object submitted on the open
	// path; MaxObjectBound the authenticated path's. No plane admits more.
	OpenObjectBound = 1 << 20
	MaxObjectBound  = 8 << 20
	// DefaultObjectBound is the object bound assumed when none is set.
	DefaultObjectBound = OpenObjectBound
)

// Funding trees.
const (
	DefaultTreeCount   = 32
	MaxTreeCount       = 1000
	PlaneTreeCountWarn = 100
	// DefaultTreeSats is each funding output's value. A carrier carries
	// its funding output's whole value and is never mined, so one satoshi
	// is enough.
	DefaultTreeSats = 1
	// DefaultAhead is how few outputs a tree has left when the next is
	// minted ahead.
	DefaultAhead = 4
)

// Sending.
const (
	// DefaultRate and MaxRate bound the envelopes a sender sends per
	// second, per office.
	DefaultRate = 1.0
	MaxRate     = 20.0
	// PaymentExpires is the default life of an envelope that carries a
	// payment: the recipient internalizes it only until an hour before
	// expires, and the sender reclaims it only an hour after.
	PaymentExpires = 24 * time.Hour
	// MaxExpires is the longest life an envelope is given: the answer
	// window, after which no free question answers it anyway.
	MaxExpires = 30 * 24 * time.Hour
	// MaxRefs is the most references one message carries (the record
	// bound, boxrec.MaxRefs).
	MaxRefs = boxrec.MaxRefs
)

// Hosts, quorum and requests.
const (
	MaxHosts         = 16
	UnicastHostsWarn = 5
	MinTimeout       = time.Second
	MaxTimeout       = 5 * time.Minute
	DefaultTimeout   = 15 * time.Second
)

// Retries are the waits between the tries of a submission: four tries,
// then the host is reported missed and the object stays persisted.
var Retries = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// PlaneWait are the waits between a publisher's lookups for an object it
// submitted to the plane, at each host it names, before it offers the
// object to that host directly: the plane delivers in well under a second.
var PlaneWait = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}

// Reading.
const (
	// PageSize is the most envelopes one answer page holds (the spec's).
	PageSize = 64
	// MaxPages is the most pages one list reads per host: 4096 envelopes.
	MaxPages = 64
	// DefaultMaxPrice is the most a priced question is paid without
	// -max-sats: a price is per question, and a host that asks more than
	// this is asked by the user, not paid by default.
	DefaultMaxPrice = 1000
	// DefaultBudget is the most a command that asks several priced
	// questions pays in all without -budget.
	DefaultBudget = 16 * DefaultMaxPrice
	// MaxAnswer is the most bytes a reader takes for one lookup answer: a
	// full page of the largest envelopes fits several times over.
	MaxAnswer = 16 << 20
)

// Settling. DefaultSettleInFlight and MaxSettleInFlight bound the payments
// one payee settle run has broadcast and not yet seen mined: each is a
// proof poll against the node until its block.
const (
	DefaultSettleInFlight = 16
	MaxSettleInFlight     = 64
)

// Send is a sender's settings, as the checks see them.
type Send struct {
	Plane       bool
	Hosts       int
	Need        int
	TreeCount   int
	ObjectBound int
	Content     int
	Refs        int
	Expires     time.Duration
}

func over(what string, v, limit any, why string) error {
	return fmt.Errorf("%s %v is over the limit of %v: %s (docs/limits.md)", what, v, limit, why)
}

// Check applies the caps, returning the first broken as an error, and the
// recommendations, returning each passed as a warning.
func (s Send) Check() (warnings []string, err error) {
	switch {
	case s.Content > boxrec.MaxContent:
		return nil, over("the envelope content", s.Content, boxrec.MaxContent, "the record bound every reader accepts; larger content goes by reference (-ref)")
	case s.Refs > MaxRefs:
		return nil, over("-ref", s.Refs, MaxRefs, "the references one message carries")
	case s.TreeCount < 1:
		return nil, fmt.Errorf("-tree-count %d is under the limit of 1: a funding tree has at least one output (docs/limits.md)", s.TreeCount)
	case s.TreeCount > MaxTreeCount:
		return nil, over("-tree-count", s.TreeCount, MaxTreeCount, "every carrier's BEEF carries its whole funding tree")
	case s.ObjectBound > MaxObjectBound:
		return nil, over("object_bound", s.ObjectBound, MaxObjectBound, "no plane admits a larger object (BRC-149)")
	case s.Hosts > MaxHosts:
		return nil, over("hosts", s.Hosts, MaxHosts, "a unicast sender uploads every object to each host, and a reader asks each")
	case s.Expires > MaxExpires:
		return nil, over("-expires", s.Expires, MaxExpires, "no free question answers an envelope outside the 30-day answer window")
	}
	if s.Plane {
		if s.Content > PlaneContentWarn {
			warnings = append(warnings, fmt.Sprintf("the envelope content is %d bytes; on the plane a carrier over %d bytes of content is more than 8 packets, which are lost more often under loss and distance (reference larger content with -ref; docs/limits.md)", s.Content, PlaneContentWarn))
		}
		if s.TreeCount > PlaneTreeCountWarn {
			warnings = append(warnings, fmt.Sprintf("-tree-count %d on the plane adds about %d bytes of funding tree to every carrier; %d is the default (docs/limits.md)", s.TreeCount, s.TreeCount*49, DefaultTreeCount))
		}
	} else {
		if s.Hosts > UnicastHostsWarn {
			warnings = append(warnings, fmt.Sprintf("%d hosts in mode unicast: every object is uploaded %d times; beyond %d hosts the plane is what removes that cost (docs/limits.md)", s.Hosts, s.Hosts, UnicastHostsWarn))
		}
		if s.Need > 0 && s.Need < s.Hosts/2+1 {
			warnings = append(warnings, fmt.Sprintf("a quorum of %d of %d hosts is below a majority: an object counts as published while most hosts lack it (docs/limits.md)", s.Need, s.Hosts))
		}
	}
	return warnings, nil
}

// Gap is the least time between two envelopes at rate per second.
func Gap(rate float64) time.Duration {
	if rate <= 0 {
		return 0
	}
	return time.Duration(float64(time.Second) / rate)
}
