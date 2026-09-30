package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/termsafe"

	"github.com/lightwebinc/bbox/internal/boxrec"
	"github.com/lightwebinc/bbox/internal/config"
	"github.com/lightwebinc/bbox/internal/limits"
	"github.com/lightwebinc/bbox/internal/send"
	"github.com/lightwebinc/bbox/internal/state"
)

// refFlags collects -ref values.
type refFlags []string

func (r *refFlags) String() string     { return strings.Join(*r, " ") }
func (r *refFlags) Set(v string) error { *r = append(*r, v); return nil }

const sendHelp = `usage: bbox send <recipient> [box] [-m TEXT | -file PATH] [-pay SATS]
                 [-ref URL,SHA256,LENGTH[,KEY]]... [-expires DUR] [-office OFFICE]

Seal a message to the recipient's identity key (66 hex characters) in box
(default the configured box, else inbox), in the office the recipient
reads, and publish it: once to the facade on the plane, or to every host in
mode unicast. The message is TEXT, the file PATH, or standard input.

The body is encrypted to the recipient (BRC-78); who wrote to whom, when,
in which box and office, is public. -pay puts a BRC-29 payment of SATS
inside the encrypted message: it is never broadcast by the sender; the
recipient internalizes it, which broadcasts it, until an hour before the
envelope expires (default 24h with a payment). -ref names content too
large for an envelope (at most 32): an https:// or uhrp:// locator, the
SHA-256 of the bytes as fetched, their length, and, when they are
encrypted, their AES-256 key.

The carrier is persisted in the home before it is published; a send that
stops part way is finished by the next send, ack or drop.`

