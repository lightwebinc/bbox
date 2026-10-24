package module

import (
	"context"
	"strings"
	"testing"
	"time"

	bmodule "github.com/lightwebinc/bcommon/module"

	"github.com/lightwebinc/bbox/boxrec"
)

// The module as a host mounts it (module.test.ts): its configuration
// refused rather than guessed, and the factory mounting each office and
// ls_bbox with its counters preset, refusing a tm_bbox_ topic it does not
// configure.

const other = "support_mkvbqzxtrw"

// env is a getenv over a map.
func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func with(base map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func TestParseOffices(t *testing.T) {
	got, err := ParseOffices(" " + office + "," + other + " ")
	if err != nil || strings.Join(got, ",") != office+","+other {
		t.Fatal(got, err)
	}
	for _, bad := range []string{"", "example_office", office + "," + office, "Example_office_qzxkvbmwtr", office + ","} {
		if _, err := ParseOffices(bad); err == nil {
			t.Errorf("%q", bad)
		}
	}
}

func TestParseConfig(t *testing.T) {
	base := map[string]string{"BBOX_OFFICES": office, "BBOX_STATE_DIR": "/var/lib/bbox"}
	c, err := ParseConfig(env(base))
	if err != nil {
		t.Fatal(err)
	}
	if c.RetentionDays != 31 || c.MaxBEEF != 262144 || len(c.Prices) != 0 || c.Listen != "" {
		t.Fatal(c)
	}
	if c.JournalSync != 100*time.Millisecond {
		t.Fatal(c.JournalSync)
	}
	for _, x := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"BBOX_OFFICES": office}, "BBOX_STATE_DIR"},
		{with(base, "BBOX_STATE_DIR", "relative"), "BBOX_STATE_DIR"},
		{with(base, "BBOX_RETENTION_DAYS", "30"), "at least 31"},
		{with(base, "BBOX_MAX_BEEF", "1000"), "at least 262144"},
		{with(base, "BBOX_LISTEN", "nowhere"), "host:port"},
		{with(base, "BBOX_LISTEN", "127.0.0.1:0"), "host:port"},
		{with(base, "BBOX_LISTEN", ":8090"), "host:port"},
		{with(base, "BBOX_PAYEE_KEY", strings.Repeat("AB", 32)), "64 lowercase hex"},
		{with(base, "BBOX_PRICES", "history=5,history-after=5"), "BBOX_LISTEN"},
		{with(base, "BBOX_PRICES", "history=5", "BBOX_LISTEN", "127.0.0.1:1", "BBOX_PAYEE_KEY", strings.Repeat("11", 32)), "history and history-after are priced together"},
		{with(base, "BBOX_PRICES", "history=0"), "BBOX_LISTEN"},
		{with(base, "BBOX_RESPONSE_BURST", "0"), "BBOX_RESPONSE_BURST"},
		{with(base, "BBOX_RESPONSES_PER_SEC", "0"), "BBOX_RESPONSES_PER_SEC"},
		{with(base, "BBOX_SESSIONS", "0"), "BBOX_SESSIONS"},
		{with(base, "BBOX_SESSION_TTL", "soon"), "BBOX_SESSION_TTL"},
		{with(base, "BBOX_HANDSHAKES_PER_SEC", "0"), "BBOX_HANDSHAKES_PER_SEC"},
		{with(base, "BBOX_HANDSHAKES_PER_SEC", "-1"), "BBOX_HANDSHAKES_PER_SEC"},
		{with(base, "BBOX_HANDSHAKES_PER_ADDR_PER_SEC", "fast"), "BBOX_HANDSHAKES_PER_ADDR_PER_SEC"},
		{with(base, "BBOX_HANDSHAKE_BURST", "0"), "BBOX_HANDSHAKE_BURST"},
		{with(base, "BBOX_HANDSHAKE_ADDR_BURST", "1.5"), "BBOX_HANDSHAKE_ADDR_BURST"},
		{with(base, "BBOX_JOURNAL_SYNC_MS", "60001"), "BBOX_JOURNAL_SYNC_MS"},
		{with(base, "BBOX_JOURNAL_SYNC_MS", "-1"), "BBOX_JOURNAL_SYNC_MS"},
	} {
		if _, err := ParseConfig(env(x.env)); err == nil || !strings.Contains(err.Error(), x.want) {
			t.Errorf("%v: %v, want %q", x.env, err, x.want)
		}
	}
	priced, err := ParseConfig(env(with(base, "BBOX_PRICES", "history=5,history-after=5", "BBOX_LISTEN", "[::1]:8081",
		"BBOX_PAYEE_KEY", strings.Repeat("11", 32), "OVERLAY_CHAIN_TRACKER_URL", "http://127.0.0.1:1")))
	if err != nil || priced.Listen != "[::1]:8081" || priced.HeadersURL != "http://127.0.0.1:1" || priced.Prices["history"] != 5 || priced.PayeeKey == nil {
		t.Fatal(priced, err)
	}
	if own, _ := ParseConfig(env(with(base, "BBOX_HEADERS_URL", "http://headers.invalid", "OVERLAY_CHAIN_TRACKER_URL", "http://127.0.0.1:1"))); own.HeadersURL != "http://headers.invalid" {
		t.Fatal(own.HeadersURL)
	}
	if c, err := ParseConfig(env(with(base, "BBOX_RETENTION_DAYS", "90", "BBOX_MAX_BEEF", "1048576"))); err != nil || c.RetentionDays != 90 || c.MaxBEEF != 1048576 {
		t.Fatal(c, err)
	}
	if c.Sessions.Max != 10000 || c.Sessions.TTL != 600*time.Second {
		t.Fatal("the BRC-104 session bound has a default", c.Sessions)
	}
	if c.Response.PerSec != 5 || c.Response.Burst != 20 {
		t.Fatal("the budget of signed responses a session has a default", c.Response)
	}
	if r, _ := ParseConfig(env(with(base, "BBOX_RESPONSES_PER_SEC", "0.5", "BBOX_RESPONSE_BURST", "3"))); r.Response.PerSec != 0.5 || r.Response.Burst != 3 {
		t.Fatal(r.Response)
	}
	if s, _ := ParseConfig(env(with(base, "BBOX_SESSIONS", "50", "BBOX_SESSION_TTL", "60"))); s.Sessions.Max != 50 || s.Sessions.TTL != 60*time.Second {
		t.Fatal(s.Sessions)
	}
	if c.Budget.PerSec != 4 || c.Budget.Burst != 8 || c.Budget.PerAddressPerSec != 1 || c.Budget.AddressBurst != 4 {
		t.Fatal("the handshake budget has a default", c.Budget)
	}
	h, err := ParseConfig(env(with(base, "BBOX_HANDSHAKES_PER_SEC", "2.5", "BBOX_HANDSHAKE_BURST", "5", "BBOX_HANDSHAKES_PER_ADDR_PER_SEC", "0.5", "BBOX_HANDSHAKE_ADDR_BURST", "2")))
	if err != nil || h.Budget.PerSec != 2.5 || h.Budget.Burst != 5 || h.Budget.PerAddressPerSec != 0.5 || h.Budget.AddressBurst != 2 {
		t.Fatal(h, err)
	}
	if j, _ := ParseConfig(env(with(base, "BBOX_JOURNAL_SYNC_MS", "0"))); j.JournalSync != 0 {
		t.Fatal("0 syncs every line", j.JournalSync)
	}
}

