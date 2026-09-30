package unicast

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
)

// beef is the smallest body the facade sends: a BEEF V2 marker.
var beef = func() []byte {
	b := make([]byte, 4)
	v := uint32(transaction.BEEF_V2)
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
	return append(b, 0, 0)
}()

// host answers with admitted outputs, or with a status.
func host(t *testing.T, status int, admitted []int, calls *atomic.Int32) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if status != http.StatusOK {
			http.Error(w, "no", status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{r.Header.Get("x-topics"): map[string]any{"outputsToAdmit": admitted, "coinsToRetain": []int{}}})
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func TestQuorumRetriesAndTallies(t *testing.T) {
	var a, b, c atomic.Int32
	up := host(t, 200, []int{0}, &a)
	dup := host(t, 200, []int{}, &b)
	down := host(t, 503, nil, &c)
	var notes []string
	s := &Set{Hosts: []string{up, dup, down}, Need: 2, Retries: []time.Duration{time.Millisecond, time.Millisecond},
		Note: func(f string, args ...any) { notes = append(notes, f) }}
	o := s.Send(context.Background(), "envelope x", "tm_bbox_post_abcdefghij", beef, 0, nil)
	if !o.OK() || o.Took() != 2 || c.Load() != 3 || a.Load() != 1 {
		t.Fatalf("took %d, down asked %d times: %+v", o.Took(), c.Load(), o)
	}
	if m := o.Missed(); len(m) != 1 || m[0] != down {
		t.Fatalf("missed %v", m)
	}
	o = s.Send(context.Background(), "sweep y", "tm_bbox_post_abcdefghij", beef, 3, nil)
	if o.OK() || !strings.Contains(o.Err().Error(), "2 of 3 host(s) took it and the quorum is 3") {
		t.Fatalf("a sweep reaches every host: %v", o.Err())
	}
	tl := s.Tallies()
	if tl[2].Missed != 2 || tl[2].MissedWhat[1] != "sweep y" || tl[0].Took != 2 {
		t.Fatalf("tallies %+v", tl)
	}
}

func TestAnAnswerThatAdmitsNothingIsConfirmedByLookup(t *testing.T) {
	var n atomic.Int32
	dup := host(t, 200, []int{}, &n)
	s := &Set{Hosts: []string{dup}, Need: 1}
	held := func(context.Context, string) (bool, error) { return true, nil }
	if o := s.Send(context.Background(), "x", "tm_bbox_post_abcdefghij", beef, 0, held); !o.OK() || !o.Results[0].Confirmed {
		t.Fatalf("a duplicate the host holds: %+v", o)
	}
	refused := func(context.Context, string) (bool, error) { return false, nil }
	o := s.Send(context.Background(), "x", "tm_bbox_post_abcdefghij", beef, 0, refused)
	if o.OK() || !strings.Contains(o.Err().Error(), "the host refused it") {
		t.Fatalf("a refusal counted as taken: %+v", o)
	}
}

func TestKeptSendsNothing(t *testing.T) {
	res, err := Kept().Submit(context.Background(), "tm_bbox_kept", beef)
	if err != nil || len(res.Admitted) != 0 || !res.Duplicate {
		t.Fatalf("%+v %v", res, err)
	}
}