func cmdSend(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("send", sendHelp)
	text := fs.String("m", "", "the message text")
	file := fs.String("file", "", "read the message from this file")
	pay := fs.Uint64("pay", 0, "satoshis of a payment inside the envelope")
	expires := fs.Duration("expires", 0, "how long the envelope stays open (0: no expiry of its own; 24h with -pay)")
	office := fs.String("office", "", "the office (default the configured office)")
	treeCount := fs.Int("tree-count", g.cfg.TreeCount, "outputs of a new funding tree")
	rate := fs.Float64("rate", limits.DefaultRate, "envelopes a second at most")
	var refs refFlags
	fs.Var(&refs, "ref", "a reference URL,SHA256,LENGTH[,KEY]; repeatable, at most 32")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usage("send <recipient> [box]")
	}
	to, err := guard.ParsePubKeyHex(pos[0])
	if err != nil || hex.EncodeToString(to.Compressed()) != pos[0] {
		return usage("recipient %q is not an identity key (66 lowercase hex characters)", termsafe.Abbrev(pos[0]))
	}
	box := g.cfg.Box
	if len(pos) == 2 {
		box = pos[1]
	}
	if box == "" {
		box = "inbox"
	}
	if boxrec.CheckBox(box) != nil {
		return usage("box %q breaks the box name grammar (1 to 50 lowercase letters, digits and single underscores, starting with a letter)", termsafe.Abbrev(box))
	}
	off, err := g.office(*office)
	if err != nil {
		return err
	}
	if *rate <= 0 || *rate > limits.MaxRate {
		return usage("-rate %v is outside 0 (exclusive) to %v: every host on the office receives every envelope (docs/limits.md)", *rate, limits.MaxRate)
	}
	body, err := readBody(g, *text, *file)
	if err != nil {
		return err
	}
	parsed, err := parseRefs(refs)
	if err != nil {
		return err
	}
	if body == "" && *pay == 0 && len(parsed) == 0 {
		return usage("nothing to send: give a message (-m, -file or standard input), a payment (-pay) or a reference (-ref)")
	}
	life := *expires
	if *pay > 0 && life == 0 {
		life = limits.PaymentExpires
	}
	if *pay > 0 && life < 2*time.Hour {
		return usage("-expires %s: an envelope that carries a payment stays open at least 2h, so the recipient has time to internalize it (spec section 10)", life)
	}
	check := limits.Send{Plane: g.cfg.Mode == config.ModePlane, Hosts: len(g.cfg.Hosts), TreeCount: *treeCount,
		ObjectBound: g.cfg.ObjectBound, Refs: len(parsed), Expires: life}
	if g.cfg.Mode == config.ModeUnicast {
		check.Need, _ = config.Need(g.cfg.Quorum, len(g.cfg.Hosts))
	}
	warnings, err := check.Check()
	if err != nil {
		return usage("%v", err)
	}
	for _, w := range warnings {
		g.say("warning: %s", w)
	}

	h, err := g.openHome()
	if err != nil {
		return err
	}
	defer h.close()
	eng, err := g.engine(ctx, h, *treeCount)
	if err != nil {
		return err
	}
	defer func() { _ = eng.Close(context.WithoutCancel(ctx)) }()
	if wait := time.Until(time.UnixMilli(h.st.LastSentMs).Add(limits.Gap(*rate))); wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	now := uint64(time.Now().Unix()) //nolint:gosec // a clock after 1970
	letter := send.Letter{Office: off, To: to.Compressed(), Box: box, Created: now}
	if life > 0 {
		letter.Expires = now + uint64(life/time.Second)
	}
	doc := &boxrec.Object{}
	if body != "" {
		doc.Set(boxrec.PlainBody, body)
	}
	if *pay > 0 {
		p, err := eng.Payment(ctx, pos[0], *pay)
		if err != nil {
			return err
		}
		doc.Set(boxrec.PlainPayment, p.Member)
		letter.Pay = p
	}
	if len(parsed) > 0 {
		doc.Set(boxrec.PlainRefs, parsed)
	}
	plain, err := boxrec.Canonical(doc)
	if err == nil {
		// The recipient's checks, before anything is sealed: a message the
		// recipient would refuse is not sent.
		if _, perr := boxrec.ParsePlaintext(plain); perr != nil {
			err = usage("the message breaks the recipient's rules (%s): %v", boxrec.Reason(perr), perr)
		}
	}
	if err != nil {
		letter.Pay.Abort()
		return err
	}
	letter.Plaintext = plain
	if g.cfg.Mode == config.ModePlane && len(plain) > limits.PlaneContentWarn/2 {
		g.say("warning: the message is %d bytes before sealing; on the plane a carrier over about %d bytes of content is more than 8 packets (docs/limits.md)", len(plain), limits.PlaneContentWarn)
	}
	sent, err := eng.Envelope(ctx, letter)
	if err != nil {
		if sent != nil {
			g.tallies(eng)
		}
		return err
	}
	fmt.Fprintf(g.stdout, "sent    %s\noffice  %s\nto      %s\nbox     %s\n", sent.Txid, off, sent.To, box)
	if sent.Expires > 0 {
		fmt.Fprintf(g.stdout, "expires %s\n", boxrec.RFC3339(sent.Expires))
	}
	if sent.Paid > 0 {
		fmt.Fprintf(g.stdout, "payment %d sat in %s, not broadcast: the recipient internalizes it\n", sent.Paid, sent.PaymentTxid)
	}
	g.tallies(eng)
	return nil
}

