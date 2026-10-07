// Package config resolves the bbox command's settings: flags, then BBOX_*
// in the environment, then the config file, then the built-in default.
//
// The overlay hosts, the facade and the host a priced question goes to have
// no default: a default there would quietly send a sender's envelopes or a
// reader's questions to somebody else's server, so a command that needs one
// and finds none is a usage error. The chain services do have one on main
// and test, so that no node is needed: WhatsOnChain for headers and the
// chain view, and GorillaPool's public arcade to broadcast. A regtest chain
// has no public services, and names its own.
package config

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"

	"github.com/lightwebinc/bcommon/acceptance"
	"github.com/lightwebinc/bcommon/feepolicy"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"

	"github.com/lightwebinc/bbox/boxrec"
	"github.com/lightwebinc/bbox/internal/limits"
)

// Config is the resolved settings.
type Config struct {
	// AcceptPayerLimit and AcceptTotalLimit bound the satoshis taken on
	// the fast path and not yet mined, per payer and in all; zero is one
	// and ten thresholds. AcceptThresholdSats is the largest payment taken
	// on the network's acceptance; above it a payment waits for its block.
	// AcceptWatch is how long to watch for a conflict before taking one
	// fast, and AcceptWindow how long one counts against the limits unless
	// it mines first (bcommon acceptance.Policy).
	AcceptPayerLimit    uint64
	AcceptThresholdSats uint64
	AcceptTotalLimit    uint64
	AcceptWatch         time.Duration
	AcceptWindow        time.Duration
	// ArcadeKey is the bearer token for an arcade installation in Settle.
	ArcadeKey string
	// Asset is the older spelling of Chain = asset:<Asset>: a node's asset
	// API base URL. It may not be set beside Chain.
	Asset string
	// Chain is the chain view transactions, proofs and spends are read
	// from (bcommon nodeapi.ParseChain): woc:main, woc:test,
	// asset:<node URL>, or a list of them. Empty is woc:<Network> on main
	// and test (ChainSpec).
	Chain string
	// Box is the box an envelope goes to when send names none, and the
	// box list and read ask about when they name none; empty asks about
	// every box.
	Box string
	// Fee is the miner fee policy (bcommon feepolicy.Config): the fee_*
	// keys. Every key unset is mint.DefaultFees, the network's rate.
	Fee feepolicy.Config
	// FundMinedOnly refuses, at fund -beef, a payment that has not mined.
	FundMinedOnly bool
	// Facade is the overlay host a publisher submits to in mode plane.
	Facade string
	// HeaderURL is the header source every proof is checked against: a
	// bridge's /v1 base URL, woc:main, woc:test, bhs:URL,
	// arcade:URL or chaintracks:URL. Empty is woc:<Network> on main and
	// test (Headers). HeaderToken is the bearer token a
	// block-headers-service asks for.
	HeaderToken string
	HeaderURL   string
	// HistoryHost is the base URL of ls_bbox on the host a priced history
	// question is asked of: the host's terms route (spec section 7.4).
	HistoryHost string
	// Home is the identity's directory: key, coin pool and state.
	Home string
	// Hosts are the overlay hosts a reader asks. In mode unicast they are
	// every host a publisher submits to; in mode plane a sweep also goes
	// to each of them directly.
	Hosts []string
	// Mode is how a publisher's objects reach the hosts: "plane" (one
	// submit to Facade, which the plane delivers to every subscribed host)
	// or "unicast" (one submit to each of Hosts).
	Mode string
	// Network is "main", "test" or "regtest": the fund address's prefix
	// and the proof-of-work floor a header source is held to.
	Network string
	// ObjectBound is the largest object the plane takes, in bytes.
	ObjectBound int
	// Office is the office identifier <name>_<suffix> a command uses when
	// it names none.
	Office string
	// Originator is the BRC-100 originator presented to the wallet.
	Originator string
	// Quorum is, in mode unicast, how many of Hosts must take an object
	// for it to count as published: "all", "majority", "one" or a number.
	Quorum string
	// RPC is the node's JSON-RPC URL; RPCUser and RPCPass its basic auth.
	RPC     string
	RPCPass string
	RPCUser string
	// Settle is the settlement leg mined transactions go to (funding
	// trees, sweeps, payments a recipient internalizes), as bcommon
	// publish.ParseSettler reads it: arcade:main, arcade:test,
	// arcade:<url>, arc:<url>, rpc:<url> or tcp:<host:port>. Empty is
	// arcade:<Network> on main and test (SettleSpec).
	Settle string
	// Timeout bounds each request to a host, node or header source.
	Timeout time.Duration
	// TreeCount is how many outputs a new funding tree has.
	TreeCount int
	// WoCKey is a WhatsOnChain API key, and WoCRate the requests a second
	// its plan allows (zero is the free tier's 3).
	WoCKey  string
	WoCRate float64
}

