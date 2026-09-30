// Package config resolves the bbox command's settings: flags, then BBOX_*
// in the environment, then the config file, then the built-in default.
//
// The deployment addresses have no default: the overlay hosts, the facade,
// the header source, the settlement leg, the node and the host a priced
// question goes to. A default there would quietly send a sender's envelopes
// or a reader's questions to somebody else's server, so a command that needs
// one and finds none is a usage error.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lightwebinc/bbox/internal/boxrec"
	"github.com/lightwebinc/bbox/internal/limits"
)

// Config is the resolved settings.
type Config struct {
	// ArcadeKey is the bearer token for an arcade installation in Settle.
	ArcadeKey string
	// Asset is the node's asset API base URL: proofs, the chain tip, blocks
	// and raw transactions.
	Asset string
	// Box is the box an envelope goes to when send names none, and the
	// box list and read ask about when they name none; empty asks about
	// every box.
	Box string
	// Facade is the overlay host a publisher submits to in mode plane.
	Facade string
	// HeaderURL is the header source every proof is checked against: a
	// bridge's /v1 base URL, woc:main, woc:test or chaintracks:URL. No
	// default, ever.
	HeaderURL string
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
	// trees, sweeps, payments a recipient internalizes): tcp:<host:port>,
	// rpc:<url> or arcade:<url>.
	Settle string
	// Timeout bounds each request to a host, node or header source.
	Timeout time.Duration
	// TreeCount is how many outputs a new funding tree has.
	TreeCount int
}

// The modes and the named quorums.
const (
	ModePlane      = "plane"
	ModeUnicast    = "unicast"
	QuorumAll      = "all"
	QuorumMajority = "majority"
	QuorumOne      = "one"
)

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
		Home:        DefaultHome(),
		Mode:        ModePlane,
		Network:     "main",
		ObjectBound: limits.DefaultObjectBound,
		Originator:  "bbox",
		Quorum:      QuorumAll,
		RPCPass:     "bitcoin",
		RPCUser:     "bitcoin",
		Timeout:     limits.DefaultTimeout,
		TreeCount:   limits.DefaultTreeCount,
	}
}

// Keys is the file grammar's key list, in lexicographic order. A key not in
// it is an error: a misspelt key that was silently ignored would leave a
// setting at its default with no sign that the file was ever read.
var Keys = []string{"arcade_key", "asset", "box", "facade", "header_url", "history_host", "home", "hosts", "mode", "network", "object_bound", "office", "originator", "quorum", "rpc", "rpc_pass", "rpc_user", "settle", "timeout", "tree_count"}

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
		case "arcade_key":
			c.ArcadeKey = v
		case "asset":
			c.Asset = v
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
	return c, nil
}
