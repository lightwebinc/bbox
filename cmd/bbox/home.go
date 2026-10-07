package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	base58 "github.com/bsv-blockchain/go-sdk/compat/base58"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/feepolicy"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/termsafe"

	"github.com/lightwebinc/bbox/boxrec"
	"github.com/lightwebinc/bbox/internal/config"
	"github.com/lightwebinc/bbox/internal/limits"
	"github.com/lightwebinc/bbox/internal/state"
	"github.com/lightwebinc/bbox/internal/unicast"
	"github.com/lightwebinc/bbox/reader"
	"github.com/lightwebinc/bbox/send"
)

// httpClient is the client for everything but submissions, which
// bcommon's facade makes itself.
var httpClient *http.Client

// poll paces proof waits; tests shorten it.
var poll = 2 * time.Second

// openWallet opens the home's embedded wallet.
func (g *global) openWallet() (*bwallet.Embedded, error) {
	e, err := bwallet.Open(g.cfg.Home, send.Profile)
	if err != nil {
		return nil, fmt.Errorf("open the home %s: %w (run `bbox init`)", g.cfg.Home, err)
	}
	e.Originator = g.cfg.Originator
	e.Mainnet = g.cfg.Network == "main"
	return e, nil
}

// lockHome takes the home's lock: one command that spends from the pool or
// writes the state at a time.
func (g *global) lockHome() (func(), error) {
	unlock, err := send.LockHome(g.cfg.Home)
	if errors.Is(err, send.ErrLocked) {
		return nil, usage("another bbox command is using the home %s (one at a time)", g.cfg.Home)
	}
	return unlock, err
}

// home is an open, locked home: its wallet and its state.
type home struct {
	e     *bwallet.Embedded
	st    *state.State
	close func()
}

// lockedWallet takes the home's lock and only then opens its wallet. The
// order matters: the wallet's coin pool is read into memory when it is
// opened and written back whole when it changes, so a pool read before the
// lock is one another command may change in between, and saving it would
// put back coins that command spent.
func (g *global) lockedWallet() (*bwallet.Embedded, func(), error) {
	if _, err := os.Stat(filepath.Join(g.cfg.Home, "identity.json")); err != nil {
		return nil, nil, fmt.Errorf("open the home %s: %w (run `bbox init`)", g.cfg.Home, err)
	}
	unlock, err := g.lockHome()
	if err != nil {
		return nil, nil, err
	}
	e, err := g.openWallet()
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return e, unlock, nil
}

// openHome locks the home, opens its wallet and loads its state.
func (g *global) openHome() (*home, error) {
	e, unlock, err := g.lockedWallet()
	if err != nil {
		return nil, err
	}
	st, err := send.LoadState(g.cfg.Home, e.Signer().IdentityHex())
	if err != nil {
		unlock()
		return nil, err
	}
	return &home{e: e, st: st, close: unlock}, nil
}

// node is the configured node, for what only a node does (coinbase on a
// regtest chain): its JSON-RPC and asset API.
func (g *global) node(needRPC bool) (*nodeapi.RPC, *nodeapi.Asset, error) {
	asset := g.cfg.AssetURL()
	if asset == "" || (needRPC && g.cfg.RPC == "") {
		if needRPC {
			return nil, nil, usage("rpc and a node must be configured (config keys rpc, and chain = asset:<URL>)")
		}
		return nil, nil, usage("a node must be configured (config key chain = asset:<URL>)")
	}
	var rpc *nodeapi.RPC
	if g.cfg.RPC != "" {
		rpc = &nodeapi.RPC{URL: g.cfg.RPC, User: g.cfg.RPCUser, Pass: g.cfg.RPCPass, ID: "bbox"}
	}
	return rpc, &nodeapi.Asset{Base: asset}, nil
}

// chain is the configured chain view, every proof it answers checked
// against hc: WhatsOnChain on main and test unless chain names another.
func (g *global) chain(hc chaintracker.ChainTracker) (*nodeapi.Sources, error) {
	spec := g.cfg.ChainSpec()
	if spec == "" {
		return nil, usage("no chain view: set chain (or -chain) to asset:<node URL>; a regtest chain has no public one")
	}
	c, err := nodeapi.ParseChain(spec, nodeapi.ChainOptions{WoCKey: g.cfg.WoCKey, WoCRate: g.cfg.WoCRate, Headers: hc, Client: httpClient})
	if err != nil {
		return nil, usage("chain %q: %v", spec, err)
	}
	return c, nil
}