// readBody is the message: -m, -file, or standard input.
func readBody(g *global, text, file string) (string, error) {
	switch {
	case text != "" && file != "":
		return "", usage("-m and -file are exclusive")
	case text != "":
		return text, nil
	case file != "":
		b, err := os.ReadFile(file) //nolint:gosec // the user's own file
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	b, err := io.ReadAll(io.LimitReader(g.stdin, boxrec.MaxContent+1))
	if err != nil {
		return "", err
	}
	if len(b) > boxrec.MaxContent {
		return "", usage("the message is over %d bytes, more than an envelope holds: reference it with -ref (docs/limits.md)", boxrec.MaxContent)
	}
	return string(b), nil
}

// parseRefs parses -ref values into plaintext references.
func parseRefs(refs []string) ([]any, error) {
	if len(refs) > limits.MaxRefs {
		return nil, usage("%d -ref values, over the limit of %d (docs/limits.md)", len(refs), limits.MaxRefs)
	}
	var out []any
	for _, r := range refs {
		parts := strings.Split(r, ",")
		if len(parts) < 3 || len(parts) > 4 {
			return nil, usage("-ref %q is not URL,SHA256,LENGTH[,KEY]", termsafe.Abbrev(r))
		}
		n, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || n < 0 || n > boxrec.MaxSafe {
			return nil, usage("-ref %q: the length is not a non-negative integer", termsafe.Abbrev(r))
		}
		o := &boxrec.Object{}
		o.Set("url", parts[0])
		o.Set("sha256", parts[1])
		o.Set("length", boxrec.Int(n))
		if len(parts) == 4 {
			o.Set("key", parts[3])
		}
		out = append(out, o)
	}
	return out, nil
}

const ackHelp = `usage: bbox ack <txid>... [-all] [-office OFFICE]

Acknowledge envelopes this identity has read (bbox read), with one receipt
per 64 envelopes: every host that applies the receipt stops answering them
to the free questions and keeps them, for as long as its policy says, for
the priced history. -all acknowledges every envelope read and not yet
acknowledged. A receipt is itself a carrier on one of this identity's
funding outputs.`

func cmdAck(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("ack", ackHelp)
	all := fs.Bool("all", false, "every envelope read and not yet acknowledged")
	office := fs.String("office", "", "with -all: only this office")
	treeCount := fs.Int("tree-count", g.cfg.TreeCount, "outputs of a new funding tree")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if (len(pos) == 0) == !*all {
		return usage("ack <txid>... | ack -all")
	}
	h, err := g.openHome()
	if err != nil {
		return err
	}
	defer h.close()
	byOffice := map[string][]string{}
	var order []string
	add := func(r *state.Received) {
		if _, ok := byOffice[r.Office]; !ok {
			order = append(order, r.Office)
		}
		if !slices.Contains(byOffice[r.Office], r.Txid) {
			byOffice[r.Office] = append(byOffice[r.Office], r.Txid)
		}
	}
	if *all {
		for i := range h.st.Received {
			r := &h.st.Received[i]
			if r.Acked != "" || (*office != "" && r.Office != *office) {
				continue
			}
			if r.Paid > 0 && r.Internalized == "" {
				// A recipient that internalizes acknowledges; -all leaves the
				// envelope to internalize, which works after an ack too.
				g.say("%s carries a payment of %d sat not yet taken: bbox internalize %s (it acknowledges), or ack it by txid", r.Txid, r.Paid, r.Txid)
				continue
			}
			add(r)
		}
		if len(order) == 0 {
			fmt.Fprintln(g.stdout, "nothing to acknowledge: every envelope read is acknowledged")
			return nil
		}
	}
	for _, id := range pos {
		r := h.st.ReceivedByTxid(id)
		if r == nil {
			return usage("%s is not an envelope this home has read: bbox read it first (a recipient acknowledges what it has read)", termsafe.Abbrev(id))
		}
		add(r)
	}
	eng, err := g.engine(ctx, h, *treeCount)
	if err != nil {
		return err
	}
	defer func() { _ = eng.Close(context.WithoutCancel(ctx)) }()
	for _, off := range order {
		ids := byOffice[off]
		for len(ids) > 0 {
			n := min(len(ids), boxrec.MaxAcks)
			rc, err := eng.Receipt(ctx, off, ids[:n])
			if err != nil {
				g.tallies(eng)
				return err
			}
			fmt.Fprintf(g.stdout, "acknowledged %d envelope(s) in %s with receipt %s\n", n, off, rc.Txid)
			ids = ids[n:]
		}
	}
	g.tallies(eng)
	return nil
}

const dropHelp = `usage: bbox drop <txid>...
       bbox drop -tree TXID -yes

Retract envelopes this identity sent, by sweeping the funding outputs their
carriers spent: one mined transaction per funding tree. Once it mines it is
published to every office it retracts in (on the plane to the facade and
directly to every other host named; in mode unicast to every host), and
every conforming host stops answering those envelopes, read or not. -tree
sweeps every output of a funding tree, used or not, which retracts every
carrier on it, receipts included, and takes its unused value into the
tombstone. Retraction reaches honest hosts only: it is not erasure, and a
copy anyone kept stays a copy.`

func cmdDrop(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("drop", dropHelp)
	tree := fs.String("tree", "", "sweep every output of this funding tree")
	yes := fs.Bool("yes", false, "with -tree: sweep it")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if (len(pos) == 0) == (*tree == "") {
		return usage("drop <txid>... | drop -tree TXID -yes")
	}
	h, err := g.openHome()
	if err != nil {
		return err
	}
	defer h.close()
	type plan struct {
		tree    string
		vouts   []uint32
		offices []string
	}
	var plans []*plan
	find := func(t string) *plan {
		for _, p := range plans {
			if p.tree == t {
				return p
			}
		}
		p := &plan{tree: t}
		plans = append(plans, p)
		return p
	}
	if *tree != "" {
		p := find(*tree)
		for _, s := range h.st.Sent {
			if s.Tree == *tree && !slices.Contains(p.offices, s.Office) {
				p.offices = append(p.offices, s.Office)
			}
		}
		for _, r := range h.st.Receipts {
			if r.Tree == *tree && !slices.Contains(p.offices, r.Office) {
				p.offices = append(p.offices, r.Office)
			}
		}
		if len(p.offices) == 0 && g.cfg.Office != "" {
			p.offices = []string{g.cfg.Office}
		}
	}
	for _, id := range pos {
		s := h.st.SentByTxid(id)
		if s == nil {
			return usage("%s is not an envelope this home sent", termsafe.Abbrev(id))
		}
		if s.Swept != "" {
			g.say("%s is retracted already, by sweep %s", id, s.Swept)
			continue
		}
		p := find(s.Tree)
		if !slices.Contains(p.vouts, s.Vout) {
			p.vouts = append(p.vouts, s.Vout)
		}
		if !slices.Contains(p.offices, s.Office) {
			p.offices = append(p.offices, s.Office)
		}
	}
	eng, err := g.engine(ctx, h, g.cfg.TreeCount)
	if err != nil {
		return err
	}
	defer func() { _ = eng.Close(context.WithoutCancel(ctx)) }()
	for _, p := range plans {
		if *tree != "" {
			t, ok := eng.TreeOf(p.tree)
			if !ok {
				return usage("tree %s is not a funding tree this home recorded", termsafe.Abbrev(p.tree))
			}
			for v := uint32(0); v < t.Count; v++ {
				p.vouts = append(p.vouts, v)
			}
			if !*yes {
				fmt.Fprintf(g.stdout, "would sweep %d output(s) of %s, retracting every carrier on them in %s; add -yes\n", len(p.vouts), p.tree, strings.Join(p.offices, ", "))
				return nil
			}
		}
		if len(p.vouts) == 0 {
			continue
		}
		if len(p.offices) == 0 {
			return usage("no office to publish the sweep of %s to: set office (or -office)", p.tree)
		}
		sw, err := eng.Retract(ctx, p.tree, p.vouts, p.offices)
		if err != nil {
			g.tallies(eng)
			return err
		}
		fmt.Fprintf(g.stdout, "retracted %d funding output(s) of %s: sweep %s mined at height %d, published to %s\n",
			len(sw.Vouts), p.tree, sw.Txid, sw.Height, strings.Join(sw.Offices, ", "))
	}
	g.say("retraction reaches honest hosts only: it is not erasure, and a copy anyone kept stays a copy")
	g.tallies(eng)
	return nil
}
