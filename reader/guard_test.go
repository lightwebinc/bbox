package reader

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"

	"github.com/lightwebinc/bbox/internal/limits"
)

// A paged walk holds what the contract's pages hold and no more, whatever a
// host answers: a page of thousands of outputs is cut, and said to be cut.
func TestPagesAreBounded(t *testing.T) {
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"output-list","outputs":[`))
		for i := 0; i < 5000; i++ {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			_, _ = w.Write([]byte(`{"beef":[1,2,3],"outputIndex":0}`))
		}
		_, _ = w.Write([]byte(`]}`))
	}))
	defer srv.Close()
	c := &Client{Hosts: []string{srv.URL}, Headers: noHeaders{}}
	ha := c.Pages(context.Background(), srv.URL, Query{"office": "post_abcdefghij", "to": "02" + string(bytes.Repeat([]byte("ab"), 32))}, nil)
	if ha.Err != nil || !ha.Truncated || len(ha.Refused) > pageOutputs || asked != 1 {
		t.Fatalf("a page of 5000 outputs: %d read, truncated %v, %d page(s) asked, %v", len(ha.Refused), ha.Truncated, asked, ha.Err)
	}
}

// An answer over the bound is an error, not read.
func TestAnAnswerIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"output-list","outputs":[]`))
		pad := bytes.Repeat([]byte(" "), 1<<20)
		for i := 0; i <= limits.MaxAnswer>>20; i++ {
			_, _ = w.Write(pad)
		}
		_, _ = w.Write([]byte(`}`))
	}))
	defer srv.Close()
	c := &Client{}
	if _, err := c.Ask(context.Background(), srv.URL, Query{}); err == nil {
		t.Fatalf("an answer over %d bytes was read", limits.MaxAnswer)
	}
}

// A cursor moves a walk on only when it sorts after the one before it.
func TestACursorMustAdvance(t *testing.T) {
	id := string(bytes.Repeat([]byte("ab"), 32))
	other := string(bytes.Repeat([]byte("cd"), 32))
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"2:" + id, "1:" + id, true},
		{"1:" + other, "1:" + id, true},
		{"1:" + id, "1:" + id, false},
		{"1:" + id, "2:" + id, false},
		{"10:" + id, "9:" + id, true},
	} {
		if got := afterCursor(c.a, c.b); got != c.want {
			t.Errorf("afterCursor(%s, %s) = %v", c.a[:4], c.b[:4], got)
		}
	}
}

type noHeaders struct{}

func (noHeaders) IsValidRootForHeight(context.Context, *chainhash.Hash, uint32) (bool, error) {
	return false, nil
}
func (noHeaders) CurrentHeight(context.Context) (uint32, error) { return 0, nil }