// settler is the configured settlement leg, arcade's verdict held to the
// chain view's spends when spends is set.
func (g *global) settler(spends nodeapi.SpendSource) (publish.Settler, *publish.Arcade, error) {
	spec := g.cfg.SettleSpec()
	if spec == "" {
		return nil, nil, usage("no settlement leg: set settle (or -settle); a regtest chain has no public one (rpc:<url>, tcp:<host:port> or arcade:<url>)")
	}
	s, a, err := publish.ParseSettler(spec, publish.SettleOptions{Key: g.cfg.ArcadeKey, RPCUser: g.cfg.RPCUser,
		RPCPass: g.cfg.RPCPass, RPCID: "bbox", Spends: spends})
	if err != nil {
		return nil, nil, usage("settle %q: %v", spec, err)
	}
	return s, a, nil
}

// fees is the miner fee policy for this run: the fee_* keys over the
// network's rate. A live policy (fee_source arc) asks the configured
// policy URLs, else the arcade or ARC the leg settles through.
func (g *global) fees(ctx context.Context) (mint.Fees, error) {
	fc := g.cfg.Fee
	if (fc.Source == feepolicy.SourceARC || fc.Source == "arcade") && len(fc.PolicyURLs) == 0 {
		if _, a, err := g.settler(nil); err == nil && a != nil {
			fc.PolicyURLs = []string{a.Base}
		} else {
			return mint.Fees{}, usage("fee_source arc reads the broadcaster's policy: set fee_policy_urls, or settle through arcade or ARC")
		}
	}
	src, err := fc.Build(mint.DefaultFees)
	if err != nil {
		return mint.Fees{}, usage("fee: %v", err)
	}
	if a, ok := src.(*feepolicy.ARC); ok {
		a.Key, a.Note = g.cfg.ArcadeKey, g.say
	}
	f, err := src.Fees(ctx)
	if err != nil {
		return mint.Fees{}, usage("fee: %v", err)
	}
	return f, nil
}

// proofSource is a chain view as the reader's source of a current proof.
type proofSource struct{ nodeapi.ProofSource }

func (p proofSource) MerkleProof(ctx context.Context, txid string) (*transaction.MerklePath, error) {
	mp, _, err := p.Proof(ctx, txid)
	return mp, err
}

// reader is a reader over the configured hosts and header source.
func (g *global) reader() (*reader.Client, error) {
	hc, err := g.headerClient()
	if err != nil {
		return nil, err
	}
	if len(g.cfg.Hosts) == 0 {
		return nil, usage("no overlay host configured (config key hosts, or -hosts)")
	}
	rd := &reader.Client{Hosts: g.cfg.Hosts, Headers: hc, Timeout: g.cfg.Timeout, HTTP: httpClient}
	g.currentProofs(rd)
	return rd, nil
}

// currentProofs gives rd the chain view, when one is configured, as the
// source of a current proof: a proof a host stored that a reorganization
// left stale is replaced by the chain's current one (spec section 8.2).
func (g *global) currentProofs(rd *reader.Client) {
	if g.cfg.ChainSpec() == "" {
		return
	}
	if c, err := g.chain(rd.Headers); err == nil {
		rd.Source = proofSource{c}
	}
}

// office is the office a command uses: its -office flag, else the
// configured one.
func (g *global) office(flagValue string) (string, error) {
	o := flagValue
	if o == "" {
		o = g.cfg.Office
	}
	if o == "" {
		return "", usage("no office: set office (or -office) to the office identifier <name>_<suffix> the recipient uses")
	}
	if boxrec.CheckOffice(o) != nil {
		return "", usage("office %q is not an office identifier <name>_<suffix>", termsafe.Abbrev(o))
	}
	return o, nil
}