// public reports a network with public chain services.
func (c Config) public() bool { return c.Network == "main" || c.Network == "test" }

// Headers is the header source: HeaderURL, else WhatsOnChain on main and
// test. Empty on regtest with none set.
func (c Config) Headers() string {
	if c.HeaderURL == "" && c.public() {
		return "woc:" + c.Network
	}
	return c.HeaderURL
}

// ChainSpec is the chain view: Chain, else asset:<Asset>, else
// WhatsOnChain on main and test. Empty on regtest with none set.
func (c Config) ChainSpec() string {
	switch {
	case c.Chain != "":
		return c.Chain
	case c.Asset != "":
		return "asset:" + c.Asset
	case c.public():
		return "woc:" + c.Network
	}
	return ""
}

// AssetURL is the node the chain view names, for what only a node does
// (coinbase on a regtest chain): Asset, else the first asset: backend in
// Chain. Empty when there is none.
func (c Config) AssetURL() string {
	if c.Asset != "" {
		return c.Asset
	}
	for _, b := range Backends(c.Chain) {
		if u, ok := strings.CutPrefix(b, "asset:"); ok {
			return u
		}
	}
	return ""
}

// Backends are the backends a chain specification names, each without the
// method it is qualified by (tx=, proof=, spend=, known=).
func Backends(spec string) []string {
	var out []string
	for _, b := range strings.Split(spec, ",") {
		b = strings.TrimSpace(b)
		if i := strings.IndexByte(b, '='); i >= 0 && !strings.Contains(b[:i], ":") {
			b = b[i+1:]
		}
		if b != "" {
			out = append(out, b)
		}
	}
	return out
}

// SettleSpec is the settlement leg: Settle, else GorillaPool's public
// arcade on main and test (publish.DefaultSettle). Empty on regtest with
// none set.
func (c Config) SettleSpec() string {
	if c.Settle != "" {
		return c.Settle
	}
	s, err := publish.DefaultSettle(c.Network)
	if err != nil {
		return ""
	}
	return s
}

// The modes and the named quorums.
const (
	ModePlane      = "plane"
	ModeUnicast    = "unicast"
	QuorumAll      = "all"
	QuorumMajority = "majority"
	QuorumOne      = "one"
)

// AcceptPolicy is the payment acceptance rule the settings name.
func (c Config) AcceptPolicy() acceptance.Policy {
	p := acceptance.DefaultPolicy()
	p.ThresholdSats, p.Window, p.Watch = c.AcceptThresholdSats, c.AcceptWindow, c.AcceptWatch
	p.PayerLimit, p.TotalLimit = c.AcceptPayerLimit, c.AcceptTotalLimit
	if p.PayerLimit == 0 {
		p.PayerLimit = p.ThresholdSats
	}
	if p.TotalLimit == 0 {
		p.TotalLimit = acceptance.DefaultTotalFactor * p.ThresholdSats
	}
	return p
}

// Need is how many of n hosts the quorum q requires. A number above n is an
// error: a quorum no host set can meet would refuse every publish.
func Need(q string, n int) (int, error) {
	if n < 1 {
		return 0, errors.New("no hosts")
	}
	switch q {
	case QuorumAll, "":
		return n, nil
	case QuorumMajority:
		return n/2 + 1, nil
	case QuorumOne:
		return 1, nil
	}
	k, err := strconv.Atoi(q)
	if err != nil || k < 1 {
		return 0, fmt.Errorf("quorum %q is not all, majority, one or a positive number", q)
	}
	if k > n {
		return 0, fmt.Errorf("quorum %d is more than the %d configured host(s)", k, n)
	}
	return k, nil
}

// DefaultHome is ~/.bbox, or .bbox when there is no home directory.
func DefaultHome() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".bbox")
	}
	return ".bbox"
}

// Defaults are the built-in values for the keys that have one.
func Defaults() Config {
	return Config{
		AcceptThresholdSats: acceptance.DefaultThresholdSats,
		AcceptWindow:        acceptance.DefaultWindow,
		Home:                DefaultHome(),
		Mode:                ModePlane,
		Network:             "main",
		ObjectBound:         limits.DefaultObjectBound,
		Originator:          "bbox",
		Quorum:              QuorumAll,
		RPCPass:             "bitcoin",
		RPCUser:             "bitcoin",
		Timeout:             limits.DefaultTimeout,
		TreeCount:           limits.DefaultTreeCount,
	}
}

