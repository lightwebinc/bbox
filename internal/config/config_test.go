package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lightwebinc/bcommon/mint"
)

func TestFileEnvironmentAndFlagsLayer(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config")
	file := "# a reader\nfacade = http://192.0.2.10:8080\nhosts = http://192.0.2.10:8080, http://192.0.2.11:8080\nnetwork = test\ntimeout = 5s\noffice = post_abcdefghij\n"
	if err := os.WriteFile(p, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"BBOX_NETWORK": "regtest", "BBOX_OBJECT_BOUND": "2048", "BBOX_BOX": "payments"}
	c, err := Load(p, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Facade != "http://192.0.2.10:8080" || len(c.Hosts) != 2 || c.Hosts[1] != "http://192.0.2.11:8080" ||
		c.Network != "regtest" || c.Timeout != 5*time.Second || c.ObjectBound != 2048 || c.Originator != "bbox" ||
		c.Office != "post_abcdefghij" || c.Box != "payments" || c.TreeCount != 32 {
		t.Fatalf("%+v", c)
	}
	c, err = c.Apply(map[string]string{"network": "main"})
	if err != nil || c.Network != "main" {
		t.Fatalf("flag over environment: %v %s", err, c.Network)
	}
	if _, err := Load(filepath.Join(dir, "absent"), func(string) string { return "" }); err != nil {
		t.Fatalf("a missing file is not an error: %v", err)
	}
}

func TestTheGrammarRefuses(t *testing.T) {
	for _, bad := range []string{"facde = x\n", "no equals\n", "facade = a\nfacade = b\n"} {
		if _, err := Parse(strings.NewReader(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, err := Parse(strings.NewReader("facde = x\n")); !errors.Is(err, ErrUnknownKey) {
		t.Error("an unknown key is not ErrUnknownKey")
	}
	for k, v := range map[string]string{"network": "mainnet", "timeout": "-1s", "object_bound": "0", "office": "post",
		"box": "Inbox", "tree_count": "1001", "hosts": strings.Repeat("http://h,", 17), "timeout ": "1s"} {
		if _, err := Defaults().Apply(map[string]string{k: v}); err == nil {
			t.Errorf("%s = %s applied", k, v)
		}
	}
	if !slices.IsSorted(Keys) {
		t.Error("Keys is not in lexicographic order")
	}
}

func TestPath(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if Path("/x/c", get) != "/x/c" {
		t.Error("the flag wins")
	}
	env["XDG_CONFIG_HOME"] = "/xdg"
	if Path("", get) != "/xdg/bbox/config" {
		t.Error(Path("", get))
	}
	env["BBOX_HOME"] = "/h"
	if Path("", get) != "/h/config" {
		t.Error(Path("", get))
	}
}

func TestModeAndQuorum(t *testing.T) {
	c := Defaults()
	if c.Mode != ModePlane || c.Quorum != QuorumAll {
		t.Fatalf("defaults: mode %q quorum %q", c.Mode, c.Quorum)
	}
	c, err := c.Apply(map[string]string{"mode": "unicast", "quorum": "majority"})
	if err != nil || c.Mode != ModeUnicast || c.Quorum != QuorumMajority {
		t.Fatalf("%v %+v", err, c)
	}
	for k, v := range map[string]string{"mode": "multicast", "quorum": "most"} {
		if _, err := Defaults().Apply(map[string]string{k: v}); err == nil {
			t.Errorf("%s = %s applied", k, v)
		}
	}
	for q, want := range map[string]int{"all": 3, "": 3, "majority": 2, "one": 1, "2": 2} {
		if n, err := Need(q, 3); err != nil || n != want {
			t.Errorf("Need(%q, 3) = %d %v, want %d", q, n, err, want)
		}
	}
	if _, err := Need("4", 3); err == nil {
		t.Error("a quorum above the host count")
	}
	if _, err := Need("all", 0); err == nil {
		t.Error("no hosts")
	}
}

// TestNoNodeDefaults: on main and test the chain services default to the
// public ones, so that no node is needed; a regtest chain names its own.
// The older asset key maps to chain = asset:URL and may not sit beside it.
func TestNoNodeDefaults(t *testing.T) {
	for _, n := range []string{"main", "test"} {
		c, err := Defaults().Apply(map[string]string{"network": n})
		if err != nil {
			t.Fatal(err)
		}
		if c.Headers() != "woc:"+n || c.ChainSpec() != "woc:"+n || c.SettleSpec() != "arcade:"+n || c.AssetURL() != "" {
			t.Errorf("%s: headers %q chain %q settle %q asset %q", n, c.Headers(), c.ChainSpec(), c.SettleSpec(), c.AssetURL())
		}
	}
	c, err := Defaults().Apply(map[string]string{"network": "regtest"})
	if err != nil || c.Headers() != "" || c.ChainSpec() != "" || c.SettleSpec() != "" {
		t.Errorf("regtest has defaults: %v %q %q %q", err, c.Headers(), c.ChainSpec(), c.SettleSpec())
	}
	c, err = Defaults().Apply(map[string]string{"asset": "http://192.0.2.1:8090"})
	if err != nil || c.ChainSpec() != "asset:http://192.0.2.1:8090" || c.AssetURL() != "http://192.0.2.1:8090" {
		t.Errorf("asset: %v %q %q", err, c.ChainSpec(), c.AssetURL())
	}
	c, err = Defaults().Apply(map[string]string{"chain": "woc:test,spend=asset:http://192.0.2.1:8090"})
	if err != nil || c.AssetURL() != "http://192.0.2.1:8090" {
		t.Errorf("chain with a node's spend view: %v %q", err, c.AssetURL())
	}
	if _, err := Defaults().Apply(map[string]string{"chain": "woc:main", "asset": "http://192.0.2.1:8090"}); err == nil {
		t.Error("chain and asset together applied")
	}
	if _, err := Defaults().Apply(map[string]string{"chain": "node:x"}); err == nil {
		t.Error("a chain that does not parse applied")
	}
}

// TestFeeKeys: the fee_* keys make bcommon's fee block; unset, the
// network's rate.
func TestFeeKeys(t *testing.T) {
	c, err := Defaults().Apply(map[string]string{"fee_rate": "50/1000", "fee_floor": "1", "fee_dust": "1",
		"fee_max_rate": "1/1", "fee_max_tx": "100000", "fee_source": "arc", "fee_policy_urls": "https://arc.example, https://arc2.example"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := c.Fee.Fees(mint.DefaultFees)
	if err != nil || f.Rate != (mint.Rate{Sats: 50, Bytes: 1000}) || f.Floor != 1 || f.Dust != 1 || f.Max != 100000 ||
		c.Fee.Source != "arc" || len(c.Fee.PolicyURLs) != 2 {
		t.Fatalf("%v %+v %+v", err, f, c.Fee)
	}
	d := Defaults()
	f, err = d.Fee.Fees(mint.DefaultFees)
	if err != nil || f.Rate != mint.DefaultFees.Rate || f.Floor != mint.DefaultFees.Floor {
		t.Fatalf("default fees: %v %+v", err, f)
	}
	for k, v := range map[string]string{"fee_rate": "1/0", "fee_source": "live", "fee_floor": "-1", "fund_mined_only": "maybe", "woc_rate": "0"} {
		if _, err := Defaults().Apply(map[string]string{k: v}); err == nil {
			t.Errorf("%s = %s applied", k, v)
		}
	}
}
