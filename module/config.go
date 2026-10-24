// Package module is the bbox overlay module in Go: tm_bbox_<name>_<suffix>
// for each configured office and ls_bbox for all of them, written to
// bcommon's engine-neutral contract (github.com/lightwebinc/bcommon/module)
// so that any Go overlay host that carries the adapter mounts it. It is the
// twin of the TypeScript module under host/src (frozen), built from the Go
// core: the admission rules are boxrec.Admit, the questions boxrec's
// classes, and the index the TypeScript module's statuses, rows and
// retention, with its journal byte for byte.
//
// Configuration is the TypeScript module's, read from the environment when
// the host calls the factory (docs/host.md): BBOX_OFFICES, BBOX_STATE_DIR,
// BBOX_RETENTION_DAYS, BBOX_MAX_BEEF, BBOX_LISTEN, BBOX_PRICES,
// BBOX_PAYEE_KEY, BBOX_HEADERS_URL (OVERLAY_CHAIN_TRACKER_URL when unset),
// BBOX_SESSIONS, BBOX_SESSION_TTL, the handshake and response budgets,
// BBOX_ARCADE_URL, BBOX_ASSET_URL, BBOX_CHAIN, BBOX_WOC_KEY and the
// BBOX_ACCEPT_* rule, and one of its own, BBOX_JOURNAL_SYNC_MS (the rows
// journal's group commit window). Every mistake is an error, and the host
// refuses to start.
package module

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/bcommon/termsroute"

	"github.com/lightwebinc/bbox/boxrec"
)

// The rows journal's group commit window (BBOX_JOURNAL_SYNC_MS): lines are
// written at once and fsynced together this often, off the lookup
// service's lock; 0 fsyncs each line before the admission goes on, as the
// TypeScript module does. The default and the bound are a timed
// write-ahead log's usual ones.
const (
	DefaultJournalSyncMs = 100
	MaxJournalSyncMs     = 60_000
)

// Config is the module's configuration.
type Config struct {
	Offices       []string
	StateDir      string
	RetentionDays int
	MaxBEEF       int
	// JournalSync is the rows journal's group commit window; 0 syncs every
	// line.
	JournalSync time.Duration
	// Listen is the terms route's host:port; empty for none.
	Listen   string
	Prices   map[string]uint64
	PayeeKey *ec.PrivateKey
	// HeadersURL is the header source payments verify against; empty for
	// none.
	HeadersURL string
	Sessions   termsroute.SessionBound
	Budget     termsroute.BudgetConfig
	Response   termsroute.ResponseBudgetConfig
	Accept     termsroute.AcceptConfig
}

// Classes are the lookup classes as the route prices them.
var Classes = func() []termsroute.PricedClass {
	out := make([]termsroute.PricedClass, len(boxrec.Classes))
	for i, c := range boxrec.Classes {
		out[i] = termsroute.PricedClass{Name: c.Name, Free: c.Free}
	}
	return out
}()

// ParseOffices parses BBOX_OFFICES: office identifiers, once each.
func ParseOffices(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("BBOX_OFFICES is required: name each office this host carries")
	}
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		office := strings.TrimSpace(entry)
		if boxrec.CheckOffice(office) != nil {
			return nil, fmt.Errorf("BBOX_OFFICES: %q is not an office identifier <name>_<suffix>", office)
		}
		for _, x := range out {
			if x == office {
				return nil, fmt.Errorf("BBOX_OFFICES names %s twice", office)
			}
		}
		out = append(out, office)
	}
	return out, nil
}

var digits = regexp.MustCompile(`^[0-9]+$`)
var rateRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
var payeeRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func integer(name, raw string, def, min int) (int, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if !digits.MatchString(v) || err != nil || n > termsroute.MaxSafe || n < int64(min) {
		return 0, fmt.Errorf("%s: %q is not an integer of at least %d", name, raw, min)
	}
	return int(n), nil
}

// rate is a positive rate, decimals allowed, at most 1e6.
func rate(name, raw string, def float64) (float64, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if !rateRE.MatchString(v) || err != nil || !(f > 0) || f > 1e6 {
		return 0, fmt.Errorf("%s: %q is not a positive number of at most 1000000", name, raw)
	}
	return f, nil
}

