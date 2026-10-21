package send

import (
	"context"
	"slices"
	"testing"

	"github.com/lightwebinc/bcommon/unicast"

	"github.com/lightwebinc/bbox/internal/state"
	"github.com/lightwebinc/bbox/reader"
)

// On the plane a sweep is sent at once only to the direct hosts no lookup
// asks; one the reader asks is offered it only when it does not answer it
// from the plane (confirmOnPlane).
func TestUnaskedDirectHosts(t *testing.T) {
	sweep := state.Pending{Kind: state.KindSweep}
	ask := unicast.Confirm(func(context.Context, string) (bool, error) { return true, nil })
	direct := &unicast.Set{Hosts: []string{"http://b.example", "http://c.example/"}, Need: 2}
	for _, c := range []struct {
		name    string
		p       state.Pending
		direct  *unicast.Set
		reader  *reader.Client
		confirm unicast.Confirm
		want    []string
	}{
		{"no direct hosts", sweep, nil, &reader.Client{}, ask, nil},
		{"not a sweep", state.Pending{Kind: state.KindEnvelope}, direct, nil, ask, nil},
		{"no reader: every direct host at once", sweep, direct, nil, ask, direct.Hosts},
		{"nothing to ask: every direct host at once", sweep, direct, &reader.Client{Hosts: direct.Hosts}, nil, direct.Hosts},
		{"every direct host asked: none at once", sweep, direct, &reader.Client{Hosts: []string{"http://a.example", "http://b.example/", "http://c.example"}}, ask, nil},
		{"a direct host not asked: it alone at once", sweep, direct, &reader.Client{Hosts: []string{"http://b.example"}}, ask, []string{"http://c.example/"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := &Engine{Legs: Legs{Direct: c.direct, Reader: c.reader}}
			got := e.unasked(c.p, c.confirm)
			if c.want == nil {
				if got != nil {
					t.Fatalf("sent at once to %v, want none", got.Hosts)
				}
				return
			}
			if got == nil || !slices.Equal(got.Hosts, c.want) || got.Need != len(c.want) {
				t.Fatalf("sent at once to %+v, want %v", got, c.want)
			}
		})
	}
}