// legs are the endpoints a publisher publishes and settles through. None
// has a default.
func (g *global) legs() (send.Legs, error) {
	var l send.Legs
	hc, err := g.headerClient()
	if err != nil {
		return l, err
	}
	l.Headers = hc
	chain, err := g.chain(hc)
	if err != nil {
		return l, err
	}
	l.Chain = chain
	if l.Settler, l.Arcade, err = g.settler(chain); err != nil {
		return l, err
	}
	// A unicast publisher confirms by lookup a host that admitted nothing
	// (spec section 9), so it needs the hosts; on the plane the reader
	// confirms that the hosts named hold what the facade took, when hosts
	// are configured.
	if rd, err := g.reader(); err == nil {
		l.Reader = rd
	} else if g.cfg.Mode == config.ModeUnicast {
		return l, err
	}
	switch g.cfg.Mode {
	case config.ModeUnicast:
		if g.cfg.Facade != "" {
			return l, usage("facade is for mode plane; mode unicast submits to every host in hosts, so leave facade unset")
		}
		need, err := config.Need(g.cfg.Quorum, len(g.cfg.Hosts))
		if err != nil {
			return l, usage("%v (config key quorum)", err)
		}
		l.Hosts = &unicast.Set{Hosts: g.cfg.Hosts, Need: need, Retries: send.Retries, HTTP: httpClient, Note: g.say, Verbose: g.verbose}
	default:
		if g.cfg.Facade == "" {
			return l, usage("facade must be configured (config key facade): the overlay host a publisher submits to on the plane (or set mode = unicast to submit to every host in hosts)")
		}
		l.Facade = &publish.Facade{Base: g.cfg.Facade, HTTP: httpClient}
		var direct []string
		for _, h := range g.cfg.Hosts {
			if strings.TrimRight(h, "/") != strings.TrimRight(g.cfg.Facade, "/") {
				direct = append(direct, h)
			}
		}
		if len(direct) > 0 {
			l.Direct = &unicast.Set{Hosts: direct, Need: len(direct), Retries: send.Retries, HTTP: httpClient, Note: g.say, Verbose: g.verbose}
		}
	}
	return l, nil
}

// engine opens a publisher over an open home, and finishes whatever a
// previous run left.
func (g *global) engine(ctx context.Context, h *home, treeCount int) (*send.Engine, error) {
	return g.engineFor(ctx, h, treeCount, false)
}

// engineFor is engine; with lenient set, work a previous run left that
// cannot be finished now is reported and left for the next command, and
// the publisher is returned all the same. drop runs so: an envelope that
// cannot reach a host must not keep a retraction from the chain.
func (g *global) engineFor(ctx context.Context, h *home, treeCount int, lenient bool) (*send.Engine, error) {
	legs, err := g.legs()
	if err != nil {
		return nil, err
	}
	need, err := config.Need(g.cfg.Quorum, len(g.cfg.Hosts))
	if err != nil {
		if g.cfg.Mode == config.ModeUnicast || len(g.cfg.Hosts) > 0 {
			return nil, usage("%v (config key quorum)", err)
		}
		need = 0
	}
	fees, err := g.fees(ctx)
	if err != nil {
		return nil, err
	}
	o := send.Options{TreeCount: treeCount, TreeSats: limits.DefaultTreeSats, Ahead: limits.DefaultAhead,
		Fees: fees, ObjectBound: g.cfg.ObjectBound, Poll: poll, Wait: 10 * time.Minute, PlaneNeed: need}
	if uint32(treeCount) <= o.Ahead { //nolint:gosec // bounded by the limits
		o.Ahead = uint32(treeCount / 2) //nolint:gosec // bounded by the limits
	}
	s := h.e.Signer()
	eng, err := send.New(h.st, s, h.e.Pool, legs, o)
	if err != nil {
		return nil, err
	}
	eng.Note, eng.Verbose = g.say, g.verbose
	if err := eng.Start(ctx); err != nil {
		if !lenient || ctx.Err() != nil {
			_ = eng.Close(context.WithoutCancel(ctx))
			return nil, err
		}
		g.say("work an earlier command left is not finished, and is left for the next command: %v", err)
	}
	return eng, nil
}

// tallies prints, in mode unicast, each host's count of what it took and
// what it missed.
func (g *global) tallies(eng *send.Engine) {
	if eng.Legs.Hosts == nil {
		return
	}
	for _, t := range eng.Legs.Hosts.Tallies() {
		line := fmt.Sprintf("host %s: took %d object(s), missed %d", t.Host, t.Took, t.Missed)
		if t.Missed > 0 {
			line += ": " + strings.Join(t.MissedWhat, ", ")
		}
		fmt.Fprintln(g.stdout, line)
	}
}

const initHelp = `usage: bbox init

Create the home (mode 0700): an identity key and an empty coin pool. Print
the identity key, which is the address senders seal envelopes to, and the
fund address for the configured network. A home that exists is left alone
and printed.`

