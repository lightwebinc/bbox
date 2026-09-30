package main

import (
	"os"

	"bytes"
	"context"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lightwebinc/bbox/internal/limits"
	"github.com/lightwebinc/bbox/internal/send"
	"github.com/lightwebinc/bbox/internal/testchain"
)

func init() {
	poll = 5 * time.Millisecond
	send.Retries = []time.Duration{time.Millisecond}
	limits.Retries = send.Retries
}

// harness is a local chain, two stand-in hosts behind a stand-in plane, a
// terms route on host a, and the environment that points the command at
// them. Each identity is a home of its own.
type harness struct {
	t      *testing.T
	dir    string
	chain  *testchain.Chain
	a, b   *testchain.Host
	chainS *httptest.Server
	aS, bS *httptest.Server
	planeS *httptest.Server
	paid   *testchain.Paid
	paidS  *httptest.Server
	env    map[string]string
	office string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), chain: testchain.New(700)}
	h.a, h.b = testchain.NewHost(h.chain), testchain.NewHost(h.chain)
	h.chainS = httptest.NewServer(h.chain)
	h.aS, h.bS = httptest.NewServer(h.a), httptest.NewServer(h.b)
	h.planeS = httptest.NewServer(&testchain.Plane{Hosts: []http.Handler{h.a, h.b}})
	for _, s := range []*httptest.Server{h.chainS, h.aS, h.bS, h.planeS} {
		t.Cleanup(s.Close)
	}
	h.env = map[string]string{
		"BBOX_NETWORK":    "regtest",
		"BBOX_ASSET":      h.chainS.URL,
		"BBOX_RPC":        h.chainS.URL + "/rpc",
		"BBOX_SETTLE":     "rpc:" + h.chainS.URL + "/rpc",
		"BBOX_FACADE":     h.planeS.URL,
		"BBOX_HOSTS":      h.aS.URL + "," + h.bS.URL,
		"BBOX_HEADER_URL": h.chainS.URL,
		"LANG":            "C.UTF-8",
	}
	return h
}

type result struct {
	code           int
	stdout, stderr string
}

// run runs the command as the identity whose home is name.
func (h *harness) run(name, stdin string, args ...string) result {
	h.t.Helper()
	var out, errb bytes.Buffer
	env := func(k string) string {
		if k == "BBOX_HOME" {
			return filepath.Join(h.dir, name)
		}
		return h.env[k]
	}
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errb, env)
	return result{code, out.String(), errb.String()}
}

// must runs a command that must exit 0.
func (h *harness) must(name, stdin string, args ...string) result {
	h.t.Helper()
	r := h.run(name, stdin, args...)
	if r.code != 0 {
		h.t.Fatalf("%s: bbox %s: exit %d\nstdout:\n%s\nstderr:\n%s", name, strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r
}

func (h *harness) want(r result, code int, has string) {
	h.t.Helper()
	if r.code != code || !strings.Contains(r.stderr+r.stdout, has) {
		h.t.Fatalf("exit %d, want %d with %q\nstdout:\n%s\nstderr:\n%s", r.code, code, has, r.stdout, r.stderr)
	}
}

var (
	identityLine = regexp.MustCompile(`(?m)^identity     ([0-9a-f]{66})$`)
	officeLine   = regexp.MustCompile(`(?m)^office ([a-z_]+)$`)
	sentLine     = regexp.MustCompile(`(?m)^sent    ([0-9a-f]{64})$`)
)

// identity initialises and funds a home and returns its identity key.
func (h *harness) identity(name string) string {
	h.t.Helper()
	r := h.must(name, "", "init")
	id := identityLine.FindStringSubmatch(r.stdout)
	if id == nil {
		h.t.Fatalf("init printed no identity:\n%s", r.stdout)
	}
	h.must(name, "", "fund", "-blocks", "102")
	return id[1]
}

// newOffice creates an office, has both hosts carry it, and configures it.
func (h *harness) newOffice(name string) string {
	h.t.Helper()
	r := h.must(name, "", "office", "new", "post")
	m := officeLine.FindStringSubmatch(r.stdout)
	if m == nil || !strings.Contains(r.stdout, "host   BBOX_OFFICES="+m[1]) {
		h.t.Fatalf("office new:\n%s", r.stdout)
	}
	h.a.Carry(m[1])
	h.b.Carry(m[1])
	h.env["BBOX_OFFICE"] = m[1]
	h.office = m[1]
	return m[1]
}

func (h *harness) send(name, to, text string, extra ...string) string {
	h.t.Helper()
	r := h.must(name, "", append([]string{"send", to, "-m", text, "-rate", "20", "-tree-count", "4"}, extra...)...)
	m := sentLine.FindStringSubmatch(r.stdout)
	if m == nil {
		h.t.Fatalf("send printed no txid:\n%s\n%s", r.stdout, r.stderr)
	}
	return m[1]
}

// startPaid serves host a's terms route, paid to the home name's identity,
// with history priced at price, and configures history_host.
func (h *harness) startPaid(t *testing.T, name string, price uint64) {
	t.Helper()
	keyFile := filepath.Join(h.dir, name+".payee")
	h.must(name, "", "payee", "key", "-out", keyFile)
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ec.PrivateKeyFromHex(strings.TrimSpace(strings.TrimPrefix(string(raw), "BBOX_PAYEE_KEY=")))
	if err != nil {
		t.Fatal(err)
	}
	h.a.Priced = true
	h.paid = testchain.NewPaid(h.a, k, map[string]uint64{"history": price, "history-after": price})
	h.paidS = httptest.NewServer(h.paid)
	t.Cleanup(h.paidS.Close)
	h.env["BBOX_HISTORY_HOST"] = h.paidS.URL
}