// Keys is the file grammar's key list, in lexicographic order. A key not in
// it is an error: a misspelt key that was silently ignored would leave a
// setting at its default with no sign that the file was ever read.
var Keys = []string{"accept_payer_limit", "accept_threshold_sats", "accept_total_limit", "accept_watch", "accept_window", "arcade_key", "asset", "box", "chain", "facade", "fee_dust", "fee_floor", "fee_max_rate", "fee_max_tx", "fee_min_rate", "fee_policy_urls", "fee_rate", "fee_source", "fund_mined_only", "header_token", "header_url", "history_host", "home", "hosts", "mode", "network", "object_bound", "office", "originator", "quorum", "rpc", "rpc_pass", "rpc_user", "settle", "timeout", "tree_count", "woc_key", "woc_rate"}

// ErrUnknownKey reports a key the grammar does not define.
var ErrUnknownKey = errors.New("config: unknown key")

// Path is the config file location: the -config flag, else
// $BBOX_HOME/config, else $XDG_CONFIG_HOME/bbox/config, else
// ~/.bbox/config.
func Path(flagPath string, getenv func(string) string) string {
	if flagPath != "" {
		return flagPath
	}
	if h := getenv("BBOX_HOME"); h != "" {
		return filepath.Join(h, "config")
	}
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "bbox", "config")
	}
	return filepath.Join(DefaultHome(), "config")
}

// Parse reads the key = value grammar: one setting per line, '#' comments,
// blank lines ignored, a key at most once.
func Parse(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			return nil, fmt.Errorf("config line %d: not key = value", line)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !known(k) {
			return nil, fmt.Errorf("%w: line %d: %q", ErrUnknownKey, line, k)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("config line %d: %q set twice", line, k)
		}
		out[k] = v
	}
	return out, sc.Err()
}

func known(k string) bool {
	for _, x := range Keys {
		if x == k {
			return true
		}
	}
	return false
}

// Load resolves the file (a missing one is fine) and the environment over
// the defaults. Flags are applied by the caller afterwards with Apply.
func Load(path string, getenv func(string) string) (Config, error) {
	c := Defaults()
	vals := map[string]string{}
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		vals, err = Parse(f)
		if err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	for _, k := range Keys {
		if v := getenv("BBOX_" + strings.ToUpper(k)); v != "" {
			vals[k] = v
		}
	}
	return c.Apply(vals)
}