func cmdInit(_ context.Context, g *global, args []string) error {
	fs := g.flagSet("init", initHelp)
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usage("init takes no arguments")
	}
	// The lock is taken before the wallet is made or opened: a home that
	// exists may be in use.
	if err := os.MkdirAll(g.cfg.Home, 0o700); err != nil {
		return err
	}
	unlock, err := g.lockHome()
	if err != nil {
		return err
	}
	defer unlock()
	e, err := bwallet.Create(g.cfg.Home, send.Profile)
	if errors.Is(err, bwallet.ErrIdentityExists) {
		if e, err = g.openWallet(); err != nil {
			return err
		}
		g.say("the home %s exists; left as it is", g.cfg.Home)
	} else if err != nil {
		return err
	} else {
		fmt.Fprintln(g.stdout, "created", g.cfg.Home)
	}
	if err := os.Chmod(g.cfg.Home, 0o700); err != nil {
		return err
	}
	e.Mainnet = g.cfg.Network == "main"
	addr, err := e.FundAddress(e.Mainnet)
	if err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "identity     %s\nfund address %s (%s)\n", e.Signer().IdentityHex(), addr, g.cfg.Network)
	return nil
}

const fundHelp = `usage: bbox fund -txid TXID
       bbox fund -beef FILE|- [-mined-only]
       bbox fund [-blocks N] [-batch N] [-rescan]   (coinbase: regtest only)

Fund the home's pool from a payment you sent from your own wallet to the
fund address init printed. This is how a home on a real network (main or
test) is funded, and it needs no node of your own.

With -txid, import a mined payment: read the transaction TXID and its
proof from the chain view (chain; WhatsOnChain by default), check the
proof against the header source, and add every output it pays to the fund
address to the pool. A payment not mined yet is refused: wait for its
block, or import its BEEF.

With -beef, import the payment as the BEEF your wallet hands over (a file,
or - for standard input; binary or hex), with no lookup at all. A mined
payment's proof is checked against the header source. One not mined yet
is taken when every transaction it spends carries a proof and its scripts
verify against them; its outputs are held until it mines, and a later
command collects its proof. -mined-only (config key fund_mined_only)
refuses an unmined one instead.

Coinbase: only on a regtest chain you run (development and tests). Without
-txid or -beef, mine coinbase to the fund address through the node's
generatetoaddress, which only a chain you run answers. Coinbase can be
spent 100 blocks after it is mined, so the first fund mines 101 or more.
-rescan re-reads recent blocks for coinbase the pool lacks.

The address prefix follows the network (main: mainnet; test and regtest:
testnet), and an address for another network than the configured one,
above all a mainnet address on a test network, is refused before anything
is mined.`

// addressNetwork is the network a base58 P2PKH address is for.
func addressNetwork(addr string) (string, error) {
	b, err := base58.Decode(addr)
	if err != nil || len(b) != 25 {
		return "", fmt.Errorf("fund address %q does not decode", addr)
	}
	switch b[0] {
	case 0x00:
		return "main", nil
	case 0x6f:
		return "test", nil
	}
	return "", fmt.Errorf("fund address %q has version byte 0x%02x", addr, b[0])
}

// checkFundAddress refuses an address for another network than the
// configured one: above all a mainnet address on a test network, which a
// chain run for testing must never be asked to pay.
func checkFundAddress(addr, network string) error {
	n, err := addressNetwork(addr)
	if err != nil {
		return err
	}
	if (n == "main") != (network == "main") {
		return usage("the fund address %s is a %s address and network is %s; refusing to mine to it", addr, n, network)
	}
	return nil
}