// ParseConfig reads the whole configuration through getenv.
func ParseConfig(getenv func(string) string) (*Config, error) {
	offices, err := ParseOffices(getenv("BBOX_OFFICES"))
	if err != nil {
		return nil, err
	}
	c := &Config{Offices: offices, StateDir: getenv("BBOX_STATE_DIR")}
	if c.StateDir == "" || !filepath.IsAbs(c.StateDir) {
		return nil, errors.New("BBOX_STATE_DIR is required: an absolute directory kept for the host's life, where the outpoint rows live")
	}
	if c.RetentionDays, err = integer("BBOX_RETENTION_DAYS", getenv("BBOX_RETENTION_DAYS"), FloorDays, FloorDays); err != nil {
		return nil, err
	}
	if c.MaxBEEF, err = integer("BBOX_MAX_BEEF", getenv("BBOX_MAX_BEEF"), boxrec.DefaultMaxBEEF, boxrec.DefaultMaxBEEF); err != nil {
		return nil, err
	}
	syncMs, err := integer("BBOX_JOURNAL_SYNC_MS", getenv("BBOX_JOURNAL_SYNC_MS"), DefaultJournalSyncMs, 0)
	if err != nil {
		return nil, err
	}
	if syncMs > MaxJournalSyncMs {
		return nil, fmt.Errorf("BBOX_JOURNAL_SYNC_MS: %d is over %d", syncMs, MaxJournalSyncMs)
	}
	c.JournalSync = time.Duration(syncMs) * time.Millisecond
	if c.Prices, err = termsroute.ParsePrices(getenv("BBOX_PRICES"), Classes, "BBOX_PRICES"); err != nil {
		return nil, err
	}
	maxSessions, err := integer("BBOX_SESSIONS", getenv("BBOX_SESSIONS"), termsroute.DefaultMaxSessions, 1)
	if err != nil {
		return nil, err
	}
	ttl, err := integer("BBOX_SESSION_TTL", getenv("BBOX_SESSION_TTL"), termsroute.DefaultSessionTTL, 1)
	if err != nil {
		return nil, err
	}
	c.Sessions = termsroute.SessionBound{Max: maxSessions, TTL: time.Duration(ttl) * time.Second}
	c.Budget = termsroute.DefaultBudget
	if c.Budget.PerSec, err = rate("BBOX_HANDSHAKES_PER_SEC", getenv("BBOX_HANDSHAKES_PER_SEC"), termsroute.DefaultBudget.PerSec); err != nil {
		return nil, err
	}
	if c.Budget.Burst, err = integer("BBOX_HANDSHAKE_BURST", getenv("BBOX_HANDSHAKE_BURST"), termsroute.DefaultBudget.Burst, 1); err != nil {
		return nil, err
	}
	if c.Budget.PerAddressPerSec, err = rate("BBOX_HANDSHAKES_PER_ADDR_PER_SEC", getenv("BBOX_HANDSHAKES_PER_ADDR_PER_SEC"), termsroute.DefaultBudget.PerAddressPerSec); err != nil {
		return nil, err
	}
	if c.Budget.AddressBurst, err = integer("BBOX_HANDSHAKE_ADDR_BURST", getenv("BBOX_HANDSHAKE_ADDR_BURST"), termsroute.DefaultBudget.AddressBurst, 1); err != nil {
		return nil, err
	}
	c.Response = termsroute.DefaultResponseBudget
	if c.Response.PerSec, err = rate("BBOX_RESPONSES_PER_SEC", getenv("BBOX_RESPONSES_PER_SEC"), termsroute.DefaultResponseBudget.PerSec); err != nil {
		return nil, err
	}
	if c.Response.Burst, err = integer("BBOX_RESPONSE_BURST", getenv("BBOX_RESPONSE_BURST"), termsroute.DefaultResponseBudget.Burst, 1); err != nil {
		return nil, err
	}
	if c.Accept, err = termsroute.ParseAcceptConfig("BBOX", getenv); err != nil {
		return nil, err
	}
	if listen := strings.TrimSpace(getenv("BBOX_LISTEN")); listen != "" {
		h, p, err := net.SplitHostPort(listen)
		port, perr := strconv.Atoi(p)
		if err != nil || h == "" || perr != nil || !digits.MatchString(p) || port < 1 || port > 65535 {
			return nil, fmt.Errorf("BBOX_LISTEN: %q is not host:port", listen)
		}
		c.Listen = listen
	}
	if k := strings.TrimSpace(getenv("BBOX_PAYEE_KEY")); k != "" {
		if !payeeRE.MatchString(k) {
			return nil, errors.New("BBOX_PAYEE_KEY is not 64 lowercase hex characters")
		}
		b, _ := hex.DecodeString(k)
		c.PayeeKey, _ = ec.PrivateKeyFromBytes(b)
		if c.PayeeKey == nil {
			return nil, errors.New("BBOX_PAYEE_KEY is not a private key")
		}
	}
	headers := strings.TrimSpace(getenv("BBOX_HEADERS_URL"))
	if headers == "" {
		headers = strings.TrimSpace(getenv("OVERLAY_CHAIN_TRACKER_URL"))
	}
	c.HeadersURL = headers
	charged := false
	for _, p := range c.Prices {
		charged = charged || p > 0
	}
	if charged && (c.Listen == "" || c.PayeeKey == nil || c.HeadersURL == "") {
		return nil, errors.New("BBOX_PRICES prices a class: BBOX_LISTEN, BBOX_PAYEE_KEY and a header source (BBOX_HEADERS_URL or OVERLAY_CHAIN_TRACKER_URL) are required")
	}
	if len(c.Prices) > 0 && c.Listen == "" {
		return nil, errors.New("BBOX_PRICES needs BBOX_LISTEN, where the terms document is served")
	}
	return c, nil
}