// Apply sets the given keys the same way for a file, the environment or
// flags, so all three parse alike.
func (c Config) Apply(vals map[string]string) (Config, error) {
	for _, k := range Keys {
		v, ok := vals[k]
		if !ok {
			continue
		}
		switch k {
		case "accept_payer_limit", "accept_threshold_sats", "accept_total_limit":
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return c, fmt.Errorf("config: %s %q is not a whole number of satoshis", k, v)
			}
			switch k {
			case "accept_payer_limit":
				c.AcceptPayerLimit = n
			case "accept_threshold_sats":
				c.AcceptThresholdSats = n
			default:
				c.AcceptTotalLimit = n
			}
		case "accept_watch", "accept_window":
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				return c, fmt.Errorf("config: %s %q is not a duration", k, v)
			}
			if k == "accept_watch" {
				c.AcceptWatch = d
			} else {
				c.AcceptWindow = d
			}
		case "arcade_key":
			c.ArcadeKey = v
		case "asset":
			c.Asset = v
		case "chain":
			if v != "" {
				if _, err := nodeapi.ParseChain(v, nodeapi.ChainOptions{Headers: noHeaders{}}); err != nil {
					return c, fmt.Errorf("config: chain %q: %w", v, err)
				}
			}
			c.Chain = v
		case "fee_dust", "fee_floor", "fee_max_tx":
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return c, fmt.Errorf("config: %s %q is not a whole number of satoshis", k, v)
			}
			switch k {
			case "fee_dust":
				c.Fee.Dust = &n
			case "fee_floor":
				c.Fee.Floor = &n
			default:
				c.Fee.MaxTx = n
			}
		case "fee_max_rate", "fee_min_rate", "fee_rate":
			r, err := mint.ParseRate(v)
			if err != nil {
				return c, fmt.Errorf("config: %s %q is not a rate SATS/BYTES (such as 100/1000): %w", k, v, err)
			}
			switch k {
			case "fee_max_rate":
				c.Fee.MaxRate = &r
			case "fee_min_rate":
				c.Fee.MinRate = &r
			default:
				c.Fee.Rate = &r
			}
		case "fee_policy_urls":
			c.Fee.PolicyURLs = nil
			for _, u := range strings.Split(v, ",") {
				if u = strings.TrimSpace(u); u != "" {
					c.Fee.PolicyURLs = append(c.Fee.PolicyURLs, u)
				}
			}
		case "fee_source":
			if v != feepolicy.SourceStatic && v != feepolicy.SourceARC && v != "arcade" {
				return c, fmt.Errorf("config: fee_source must be static or arc, got %q", v)
			}
			c.Fee.Source = v
		case "fund_mined_only":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return c, fmt.Errorf("config: fund_mined_only %q is not true or false", v)
			}
			c.FundMinedOnly = b
		case "header_token":
			c.HeaderToken = v
		case "box":
			if v != "" && boxrec.CheckBox(v) != nil {
				return c, fmt.Errorf("config: box %q breaks the box name grammar (1 to 50 lowercase letters, digits and single underscores, starting with a letter)", v)
			}
			c.Box = v
		case "facade":
			c.Facade = v
		case "header_url":
			c.HeaderURL = v
		case "history_host":
			c.HistoryHost = v
		case "home":
			c.Home = v
		case "hosts":
			c.Hosts = nil
			for _, h := range strings.Split(v, ",") {
				if h = strings.TrimSpace(h); h != "" {
					c.Hosts = append(c.Hosts, h)
				}
			}
			if len(c.Hosts) > limits.MaxHosts {
				return c, fmt.Errorf("config: hosts names %d hosts, over the limit of %d: a reader asks every host, and a unicast publisher uploads every object to each (docs/limits.md)", len(c.Hosts), limits.MaxHosts)
			}
		case "mode":
			if v != ModePlane && v != ModeUnicast {
				return c, fmt.Errorf("config: mode must be plane or unicast, got %q", v)
			}
			c.Mode = v
		case "network":
			if v != "main" && v != "test" && v != "regtest" {
				return c, fmt.Errorf("config: network must be main, test or regtest, got %q", v)
			}
			c.Network = v
		case "object_bound":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return c, fmt.Errorf("config: object_bound %q is not a positive number of bytes", v)
			}
			if n > limits.MaxObjectBound {
				return c, fmt.Errorf("config: object_bound %d is over the limit of %d: no plane admits a larger object (BRC-149; docs/limits.md)", n, limits.MaxObjectBound)
			}
			c.ObjectBound = n
		case "office":
			if v != "" && boxrec.CheckOffice(v) != nil {
				return c, fmt.Errorf("config: office %q is not an office identifier <name>_<suffix> (bbox office new prints one)", v)
			}
			c.Office = v
		case "originator":
			c.Originator = v
		case "quorum":
			if _, err := Need(v, 1<<30); err != nil {
				return c, fmt.Errorf("config: %w", err)
			}
			c.Quorum = v
		case "rpc":
			c.RPC = v
		case "rpc_pass":
			c.RPCPass = v
		case "rpc_user":
			c.RPCUser = v
		case "settle":
			c.Settle = v
		case "timeout":
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return c, fmt.Errorf("config: timeout %q is not a positive duration", v)
			}
			if d < limits.MinTimeout || d > limits.MaxTimeout {
				return c, fmt.Errorf("config: timeout %s is outside the limits of %s to %s (docs/limits.md)", d, limits.MinTimeout, limits.MaxTimeout)
			}
			c.Timeout = d
		case "woc_key":
			c.WoCKey = v
		case "woc_rate":
			r, err := strconv.ParseFloat(v, 64)
			if err != nil || r <= 0 || r > 1000 {
				return c, fmt.Errorf("config: woc_rate %q is not a number of requests a second from above 0 to 1000", v)
			}
			c.WoCRate = r
		case "tree_count":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > limits.MaxTreeCount {
				return c, fmt.Errorf("config: tree_count %q is not a number of outputs from 1 to %d: every carrier's BEEF carries its whole funding tree (docs/limits.md)", v, limits.MaxTreeCount)
			}
			c.TreeCount = n
		}
	}
	for k := range vals {
		if !known(k) {
			return c, fmt.Errorf("%w: %q", ErrUnknownKey, k)
		}
	}
	if c.Chain != "" && c.Asset != "" {
		return c, errors.New("config: chain and asset are both set; asset is the older spelling of chain = asset:<URL>, so set one")
	}
	if _, err := c.Fee.Fees(mint.DefaultFees); err != nil {
		return c, fmt.Errorf("config: fee: %w", err)
	}
	return c, nil
}

// noHeaders stands in for a header source while a chain specification is
// only parsed: nothing is asked of it.
type noHeaders struct{}

func (noHeaders) IsValidRootForHeight(context.Context, *chainhash.Hash, uint32) (bool, error) {
	return false, errors.New("no header source")
}

func (noHeaders) CurrentHeight(context.Context) (uint32, error) {
	return 0, errors.New("no header source")
}