// cmdFund imports a payment (-txid, -beef), or mines or rescans coinbase.
// Coinbase: only on a regtest chain you run (development and tests).
func cmdFund(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("fund", fundHelp)
	blocks := fs.Int("blocks", 101, "coinbase: only on a regtest chain you run (development and tests); blocks to mine to the fund address")
	batch := fs.Int("batch", bwallet.DefaultFundBatch, "coinbase: only on a regtest chain you run (development and tests); blocks per generatetoaddress call")
	rescan := fs.Bool("rescan", false, "coinbase: only on a regtest chain you run (development and tests); re-read the last -blocks blocks for coinbase the pool lacks instead of mining")
	txid := fs.String("txid", "", "import this mined payment, sent from your own wallet to the fund address (main, test)")
	beef := fs.String("beef", "", "import this payment as the BEEF your wallet handed over: a file, or - for standard input (main, test)")
	minedOnly := fs.Bool("mined-only", g.cfg.FundMinedOnly, "with -beef, refuse a payment that has not mined yet (config key fund_mined_only)")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usage("fund takes no arguments")
	}
	if *txid != "" || *beef != "" {
		if *rescan {
			return usage("-txid and -beef do not take -rescan")
		}
		if *txid != "" && *beef != "" {
			return usage("-txid and -beef are exclusive")
		}
		if *beef != "" {
			return g.importBEEF(ctx, *beef, *minedOnly)
		}
		return g.importPayment(ctx, *txid)
	}
	if *blocks < 1 {
		return usage("-blocks must be at least 1")
	}
	if g.cfg.Network == "main" {
		return usage("fund without -txid or -beef mines coinbase (coinbase: only on a regtest chain you run, for development and tests); on network main, import a payment you sent from your own wallet to the fund address with fund -txid or fund -beef")
	}
	e, unlock, err := g.lockedWallet()
	if err != nil {
		return err
	}
	defer unlock()
	addr, err := e.FundAddress(e.Mainnet)
	if err != nil {
		return err
	}
	if err := checkFundAddress(addr, g.cfg.Network); err != nil {
		return err
	}
	rpc, asset, err := g.node(true)
	if err != nil {
		return err
	}
	hc, err := g.headerClient()
	if err != nil {
		return err
	}
	tip, err := hc.CurrentHeight(ctx)
	if err != nil {
		return fmt.Errorf("chain tip: %w", err)
	}
	fmt.Fprintf(g.stdout, "pool before: %d output(s), %d sat; chain tip %d; fund address %s\n", e.Pool.Count(), e.Pool.Balance(), tip, addr)
	var added int
	if *rescan {
		from := uint32(0)
		if uint32(*blocks) < tip { //nolint:gosec // a flag value
			from = tip - uint32(*blocks) //nolint:gosec // a flag value
		}
		added, err = bwallet.Rescan(ctx, e.Signer(), e.Pool, asset, from, tip)
	} else {
		added, _, err = bwallet.FundFromCoinbase(ctx, e.Signer(), e.Pool, rpc, asset, *blocks, *batch)
	}
	if err != nil {
		return err
	}
	h := tip
	if t2, err := hc.CurrentHeight(ctx); err == nil {
		h = t2
	}
	fmt.Fprintf(g.stdout, "pool after:  %d output(s), %d sat (+%d); immature %d; chain tip %d\n",
		e.Pool.Count(), e.Pool.Balance(), added, len(e.Pool.Immature(h)), h)
	return nil
}

// importPayment adds the outputs a mined payment pays to the fund address
// to the pool, read from the chain view once its proof holds against the
// header source.
func (g *global) importPayment(ctx context.Context, txid string) error {
	if h, err := chainhash.NewHashFromHex(txid); err != nil || h.String() != txid {
		return usage("-txid %q is not a transaction id (64 lowercase hex characters)", termsafe.Abbrev(txid))
	}
	e, unlock, err := g.lockedWallet()
	if err != nil {
		return err
	}
	defer unlock()
	hc, err := g.headerClient()
	if err != nil {
		return err
	}
	chain, err := g.chain(hc)
	if err != nil {
		return err
	}
	fund, err := e.Signer().FundScript()
	if err != nil {
		return err
	}
	im, err := bwallet.ImportTxid(ctx, txid, fund, chain, hc)
	if err != nil {
		return g.importError(e, txid, err)
	}
	return g.pool(e, im)
}

// maxBEEFFile bounds what fund -beef reads: a BEEF at the import bound,
// written as hex, and a line end.
const maxBEEFFile = 2*guard.DefaultBound + 2

// importBEEF adds the outputs a payment handed over as BEEF pays to the
// fund address to the pool, checked against the header source with no
// lookup.
func (g *global) importBEEF(ctx context.Context, path string, minedOnly bool) error {
	var r io.Reader = g.stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return usage("-beef: %v", err)
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, maxBEEFFile+1))
	if err != nil {
		return fmt.Errorf("-beef: %w", err)
	}
	if len(b) > maxBEEFFile {
		return usage("-beef: more than %d bytes; a funding payment is far smaller", maxBEEFFile)
	}
	if t := bytes.TrimSpace(b); len(t) > 0 && len(t)%2 == 0 {
		if raw, err := hex.DecodeString(string(t)); err == nil {
			b = raw
		}
	}
	e, unlock, err := g.lockedWallet()
	if err != nil {
		return err
	}
	defer unlock()
	hc, err := g.headerClient()
	if err != nil {
		return err
	}
	fund, err := e.Signer().FundScript()
	if err != nil {
		return err
	}
	im, err := bwallet.ImportBEEF(ctx, b, fund, hc, bwallet.ImportOptions{RefuseUnmined: minedOnly})
	if err != nil {
		return g.importError(e, "in the BEEF", err)
	}
	return g.pool(e, im)
}

