package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	base58 "github.com/bsv-blockchain/go-sdk/compat/base58"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"
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

// node is the configured node: its JSON-RPC and asset API.
func (g *global) node(needRPC bool) (*nodeapi.RPC, *nodeapi.Asset, error) {
	if g.cfg.Asset == "" || (needRPC && g.cfg.RPC == "") {
		if needRPC {
			return nil, nil, usage("rpc and asset must be configured (config keys rpc, asset)")
		}
		return nil, nil, usage("asset must be configured (config key asset): proofs are read from the node")
	}
	var rpc *nodeapi.RPC
	if g.cfg.RPC != "" {
		rpc = &nodeapi.RPC{URL: g.cfg.RPC, User: g.cfg.RPCUser, Pass: g.cfg.RPCPass, ID: "bbox"}
	}
	return rpc, &nodeapi.Asset{Base: g.cfg.Asset}, nil
}

// settler is the configured settlement leg.
func (g *global) settler() (publish.Settler, *publish.Arcade, error) {
	kind, addr, _ := strings.Cut(g.cfg.Settle, ":")
	switch kind {
	case "tcp":
		return &publish.TCPIngress{Addr: addr}, nil, nil
	case "rpc":
		return &publish.RPCSettler{RPC: &nodeapi.RPC{URL: addr, User: g.cfg.RPCUser, Pass: g.cfg.RPCPass, ID: "bbox"}}, nil, nil
	case "arcade":
		a := &publish.Arcade{Base: addr, Key: g.cfg.ArcadeKey}
		return a, a, nil
	}
	return nil, nil, usage("settle must be tcp:<host:port>, rpc:<url> or arcade:<url> (config key settle)")
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
	if g.cfg.Asset != "" {
		// A proof a host stored that a reorganization left stale is
		// replaced by the node's current one (spec section 8.2).
		rd.Source = &nodeapi.Asset{Base: g.cfg.Asset}
	}
	return rd, nil
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
	_, asset, err := g.node(false)
	if err != nil {
		return l, err
	}
	l.Asset = asset
	if l.Settler, l.Arcade, err = g.settler(); err != nil {
		return l, err
	}
	// A unicast publisher confirms by lookup a host that admitted nothing
	// (spec section 9), so it needs the hosts and the headers; on the plane
	// the reader confirms that the hosts named hold what the facade took,
	// when hosts and a header source are configured.
	if rd, err := g.reader(); err == nil {
		l.Reader = rd
		l.Headers = rd.Headers
	} else if g.cfg.Mode == config.ModeUnicast {
		return l, err
	} else if g.cfg.HeaderURL != "" {
		if l.Headers, err = g.headerClient(); err != nil {
			return l, err
		}
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
	o := send.Options{TreeCount: treeCount, TreeSats: limits.DefaultTreeSats, Ahead: limits.DefaultAhead,
		Fees: mint.DefaultFees, ObjectBound: g.cfg.ObjectBound, Poll: poll, Wait: 10 * time.Minute, PlaneNeed: need}
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
       bbox fund [-blocks N] [-batch N] [-rescan]   (coinbase: regtest only)

With -txid, import a payment: read the mined transaction TXID with its
proof from the node (asset), check the proof against the header source,
and add every output it pays to the home's fund address to the pool. This
is how a home on a real network (main or test) is funded: send BSV from
your own wallet to the fund address init printed, wait for one
confirmation, and import it.

Coinbase: only on a regtest chain you run (development and tests). Without
-txid, mine coinbase to the fund address through the node's
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

// cmdFund imports a payment (-txid), or mines or rescans coinbase.
// Coinbase: only on a regtest chain you run (development and tests).
func cmdFund(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("fund", fundHelp)
	blocks := fs.Int("blocks", 101, "coinbase: only on a regtest chain you run (development and tests); blocks to mine to the fund address")
	batch := fs.Int("batch", bwallet.DefaultFundBatch, "coinbase: only on a regtest chain you run (development and tests); blocks per generatetoaddress call")
	rescan := fs.Bool("rescan", false, "coinbase: only on a regtest chain you run (development and tests); re-read the last -blocks blocks for coinbase the pool lacks instead of mining")
	txid := fs.String("txid", "", "import this mined payment, sent from your own wallet to the fund address (main, test)")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usage("fund takes no arguments")
	}
	if *txid != "" {
		if *rescan {
			return usage("-txid and -rescan are exclusive")
		}
		return g.importPayment(ctx, *txid)
	}
	if *blocks < 1 {
		return usage("-blocks must be at least 1")
	}
	if g.cfg.Network == "main" {
		return usage("fund without -txid mines coinbase (coinbase: only on a regtest chain you run, for development and tests); on network main, import a payment with fund -txid (one you sent from your own wallet to the fund address)")
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
	tip, err := asset.BestHeader(ctx)
	if err != nil {
		return fmt.Errorf("node tip: %w", err)
	}
	fmt.Fprintf(g.stdout, "pool before: %d output(s), %d sat; node tip %d; fund address %s\n", e.Pool.Count(), e.Pool.Balance(), tip.Height, addr)
	var added int
	if *rescan {
		from := uint32(0)
		if uint32(*blocks) < tip.Height { //nolint:gosec // a flag value
			from = tip.Height - uint32(*blocks) //nolint:gosec // a flag value
		}
		added, err = bwallet.Rescan(ctx, e.Signer(), e.Pool, asset, from, tip.Height)
	} else {
		added, _, err = bwallet.FundFromCoinbase(ctx, e.Signer(), e.Pool, rpc, asset, *blocks, *batch)
	}
	if err != nil {
		return err
	}
	h := tip.Height
	if t2, err := asset.BestHeader(ctx); err == nil {
		h = t2.Height
	}
	fmt.Fprintf(g.stdout, "pool after:  %d output(s), %d sat (+%d); immature %d; node tip %d\n",
		e.Pool.Count(), e.Pool.Balance(), added, len(e.Pool.Immature(h)), h)
	return nil
}

// importPayment adds the outputs a mined payment pays to the fund address
// to the pool, once its proof holds against the header source.
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
	_, asset, err := g.node(false)
	if err != nil {
		return usage("fund -txid reads the payment from the node: %v", err)
	}
	raw, err := asset.TxRaw(ctx, txid)
	if err != nil {
		return fmt.Errorf("payment %s: %w", txid, err)
	}
	tx, err := guard.ParseTransaction(raw, guard.DefaultBound)
	if err != nil || tx.TxID().String() != txid {
		return refused("payment %s: the node answered other bytes", txid)
	}
	mp, _, err := asset.Proof(ctx, txid)
	if err != nil {
		return fmt.Errorf("payment %s: its proof: %w (wait for it to mine)", txid, err)
	}
	tx.MerklePath = mp
	ok, err := mp.Verify(ctx, tx.TxID(), hc)
	if err != nil {
		return fmt.Errorf("payment %s: checking its proof against %s: %w", txid, g.cfg.HeaderURL, err)
	}
	if !ok {
		return refused("payment %s: its proof does not hold against the header source %s", txid, g.cfg.HeaderURL)
	}
	fund, err := e.Signer().FundScript()
	if err != nil {
		return err
	}
	var outs []bwallet.Output
	var sats uint64
	for i, o := range tx.Outputs {
		if o.LockingScript == nil || !bytes.Equal(*o.LockingScript, *fund) {
			continue
		}
		outs = append(outs, bwallet.Output{TxID: txid, Vout: uint32(i), Satoshis: o.Satoshis, //nolint:gosec // an output index
			LockingScript: o.LockingScript.String(), Height: mp.BlockHeight, Coinbase: tx.IsCoinbase(),
			Raw: tx.Hex(), Bump: mp.Hex()})
		sats += o.Satoshis
	}
	addr, _ := e.FundAddress(e.Mainnet)
	if len(outs) == 0 {
		return usage("payment %s pays nothing to the fund address %s", txid, addr)
	}
	added, err := e.Pool.Add(outs...)
	if err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "imported %d of %d output(s) paying %s, %d sat, mined at height %d; pool %d output(s), %d sat\n",
		added, len(outs), addr, sats, mp.BlockHeight, e.Pool.Count(), e.Pool.Balance())
	if added < len(outs) {
		g.say("note: %d output(s) were in the pool already", len(outs)-added)
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
	if g.cfg.Asset != "" {
		if h, err := (&nodeapi.Asset{Base: g.cfg.Asset}).BestHeader(ctx); err != nil {
			fmt.Fprintf(out, "node        %s: %v\n", g.cfg.Asset, err)
		} else {
			tip = h.Height
			fmt.Fprintf(out, "node        %s tip %d\n", g.cfg.Asset, h.Height)
		}
	} else {
		fmt.Fprintln(out, "node        not configured (asset)")
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
	if g.cfg.HeaderURL == "" {
		fmt.Fprintln(out, "headers     NOT CONFIGURED (nothing can be verified)")
	} else if hc, err := g.headerClient(); err == nil {
		if n, err := hc.CurrentHeight(ctx); err != nil {
			fmt.Fprintf(out, "headers     %s: %v\n", g.cfg.HeaderURL, err)
		} else {
			fmt.Fprintf(out, "headers     %s tip %d\n", g.cfg.HeaderURL, n)
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
	if g.cfg.Settle != "" {
		fmt.Fprintf(out, "settle      %s\n", g.cfg.Settle)
	} else {
		fmt.Fprintln(out, "settle      not configured")
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
