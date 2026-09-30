package reader

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lightwebinc/bbox/internal/boxrec"
)

func item(txid string, created uint64) *Item {
	return &Item{Txid: txid, Admission: &boxrec.Admission{Carrier: &boxrec.Carrier{Envelope: &boxrec.Envelope{Created: created}}}}
}

// A reader that asks several hosts reports a difference, never merging it
// silently (spec section 9).
func TestAListingNamesTheHostThatWithholds(t *testing.T) {
	a := HostAnswer{Host: "a", Items: []*Item{item("t1", 1), item("t2", 2)}}
	b := HostAnswer{Host: "b", Items: []*Item{item("t1", 1)}}
	down := HostAnswer{Host: "c", Err: errors.New("down")}
	l := &Listing{Items: a.Items, Answers: []HostAnswer{a, b, down}}
	m := l.Missing()
	if l.Agree() || len(m) != 1 || len(m["b"]) != 1 || m["b"][0] != "t2" {
		t.Fatalf("missing %v", m)
	}
	if l.Answered() != 2 {
		t.Fatalf("answered %d", l.Answered())
	}
	if l.Find("t2") == nil || l.Find("t3") != nil {
		t.Fatal("find")
	}
	if cursor(item("t2", 2)) != "2:t2" {
		t.Fatal(cursor(item("t2", 2)))
	}
}

func TestVerifyNeedsHeaders(t *testing.T) {
	if _, err := Verify(context.Background(), nil, "post_abcdefghij", boxrec.TxEnvelope, nil, nil); err == nil || !strings.Contains(err.Error(), "no header source") {
		t.Fatalf("verified without headers: %v", err)
	}
}

func TestTermsDocument(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/good/ls_bbox/terms", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"service":"ls_bbox","terms":1,"classes":[{"class":"history","satoshis":5},{"class":"future","satoshis":1}],"extra":true}`))
	})
	mux.HandleFunc("/wrong/ls_bbox/terms", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"service":"ls_log","terms":1}`))
	})
	s := httptest.NewServer(mux)
	defer s.Close()
	tm, err := FetchTerms(context.Background(), s.Client(), s.URL+"/good")
	if err != nil || len(tm.Classes) != 2 || tm.Classes[0].Satoshis != 5 {
		t.Fatalf("%+v %v", tm, err)
	}
	if _, err := FetchTerms(context.Background(), s.Client(), s.URL+"/none"); !errors.Is(err, ErrNoTerms) {
		t.Fatalf("no document: %v", err)
	}
	if _, err := FetchTerms(context.Background(), s.Client(), s.URL+"/wrong"); err == nil {
		t.Fatal("another service's document")
	}
}

func TestHash(t *testing.T) {
	if _, err := Hash(strings.Repeat("AB", 32)); err == nil {
		t.Fatal("uppercase txid")
	}
	h, err := Hash("01" + strings.Repeat("00", 31))
	if err != nil || h[31] != 1 {
		t.Fatalf("%x %v", h, err)
	}
}