// importError words an import's refusal.
func (g *global) importError(e *bwallet.Embedded, what string, err error) error {
	addr, _ := e.FundAddress(e.Mainnet)
	switch {
	case errors.Is(err, bwallet.ErrPaysNothing):
		return usage("payment %s pays nothing to the fund address %s", what, addr)
	case errors.Is(err, bwallet.ErrUnmined):
		return usage("payment %s has not mined yet: import it once it has a block, or import the BEEF your wallet hands over (fund -beef)", what)
	case errors.Is(err, nodeapi.ErrProofRefused):
		return refused("payment %s: its proof does not hold against the header source %s", what, g.cfg.Headers())
	case errors.Is(err, bwallet.ErrUnprovenParent):
		return refused("payment %s is not mined and spends a transaction with no proof: wait for its block and import it again", what)
	}
	return fmt.Errorf("payment %s: %w", what, err)
}

// pool adds an imported payment's outputs to the pool.
func (g *global) pool(e *bwallet.Embedded, im *bwallet.Import) error {
	addr, _ := e.FundAddress(e.Mainnet)
	added, err := e.Pool.Add(im.Outputs...)
	if err != nil {
		return err
	}
	if im.Mined {
		fmt.Fprintf(g.stdout, "imported %d of %d output(s) paying %s, %d sat, mined at height %d; pool %d output(s), %d sat\n",
			added, len(im.Outputs), addr, im.Sats, im.Height, e.Pool.Count(), e.Pool.Balance())
	} else {
		fmt.Fprintf(g.stdout, "imported %d of %d output(s) paying %s, %d sat, not mined yet: held until it mines, when a later command collects its proof; pool %d output(s), %d sat\n",
			added, len(im.Outputs), addr, im.Sats, e.Pool.Count(), e.Pool.Balance())
	}
	if added < len(im.Outputs) {
		g.say("note: %d output(s) were in the pool already", len(im.Outputs)-added)
	}
	return nil
}

const officeHelp = `usage: bbox office new <name>
       bbox office list

new draws the office's 10-letter suffix uniformly at random and prints the
office identifier <name>_<suffix>, its topic, and the line a host's
BBOX_OFFICES takes to carry it. <name> is 1 to 31 lowercase letters and
single underscores, starting and ending with a letter. The suffix is not a
secret and not an access control: it keeps unrelated offices off each
other's topic. When a home exists, the office is recorded in it, and list
prints the offices recorded.`

func cmdOffice(_ context.Context, g *global, args []string) error {
	fs := g.flagSet("office", officeHelp)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 2 && pos[0] == "new":
		if boxrec.CheckOfficeName(pos[1]) != nil {
			return usage("office name %q: 1 to %d lowercase letters and single underscores, starting and ending with a letter", termsafe.Abbrev(pos[1]), boxrec.MaxOfficeName)
		}
		office, err := boxrec.NewOffice(pos[1], rand.Reader)
		if err != nil {
			return err
		}
		topic, _ := boxrec.Topic(office)
		if _, serr := os.Stat(filepath.Join(g.cfg.Home, "identity.json")); serr == nil {
			e, unlock, err := g.lockedWallet()
			if err != nil {
				return err
			}
			defer unlock()
			st, err := send.LoadState(g.cfg.Home, e.Signer().IdentityHex())
			if err != nil {
				return err
			}
			st.AddOffice(office)
			if err := st.Save(); err != nil {
				return err
			}
		}
		fmt.Fprintf(g.stdout, "office %s\ntopic  %s\nhost   BBOX_OFFICES=%s\n", office, topic, office)
		return nil
	case len(pos) == 1 && pos[0] == "list":
		e, err := g.openWallet()
		if err != nil {
			return err
		}
		st, err := send.LoadState(g.cfg.Home, e.Signer().IdentityHex())
		if err != nil {
			return err
		}
		for _, o := range st.Offices {
			fmt.Fprintln(g.stdout, o)
		}
		return nil
	}
	return usage("office new <name> | office list")
}

