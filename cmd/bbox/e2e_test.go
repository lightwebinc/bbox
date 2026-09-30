//go:build e2e

package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lightwebinc/bbox/internal/testchain"
)

// The client end to end: the bbox command against two local reference
// overlay hosts running the bbox host module, over a local chain (package
// testchain: the node's RPC and asset API, and the header source the hosts
// and the command check proofs against). Nothing leaves the machine.
//
//	make e2e-client REFERENCE_HOST=/path/to/reference-host NODE=/path/to/node24
//
// It needs Docker for the hosts' MySQL.

const e2ePassword = "bbox-e2e"

type refHost struct {
	t     *testing.T
	dir   string
	node  string
	name  string
	env   []string
	proc  *exec.Cmd
	out   *bytes.Buffer
	url   string
	terms string
	state string
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func mysql(t *testing.T) int {
	t.Helper()
	id := docker(t, "run", "-d", "--rm", "-e", "MYSQL_ROOT_PASSWORD="+e2ePassword, "-e", "MYSQL_DATABASE=overlay_a",
		"-p", "127.0.0.1::3306", "--tmpfs", "/var/lib/mysql", "mysql:9")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	port := docker(t, "port", id, "3306/tcp")
	port = strings.Split(port, "\n")[0]
	p, _ := strconv.Atoi(port[strings.LastIndex(port, ":")+1:])
	for i := 0; i < 120; i++ {
		if exec.Command("docker", "exec", id, "mysql", "-h127.0.0.1", "-uroot", "-p"+e2ePassword, "-e", "create database if not exists overlay_b").Run() == nil {
			return p
		}
		time.Sleep(time.Second)
	}
	t.Fatal("MySQL did not come up")
	return 0
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func (h *refHost) start() {
	h.t.Helper()
	h.out = &bytes.Buffer{}
	h.proc = exec.Command(h.node, filepath.Join(h.dir, "dist", "index.js"))
	h.proc.Env = h.env
	h.proc.Stdout, h.proc.Stderr = h.out, h.out
	if err := h.proc.Start(); err != nil {
		h.t.Fatal(err)
	}
	for i := 0; i < 120; i++ {
		if resp, err := http.Get(h.url + "/readyz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	h.stop()
	h.t.Fatalf("host %s did not become ready:\n%s", h.name, h.out)
}

func (h *refHost) stop() {
	if h.proc == nil || h.proc.Process == nil {
		return
	}
	_ = h.proc.Process.Signal(syscall.SIGTERM)
	_ = h.proc.Wait()
	h.proc = nil
}

// refHosts stages the bundle as a deployment places it (in a modules/
// directory whose only node_modules is the reference host's), starts one
// MySQL and a reference host per name, each carrying office with its terms
// route pricing history, paid to payeeKey, and returns them by name.
func refHosts(t *testing.T, chainURL, office, payeeKey string, names ...string) map[string]*refHost {
	t.Helper()
	dir := os.Getenv("BBOX_E2E_REFERENCE_HOST")
	node := os.Getenv("BBOX_E2E_NODE")
	if node == "" {
		node = "node"
	}
	raw, err := os.ReadFile("../../host/bundle/bbox-module.js")
	if err != nil {
		t.Fatalf("the host module bundle is not built: %v", err)
	}
	staging := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "node_modules"), filepath.Join(staging, "node_modules")); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(staging, "modules", "bbox", "bbox-module.js")
	if err := os.MkdirAll(filepath.Dir(module), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(module, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	db := mysql(t)
	hosts := map[string]*refHost{}
	for _, name := range names {
		port, terms := freePort(t), freePort(t)
		st := filepath.Join(staging, "state-"+name)
		rh := &refHost{t: t, dir: dir, node: node, name: name, url: fmt.Sprintf("http://127.0.0.1:%d", port),
			terms: fmt.Sprintf("http://127.0.0.1:%d", terms), state: st}
		rh.env = []string{
			"PATH=" + os.Getenv("PATH"),
			"OVERLAY_TOPICS=tm_bbox_" + office,
			"OVERLAY_MODULES=" + module,
			fmt.Sprintf("OVERLAY_KNEX_URL=mysql://root:%s@127.0.0.1:%d/overlay_%s", e2ePassword, db, name),
			"OVERLAY_CHAIN_TRACKER_URL=" + chainURL,
			"OVERLAY_ADMIN_TOKEN=bbox-e2e",
			"OVERLAY_LISTEN=127.0.0.1",
			"OVERLAY_PORT=" + strconv.Itoa(port),
			"BBOX_OFFICES=" + office,
			"BBOX_STATE_DIR=" + st,
			fmt.Sprintf("BBOX_LISTEN=127.0.0.1:%d", terms),
			"BBOX_PRICES=history=5,history-after=5",
			"BBOX_PAYEE_KEY=" + payeeKey,
			"BBOX_SESSIONS=64",
		}
		rh.start()
		t.Cleanup(rh.stop)
		hosts[name] = rh
	}
	return hosts
}

// proxy is a reverse proxy to a host, for the plane stand-in.
func proxy(t *testing.T, base string) http.Handler {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return httputil.NewSingleHostReverseProxy(u)
}

var paidLine = regexp.MustCompile(`(?m)^paid 5 sat to \S+ in ([0-9a-f]{64})`)

func TestClientE2E(t *testing.T) {
	if os.Getenv("BBOX_E2E_REFERENCE_HOST") == "" {
		t.Skip("set BBOX_E2E_REFERENCE_HOST (make e2e-client REFERENCE_HOST=...)")
	}
	h := newHarness(t)
	step := func(what string) { t.Logf("ok  %s", what) }
	h.identity("alice")
	bob := h.identity("bob")
	h.identity("payee")
	office := h.newOffice("bob")
	keyFile := filepath.Join(h.dir, "payee.env")
	h.must("payee", "", "payee", "key", "-out", keyFile)
	rawKey, _ := os.ReadFile(keyFile)
	payeeKey := strings.TrimSpace(strings.TrimPrefix(string(rawKey), "BBOX_PAYEE_KEY="))
	step("init and fund three homes (a sender, a recipient, a host's payee); office new " + office)

	hosts := refHosts(t, h.chainS.URL, office, payeeKey, "a", "b")
	a, b := hosts["a"], hosts["b"]
	if !strings.Contains(a.out.String(), "bbox module mounted") || !strings.Contains(a.out.String(), "@lightwebinc/bcommon") {
		t.Fatalf("host a did not log the bundle's mount line:\n%s", a.out)
	}
	plane := httptest.NewServer(&testchain.Plane{Hosts: []http.Handler{proxy(t, a.url), proxy(t, b.url)}})
	t.Cleanup(plane.Close)
	h.env["BBOX_FACADE"] = plane.URL
	h.env["BBOX_HOSTS"] = a.url + "," + b.url
	h.env["BBOX_HISTORY_HOST"] = a.terms
	step("two reference hosts loading the bundle, each carrying the office, each with a terms route pricing history")

	// Plane: one submit to the facade, which the stand-in plane delivers to
	// both hosts; the recipient reads both and compares.
	hostile := "hello bob, from the plane \x1b[31mred\x1b]0;owned\x07 end"
	tx1 := h.send("alice", bob, hostile)
	r := h.must("bob", "", "list")
	if !strings.Contains(r.stdout, tx1) || !strings.Contains(r.stdout, "["+a.url+", "+b.url+"]") {
		t.Fatalf("list:\n%s\n%s", r.stdout, r.stderr)
	}
	step("send on the plane: one submit; list at both hosts: they agree")
	r = h.must("bob", "", "read", tx1)
	if !strings.Contains(r.stdout, "hello bob, from the plane red end") || strings.ContainsAny(r.stdout, "\x1b\x07") {
		t.Fatalf("read:\n%q", r.stdout)
	}
	step("read: verified at both hosts, decrypted, the hostile escapes filtered")
	r = h.must("bob", "", "ack", tx1)
	if !strings.Contains(r.stdout, "acknowledged 1 envelope(s)") {
		t.Fatalf("ack:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("bob", "", "list")
	if strings.Contains(r.stdout, tx1) {
		t.Fatalf("list after the receipt:\n%s", r.stdout)
	}
	step("ack: the receipt reaches both hosts through the plane, and neither answers the envelope to a free question")

	// Unicast: every object to each host, quorum all, a duplicate confirmed
	// by lookup.
	h.env["BBOX_MODE"] = "unicast"
	delete(h.env, "BBOX_FACADE")
	tx2 := h.send("alice", bob, "hello bob, in unicast")
	r = h.must("bob", "", "list")
	if !strings.Contains(r.stdout, tx2) || !strings.Contains(r.stdout, "["+a.url+", "+b.url+"]") {
		t.Fatalf("list after unicast:\n%s\n%s", r.stdout, r.stderr)
	}
	step("send in unicast: each host took it; both hosts agree on the box")

	// A payment rides inside an envelope; the recipient internalizes it.
	tx3 := h.send("alice", bob, "for the coffee", "-pay", "5000", "payments")
	r = h.must("bob", "", "read", tx3, "-box", "payments")
	m := regexp.MustCompile(`payment  5000 sat in ([0-9a-f]{64})`).FindStringSubmatch(r.stdout)
	if m == nil {
		t.Fatalf("read the payment:\n%s\n%s", r.stdout, r.stderr)
	}
	if h.chain.Tx(m[1]) != nil {
		t.Fatal("the sender broadcast the payment")
	}
	r = h.must("bob", "", "internalize", tx3, "-box", "payments")
	if !strings.Contains(r.stdout, "internalized 5000 sat") || !strings.Contains(r.stdout, "acknowledged "+tx3) || !h.chain.Mined(m[1]) {
		t.Fatalf("internalize:\n%s\n%s", r.stdout, r.stderr)
	}
	step("a payment rides inside an envelope, unbroadcast; the recipient internalizes it (broadcast, mined, pooled) and acknowledges")

	// A priced question: the 402 is paid through BRC-104/105, and the payee
	// settles it.
	r = h.must("bob", "", "terms")
	if !strings.Contains(r.stdout, "history        5 sat a question") {
		t.Fatalf("terms:\n%s", r.stdout)
	}
	r = h.must("bob", "", "history")
	pm := paidLine.FindStringSubmatch(r.stdout)
	if pm == nil || !strings.Contains(r.stdout, tx1) || !strings.Contains(r.stdout, tx3) {
		t.Fatalf("history:\n%s\n%s", r.stdout, r.stderr)
	}
	step("terms, then history: the host answers 402, the command pays it (BRC-104, BRC-105, AuthFetch) and reads the answer")
	r = h.must("payee", "", "payee", "settle", filepath.Join(a.state, "payments.jsonl"))
	if !strings.Contains(r.stdout, "1 payment(s) settled, 5 sat") || !h.chain.Mined(pm[1]) {
		t.Fatalf("payee settle:\n%s\n%s", r.stdout, r.stderr)
	}
	step("payee settle: the host's recorded payment internalized into the payee's pool, broadcast and mined")

	// Drop: a sweep retracts at both hosts.
	tx4 := h.send("alice", bob, "take this back")
	r = h.must("alice", "", "drop", tx4)
	if !strings.Contains(r.stdout, "retracted 1 funding output(s)") {
		t.Fatalf("drop:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, host := range []*refHost{a, b} {
		r = h.must("bob", "", "-hosts", host.url, "list")
		if strings.Contains(r.stdout, tx4) || !strings.Contains(r.stdout, tx2) {
			t.Fatalf("list at %s after the drop:\n%s", host.name, r.stdout)
		}
	}
	step("drop: the sweep mines and reaches both hosts; neither answers the envelope")

	// Restart and restore: host a from its storage and outpoint rows; the
	// command from its home, with a carrier persisted and not published.
	a.stop()
	a.start()
	if !strings.Contains(a.out.String(), "module lookup restored from storage") {
		t.Fatalf("no restore line:\n%s", a.out)
	}
	r = h.must("bob", "", "list")
	if !strings.Contains(r.stdout, tx2) || strings.Contains(r.stdout, tx1) || strings.Contains(r.stdout, tx4) {
		t.Fatalf("list after restore:\n%s\n%s", r.stdout, r.stderr)
	}
	step("host a restarted: its index restored; both hosts still agree (acknowledged and retracted stay unanswered)")
	b.stop()
	h.want(h.run("alice", "", "send", bob, "-m", "while b is down", "-rate", "20"), exitUsage, "1 of 2 host(s) took it and the quorum is 2")
	b.start()
	r = h.must("alice", "", "send", bob, "-m", "after b is back", "-rate", "20")
	if !strings.Contains(r.stderr, "publishing what a previous run persisted") || !strings.Contains(r.stdout, "host "+a.url+": took 2 object(s), missed 0") ||
		!strings.Contains(r.stdout, "host "+b.url+": took 2 object(s), missed 0") {
		t.Fatalf("resume:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("bob", "", "list")
	if strings.Count(r.stdout, "\n") != 3 {
		t.Fatalf("list after the resume:\n%s\n%s", r.stdout, r.stderr)
	}
	step("b down, quorum all: the carrier persisted, exit 2; b back: the next send publishes it first (a answers a duplicate, confirmed by lookup); both agree")

	r = h.must("bob", "", "doctor")
	if !strings.Contains(r.stdout, "host        "+a.url+" answering") || !strings.Contains(r.stdout, "history     "+a.terms+" serves terms") {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
	step("doctor")
	t.Log("e2e client: PASS")
}