func TestFactoryMountsEachOfficeAndRefusesAnUnconfiguredTopic(t *testing.T) {
	dir := t.TempDir()
	vars := map[string]string{"BBOX_OFFICES": office + "," + other, "BBOX_STATE_DIR": dir,
		"OVERLAY_TOPICS": "tm_anytx," + boxrec.TopicPrefix + office + "," + boxrec.TopicPrefix + other}
	host := newFakeHost()
	m, err := New(host, env(vars))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Module.Topics) != 2 || m.Module.Topics[topic] == nil || m.Module.Topics[boxrec.TopicPrefix+other] == nil {
		t.Fatal(m.Module.Topics)
	}
	if len(m.Module.Lookups) != 1 || m.Module.Lookups[boxrec.LookupService] == nil {
		t.Fatal(m.Module.Lookups)
	}
	if _, ok := m.Module.Lookups[boxrec.LookupService].(bmodule.Restorer); !ok {
		t.Fatal("the index is in memory, so the module must restore")
	}
	if !host.has("bbox_refused_total", map[string]string{"reason": "beef"}) || host.gauges["bbox_outpoint_rows"] == nil {
		t.Fatal("counters are preset and gauges registered")
	}
	if host.logged("bbox module mounted") != 1 {
		t.Fatal(host.lines)
	}
	mounted := host.lines[len(host.lines)-1].extra
	if mounted["topics"] != topic+","+boxrec.TopicPrefix+other || mounted["terms"] != "none" || mounted["journal_sync"] != "every 100ms" {
		t.Fatal(mounted)
	}
	// Start runs until its context ends, then closes the journal.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Start(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A tm_bbox_ topic the host carries but the module does not configure
	// would fall to the admit-everything default: refused.
	if _, err := New(newFakeHost(), env(with(vars, "OVERLAY_TOPICS", topic+","+boxrec.TopicPrefix+"third_mkvbqzxtrw"))); err == nil || !strings.Contains(err.Error(), "tm_bbox_third_mkvbqzxtrw") {
		t.Fatal(err)
	}
	if _, err := New(newFakeHost(), env(with(vars, "BBOX_OFFICES", ""))); err == nil || !strings.Contains(err.Error(), "BBOX_OFFICES") {
		t.Fatal(err)
	}
	if _, err := New(newFakeHost(), nil); err == nil {
		t.Fatal("no environment")
	}
}