const doctorHelp = `usage: bbox doctor

Report the home (identity, pool, funding tree, what is persisted and not
yet published) and whether the configured node, header source, hosts and
priced host answer. Reads only: it publishes nothing and spends nothing.`

func cmdDoctor(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("doctor", doctorHelp)
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usage("doctor takes no arguments")
	}
	out := g.stdout
	fmt.Fprintf(out, "home        %s\n", g.cfg.Home)
	fmt.Fprintf(out, "network     %s\n", g.cfg.Network)
	if g.cfg.Office != "" {
		fmt.Fprintf(out, "office      %s\n", termsafe.Text(g.cfg.Office))
	} else {
		fmt.Fprintln(out, "office      not configured")
	}
	var tip uint32
	var hc *headers.Client
	if spec := g.cfg.Headers(); spec == "" {
		fmt.Fprintln(out, "headers     NOT CONFIGURED (nothing can be verified)")
	} else if hc, _ = g.headerClient(); hc != nil {
		if n, err := hc.CurrentHeight(ctx); err != nil {
			fmt.Fprintf(out, "headers     %s: %v\n", spec, err)
		} else {
			tip = n
			fmt.Fprintf(out, "headers     %s tip %d\n", spec, n)
		}
	}
	if spec := g.cfg.ChainSpec(); spec == "" {
		fmt.Fprintln(out, "chain       not configured (chain)")
	} else {
		fmt.Fprintf(out, "chain       %s\n", spec)
	}
	if spec := g.cfg.SettleSpec(); spec == "" {
		fmt.Fprintln(out, "settle      not configured")
	} else if _, a, err := g.settler(nil); err != nil {
		fmt.Fprintf(out, "settle      %s: %v\n", spec, err)
	} else if a != nil {
		if err := a.Ping(ctx); err != nil {
			fmt.Fprintf(out, "settle      %s: NOT ANSWERING: %s\n", spec, firstLine(err.Error()))
		} else {
			fmt.Fprintf(out, "settle      %s answering\n", spec)
		}
	} else {
		fmt.Fprintf(out, "settle      %s\n", spec)
	}
	if f, err := g.fees(ctx); err != nil {
		fmt.Fprintf(out, "fee         %v\n", err)
	} else {
		fmt.Fprintf(out, "fee         %s satoshis/bytes, floor %d sat (%s)\n", rateOf(f), f.Floor, feeSource(g.cfg.Fee.Source))
	}
	if e, err := g.openWallet(); err != nil {
		fmt.Fprintf(out, "identity    absent (%v)\n", err)
	} else {
		fmt.Fprintf(out, "identity    %s\n", e.Signer().IdentityHex())
		fmt.Fprintf(out, "pool        %d output(s), %d sat", e.Pool.Count(), e.Pool.Balance())
		if tip > 0 {
			fmt.Fprintf(out, ", %d immature", len(e.Pool.Immature(tip)))
		}
		if n := len(e.Pool.UnprovenTxids()); n > 0 {
			fmt.Fprintf(out, ", change from %d transaction(s) held until mined", n)
		}
		fmt.Fprintln(out)
		if st, err := state.Load(g.cfg.Home, e.Signer().IdentityHex()); err != nil {
			fmt.Fprintf(out, "state       UNREADABLE: %v\n", err)
		} else {
			if st.Tree != nil {
				fmt.Fprintf(out, "tree        %s %d of %d left\n", st.Tree.Txid, st.Tree.Remaining(), st.Tree.Count)
			} else {
				fmt.Fprintln(out, "tree        none yet (the first send mints one)")
			}
			for _, p := range st.UnsettledTrees() {
				fmt.Fprintf(out, "tree        %s signed for coin %s and not recorded as minted; the next send, ack or drop asks the node what became of it\n", p.Tree.Txid, p.Coin.Outpoint())
			}
			fmt.Fprintf(out, "sent        %d envelope(s), %d receipt(s), %d sweep(s)\n", len(st.Sent), len(st.Receipts), len(st.Sweeps))
			for _, p := range st.Outbox {
				fmt.Fprintf(out, "outbox      %s %s persisted and not confirmed published; the next send, ack or drop publishes it\n", p.Kind, p.Txid)
			}
			if sw := st.InFlight(); sw != nil {
				fmt.Fprintf(out, "sweep       %s in flight; the next drop finishes it\n", sw.Txid)
			}
			for _, sw := range st.Sweeps {
				if sw.Failed != "" {
					fmt.Fprintf(out, "sweep       %s FAILED: %s; it retracts nothing (drop again to sweep its outputs)\n", sw.Txid, termsafe.Text(sw.Failed))
				}
			}
		}
	}
	switch {
	case g.cfg.Mode == config.ModeUnicast:
		need, err := config.Need(g.cfg.Quorum, len(g.cfg.Hosts))
		if err != nil {
			fmt.Fprintf(out, "mode        unicast: %v\n", err)
		} else {
			fmt.Fprintf(out, "mode        unicast: every object to each of %d host(s), published once %d take it (quorum %s)\n", len(g.cfg.Hosts), need, g.cfg.Quorum)
		}
	case g.cfg.Facade != "":
		fmt.Fprintf(out, "mode        plane\nfacade      %s\n", g.cfg.Facade)
	default:
		fmt.Fprintln(out, "mode        plane\nfacade      not configured")
	}
	for _, h := range g.cfg.Hosts {
		fmt.Fprintf(out, "host        %s %s\n", h, ping(ctx, h, g.cfg.Timeout))
	}
	if g.cfg.HistoryHost != "" {
		t, err := reader.FetchTerms(ctx, termsClient(g.cfg.Timeout), g.cfg.HistoryHost)
		switch {
		case errors.Is(err, reader.ErrNoTerms):
			fmt.Fprintf(out, "history     %s prices nothing\n", g.cfg.HistoryHost)
		case err != nil:
			fmt.Fprintf(out, "history     %s: %v\n", g.cfg.HistoryHost, firstLine(err.Error()))
		default:
			fmt.Fprintf(out, "history     %s serves terms (%d priced class(es))\n", g.cfg.HistoryHost, len(t.Classes))
		}
	}
	return nil
}

// rateOf is a fee policy's rate, as SATS/BYTES.
func rateOf(f mint.Fees) string {
	r := f.Rate
	if r.IsZero() {
		r = mint.Rate{Sats: f.SatPerByte, Bytes: 1}
	}
	return r.String()
}

// feeSource names a fee policy's source.
func feeSource(s string) string {
	if s == "" || s == feepolicy.SourceStatic {
		return "static"
	}
	return "the broadcaster's published policy"
}

// pingQuery is a question no class answers, which a live ls_bbox refuses
// with an error.
var pingQuery = reader.Query{}

// ping asks a host a question it must refuse: the host is up and runs the
// service.
func ping(ctx context.Context, base string, timeout time.Duration) string {
	c := &reader.Client{Timeout: timeout, HTTP: httpClient}
	_, err := c.Ask(ctx, base, pingQuery)
	switch {
	case err == nil || errors.Is(err, reader.ErrHost):
		return "answering"
	case strings.Contains(err.Error(), "status 5"):
		return "answering with an ERROR (" + firstLine(err.Error()) + ")"
	}
	return "NOT ANSWERING: " + firstLine(err.Error())
}

func termsClient(timeout time.Duration) *http.Client {
	if httpClient != nil {
		return httpClient
	}
	return &http.Client{Timeout: timeout}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// field is someone else's text that fills one field of a line this command
// prints (a reference's locator, a class name, a refusal's words): filtered
// for the terminal, and held to that line. Such text is any UTF-8, and one
// with a line break in it would otherwise write lines of its own in a
// report, in this command's voice.
func field(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t', '\v', '\f', 0x85, 0x2028, 0x2029:
			return ' '
		}
		return r
	}, s)
	return termsafe.Text(s)
}

// plain is a note or an error filtered for the terminal, whole. termsafe
// bounds a line at 512 columns, and a line that quotes a refusal with two
// transaction ids and a host's words is longer: it would lose its end,
// which is where it says what to do. So each line is filtered in pieces of
// 256 runes: a piece holds nothing a terminal acts on once filtered, and
// joining pieces adds nothing, so the whole is as safe as its parts. At
// most 200 lines are printed.
func plain(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > termsafe.MaxLines {
		lines = append(lines[:termsafe.MaxLines], "[truncated]")
	}
	for i, l := range lines {
		r := []rune(l)
		var b strings.Builder
		for len(r) > 0 {
			n := min(len(r), 256)
			b.WriteString(termsafe.Text(string(r[:n])))
			r = r[n:]
		}
		lines[i] = b.String()
	}
	return strings.Join(lines, "\n")
}
