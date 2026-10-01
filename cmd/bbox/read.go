package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	clients "github.com/bsv-blockchain/go-sdk/auth/clients/authhttp"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/lookup"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/termsafe"

	"github.com/lightwebinc/bbox/internal/boxrec"
	"github.com/lightwebinc/bbox/internal/limits"
	"github.com/lightwebinc/bbox/internal/purse"
	"github.com/lightwebinc/bbox/internal/reader"
	"github.com/lightwebinc/bbox/internal/send"
	"github.com/lightwebinc/bbox/internal/state"
	"github.com/lightwebinc/bbox/internal/unicast"
)

// view is which envelope class a reading command asks: the inbox, a box, or
// a sender's envelopes.
type view struct {
	office string
	to     []byte
	box    string
	from   string
}

func (v view) query() reader.Query {
	q := reader.Query{"office": v.office, "to": hex.EncodeToString(v.to)}
	switch {
	case v.box != "":
		q["box"] = v.box
	case v.from != "":
		q["from"] = v.from
	}
	return q
}

func (v view) String() string {
	switch {
	case v.box != "":
		return "box " + v.box
	case v.from != "":
		return "from " + termsafe.Abbrev(v.from)
	}
	return "inbox"
}

// viewFlags are the flags every reading command takes.
type viewFlags struct {
	office, box, from, to *string
}

func (g *global) viewFlags(fs interface {
	String(name, value, usage string) *string
}) viewFlags {
	return viewFlags{
		office: fs.String("office", "", "the office (default the configured office)"),
		box:    fs.String("box", "", "one box (default the configured box, else every box)"),
		from:   fs.String("from", "", "only envelopes from this sender's identity key"),
		to:     fs.String("to", "", "the recipient (default this home's identity)"),
	}
}

// resolve is the view the flags name. me is the home's identity, or nil
// when there is no home.
func (g *global) resolve(f viewFlags, me []byte) (view, error) {
	var v view
	var err error
	if v.office, err = g.office(*f.office); err != nil {
		return v, err
	}
	v.box = *f.box
	if v.box == "" && *f.from == "" {
		v.box = g.cfg.Box
	}
	if v.box != "" && *f.from != "" {
		return v, usage("-box and -from are exclusive: a question names a box or a sender")
	}
	if v.box != "" && boxrec.CheckBox(v.box) != nil {
		return v, usage("box %q breaks the box name grammar", termsafe.Abbrev(v.box))
	}
	if *f.from != "" {
		k, err := guard.ParsePubKeyHex(*f.from)
		if err != nil || hex.EncodeToString(k.Compressed()) != *f.from {
			return v, usage("-from %q is not an identity key (66 lowercase hex characters)", termsafe.Abbrev(*f.from))
		}
		v.from = *f.from
	}
	switch {
	case *f.to != "":
		k, err := guard.ParsePubKeyHex(*f.to)
		if err != nil || hex.EncodeToString(k.Compressed()) != *f.to {
			return v, usage("-to %q is not an identity key (66 lowercase hex characters)", termsafe.Abbrev(*f.to))
		}
		v.to = k.Compressed()
	case me != nil:
		v.to = me
	default:
		return v, usage("no recipient: run `bbox init`, or name one with -to")
	}
	return v, nil
}

// listing asks every host the view's question and reports what they
// answered, refused or lacked. Its error is nil when every host answered
// the same verified set, exit 1 when a host answered something that does
// not verify, exit 3 when hosts disagree or one could not be asked.
func (g *global) listing(ctx context.Context, rd *reader.Client, v view) (*reader.Listing, error) {
	l, err := rd.List(ctx, v.query(), v.to)
	if err != nil {
		return nil, usage("%v", err)
	}
	var problem error
	for _, a := range l.Answers {
		switch {
		case a.Err != nil:
			g.say("host %s: NOT ASKED: %s", a.Host, firstLine(a.Err.Error()))
			problem = incomplete("%d of %d host(s) answered", l.Answered(), len(l.Answers))
		case a.Truncated:
			g.say("host %s: more than %d pages; the rest is not read (docs/limits.md)", a.Host, limits.MaxPages)
		}
	}
	if l.Answered() == 0 {
		return l, usage("no host answered")
	}
	for _, r := range l.Refusals() {
		g.say("host %s: REFUSED %s (%s): %v", r.Host, r.Txid, r.Reason, r.Err)
		problem = refused("a host answered what does not verify; it is not shown")
	}
	for host, txids := range l.Missing() {
		g.say("host %s: DISAGREES: it does not answer %d envelope(s) another host answered: %s", host, len(txids), strings.Join(txids, ", "))
		if problem == nil {
			problem = incomplete("the hosts disagree about the %s; an envelope one host withholds is shown from the others", v)
		}
	}
	return l, problem
}

// hostsOf names the hosts that answered an envelope, when more than one
// was asked.
func hostsOf(l *reader.Listing, txid string, asked int) string {
	if asked < 2 {
		return ""
	}
	return strings.Join(l.By[txid], ", ")
}

const listHelp = `usage: bbox list [-box BOX | -from KEY] [-office OFFICE] [-to KEY] [-fill]

List the open envelopes to this identity (or -to) in the office: the inbox
(every box), one box, or one sender's envelopes. Every host configured is
asked, page after page; every envelope is checked against the headers and
the host's own rules before it is printed, and the hosts' answers are
compared: a host that withholds an envelope another answered is reported,
never merged silently (exit 3). One line per envelope: created, txid,
sender, box, size, and the hosts that answered it.

-fill copies each envelope a host lacks across to it (spec section 9): the
carrier as another host answered it and as it verified here, submitted to
the host that lacks it and confirmed there by a lookup. A host that did not
lose it but dropped it (a superseded carrier, for one) does not take it
back, and stays reported. Exit 0 once every host holds every envelope.`

func cmdList(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("list", listHelp)
	vf := g.viewFlags(fs)
	fill := fs.Bool("fill", false, "copy each envelope a host lacks across to it")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usage("list takes no arguments")
	}
	var me []byte
	if e, err := g.openWallet(); err == nil {
		me = e.Signer().Identity.Compressed()
	}
	v, err := g.resolve(vf, me)
	if err != nil {
		return err
	}
	rd, err := g.reader()
	if err != nil {
		return err
	}
	l, problem := g.listing(ctx, rd, v)
	if l == nil {
		return problem
	}
	for _, it := range l.Items {
		e := it.Envelope()
		line := fmt.Sprintf("%s  %s  from %s  box %s  %dB", boxrec.RFC3339(e.Created), it.Txid, hex.EncodeToString(e.From),
			termsafe.Text(e.Box), len(e.Content))
		if e.Expires > 0 {
			line += "  expires " + boxrec.RFC3339(e.Expires)
		}
		if hs := hostsOf(l, it.Txid, len(rd.Hosts)); hs != "" {
			line += "  [" + hs + "]"
		}
		fmt.Fprintln(g.stdout, line)
	}
	g.say("%d envelope(s) in the %s of %s, %d of %d host(s) answering", len(l.Items), v, termsafe.Text(v.office), l.Answered(), len(l.Answers))
	if *fill && len(l.Missing()) > 0 {
		return g.fill(ctx, rd, l, v, problem)
	}
	return problem
}

// fill copies every envelope a host lacks across to it, as spec section 9
// allows anyone holding a verified carrier to: submitted to that host
// alone, and counted once a lookup there answers it. It returns problem
// unless the listing's only problem was the hosts' disagreement and every
// copy was confirmed.
func (g *global) fill(ctx context.Context, rd *reader.Client, l *reader.Listing, v view, problem error) error {
	failed := 0
	for host, txids := range l.Missing() {
		set := &unicast.Set{Hosts: []string{host}, Need: 1, Retries: send.Retries, HTTP: httpClient, Note: g.say, Verbose: g.verbose}
		filled := 0
		for _, id := range txids {
			it := l.Find(id)
			e := it.Envelope()
			q := reader.Query{"office": v.office, "to": hex.EncodeToString(e.To), "box": e.Box,
				"after": fmt.Sprintf("%d:%s", e.Created, strings.Repeat("0", 64))}
			confirm := func(ctx context.Context, host string) (bool, error) { return rd.Held(ctx, host, q, id) }
			o := set.Send(ctx, "envelope "+id, send.Topic(v.office), it.Beef, 1, confirm)
			if err := o.Err(); err != nil {
				failed++
				g.say("host %s: envelope %s NOT FILLED: %v", host, id, err)
				continue
			}
			filled++
		}
		fmt.Fprintf(g.stdout, "host %s: filled %d of %d envelope(s) it lacked\n", host, filled, len(txids))
	}
	if failed > 0 {
		return incomplete("%d envelope(s) not filled; the hosts still disagree", failed)
	}
	var ee *exitError
	if errors.As(problem, &ee) && ee.code == exitIncomplete && l.Answered() == len(l.Answers) && len(l.Refusals()) == 0 {
		return nil
	}
	return problem
}

const readHelp = `usage: bbox read [txid...] [-box BOX | -from KEY] [-office OFFICE]

Read envelopes to this identity: every open envelope in the view, or the
ones named. Each is checked as list checks it, then decrypted and checked
as the recipient's rules say, in order (spec section 4.7): a message that
cannot be decrypted is shown as UNDECRYPTABLE, never as empty; a payment
inside is checked (its shape, its timing, its outputs, its proof against
the headers) and shown as acceptable or refused, and is taken into the
wallet only by bbox internalize. Every field written by someone else is
filtered before it reaches the terminal. What is read is recorded in the
home, for ack and internalize.`

func cmdRead(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("read", readHelp)
	vf := g.viewFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	for _, id := range pos {
		if _, err := reader.Hash(id); err != nil {
			return usage("%v", err)
		}
	}
	h, err := g.openHome()
	if err != nil {
		return err
	}
	defer h.close()
	me := h.e.Signer().Identity.Compressed()
	if *vf.to != "" {
		return usage("read decrypts with this home's key; -to is for list")
	}
	v, err := g.resolve(vf, me)
	if err != nil {
		return err
	}
	rd, err := g.reader()
	if err != nil {
		return err
	}
	l, problem := g.listing(ctx, rd, v)
	if l == nil {
		return problem
	}
	items := l.Items
	if len(pos) > 0 {
		items = nil
		for _, id := range pos {
			it := l.Find(id)
			if it == nil {
				return incomplete("no host answers %s in the %s: an envelope that is acknowledged, expired, retracted or never sent is not answered by a free question (bbox history asks for what a host keeps)", id, v)
			}
			items = append(items, it)
		}
	}
	var bad error
	for _, it := range items {
		m := reader.Open(ctx, h.e, g.cfg.Originator, it, rd.Headers, time.Now())
		g.printMessage(m, hostsOf(l, it.Txid, len(rd.Hosts)))
		if m.Err != nil {
			bad = refused("a message was refused by the recipient's checks")
		}
		e := it.Envelope()
		rec := state.Received{Txid: it.Txid, Office: e.Office, From: hex.EncodeToString(e.From), Box: e.Box, Created: e.Created, Expires: e.Expires}
		if m.Payment != nil {
			rec.Paid, rec.Beef = m.Paid(), hex.EncodeToString(it.Beef)
		}
		h.st.Remember(rec)
	}
	if err := h.st.Save(); err != nil {
		return err
	}
	if bad != nil {
		return bad
	}
	return problem
}

// printMessage prints one message, every field someone else wrote
// filtered.
func (g *global) printMessage(m *reader.Message, hosts string) {
	e := m.Item.Envelope()
	out := g.stdout
	fmt.Fprintf(out, "envelope %s\n", m.Item.Txid)
	fmt.Fprintf(out, "office   %s\n", termsafe.Text(e.Office))
	fmt.Fprintf(out, "from     %s\n", hex.EncodeToString(e.From))
	fmt.Fprintf(out, "box      %s\n", termsafe.Text(e.Box))
	fmt.Fprintf(out, "created  %s\n", boxrec.RFC3339(e.Created))
	if e.Expires > 0 {
		fmt.Fprintf(out, "expires  %s\n", boxrec.RFC3339(e.Expires))
	}
	if hosts != "" {
		fmt.Fprintf(out, "hosts    %s\n", hosts)
	}
	if m.Err != nil {
		label := boxrec.Reason(m.Err)
		if errors.Is(m.Err, boxrec.ErrUndecryptable) {
			fmt.Fprintf(out, "message  UNDECRYPTABLE (%s): the sender wrote something this key cannot read\n\n", label)
			return
		}
		fmt.Fprintf(out, "message  REFUSED (%s): %s\n\n", label, termsafe.Text(m.Err.Error()))
		return
	}
	switch {
	case m.PayErr != nil:
		fmt.Fprintf(out, "payment  REFUSED (%s): the message stands, the payment is not taken\n", boxrec.Reason(m.PayErr))
	case m.Payment != nil:
		fmt.Fprintf(out, "payment  %d sat in %s: acceptable until %s; take it with bbox internalize %s\n",
			m.Paid(), m.Payment.TxID(), boxrec.RFC3339(e.Expires-boxrec.PaymentMargin), m.Item.Txid)
	}
	for _, r := range m.Plain.Refs {
		enc := ""
		if r.Key != nil {
			enc = " encrypted"
		}
		fmt.Fprintf(out, "ref      %s %d bytes sha256 %s%s\n", termsafe.Text(r.URL), r.Length, hex.EncodeToString(r.SHA256[:]), enc)
	}
	fmt.Fprintln(out)
	if m.Plain.Body != nil {
		body := termsafe.Text(*m.Plain.Body)
		fmt.Fprint(out, body)
		if !strings.HasSuffix(body, "\n") {
			fmt.Fprintln(out)
		}
	}
	fmt.Fprintln(out)
}

// purse is the home's wallet with the pool's payer, the settlement leg and
// the header source: what internalize, history and payee settle pay and
// take through.
func (g *global) purse(ctx context.Context, h *home, hc chaintracker.ChainTracker, maxPay uint64) (*purse.Purse, error) {
	_, asset, err := g.node(false)
	if err != nil {
		return nil, err
	}
	settler, _, err := g.settler()
	if err != nil {
		return nil, err
	}
	tip, err := asset.BestHeader(ctx)
	if err != nil {
		return nil, fmt.Errorf("node tip: %w", err)
	}
	send.CollectChange(ctx, h.e.Pool, asset)
	s := h.e.Signer()
	newPayer := func() *producer.Payer {
		return &producer.Payer{Pool: h.e.Pool, Tip: tip.Height, Keys: map[string]*bwallet.Signer{s.IdentityHex(): s},
			Settler: settler, Asset: asset, Fees: mint.DefaultFees, Poll: poll, Note: g.say}
	}
	return &purse.Purse{Embedded: h.e, NewPayer: newPayer, Fees: mint.DefaultFees, MaxPay: maxPay,
		Settler: settler, Asset: asset, Headers: hc, Wait: 10 * time.Minute, Poll: poll}, nil
}

const internalizeHelp = `usage: bbox internalize <txid> [-no-ack] [-office OFFICE]

Take the payment inside an envelope to this identity into the wallet. The
envelope is fetched and checked again, and the payment checked in the order
of spec section 4.7, at this moment: it is taken only while it is more than
an hour before the envelope expires. The wallet broadcasts it through the
settlement leg, waits for its proof, and adds its outputs to the pool, so
it is spendable. The envelope is then acknowledged with a receipt, unless
-no-ack. A payment whose inputs the sender spent elsewhere is refused by
the network and reported as reclaimed.`

func cmdInternalize(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("internalize", internalizeHelp)
	vf := g.viewFlags(fs)
	noAck := fs.Bool("no-ack", false, "do not acknowledge the envelope")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usage("internalize <txid>")
	}
	if _, err := reader.Hash(pos[0]); err != nil {
		return usage("%v", err)
	}
	h, err := g.openHome()
	if err != nil {
		return err
	}
	defer h.close()
	if r := h.st.ReceivedByTxid(pos[0]); r != nil && r.Internalized != "" {
		fmt.Fprintf(g.stdout, "the payment of %s is internalized already: %s\n", pos[0], r.Internalized)
		return nil
	}
	v, err := g.resolve(vf, h.e.Signer().Identity.Compressed())
	if err != nil {
		return err
	}
	rd, err := g.reader()
	if err != nil {
		return err
	}
	l, problem := g.listing(ctx, rd, v)
	if l == nil {
		return problem
	}
	it := l.Find(pos[0])
	if it == nil {
		// Acknowledged, or no longer answered: the carrier read kept is
		// checked again, against this home's headers, as a host's answer is.
		kept := h.st.ReceivedByTxid(pos[0])
		if kept == nil || kept.Beef == "" {
			return incomplete("no host answers %s in the %s: acknowledged, expired or retracted, or never sent (read an envelope before it is acknowledged, and its payment is kept for internalize)", pos[0], v)
		}
		raw, err := hex.DecodeString(kept.Beef)
		if err != nil {
			return err
		}
		if it, err = reader.Verify(ctx, raw, kept.Office, boxrec.TxEnvelope, h.e.Signer().Identity.Compressed(), rd.Headers); err != nil {
			return refused("%s: the carrier kept when it was read does not verify now (%s): %v", pos[0], boxrec.Reason(err), err)
		}
		g.say("%s: no host answers it now; using the carrier kept when it was read", pos[0])
	}
	m := reader.Open(ctx, h.e, g.cfg.Originator, it, rd.Headers, time.Now())
	switch {
	case m.Err != nil:
		return refused("%s: the message is refused (%s): %v", pos[0], boxrec.Reason(m.Err), m.Err)
	case m.Plain.Payment == nil:
		return usage("%s carries no payment", pos[0])
	case m.PayErr != nil:
		return refused("%s: the payment is refused (%s): %v", pos[0], boxrec.Reason(m.PayErr), m.PayErr)
	}
	e := it.Envelope()
	p, err := g.purse(ctx, h, rd.Headers, 0)
	if err != nil {
		return err
	}
	beef, err := m.Payment.AtomicBEEF(false)
	if err != nil {
		return err
	}
	var outs []wallet.InternalizeOutput
	for _, o := range m.Plain.Payment.Outputs {
		r, err := purse.Remittance(m.Plain.Payment.DerivationPrefix, o.DerivationSuffix, hex.EncodeToString(e.From))
		if err != nil {
			return refused("%v", err)
		}
		outs = append(outs, wallet.InternalizeOutput{OutputIndex: o.OutputIndex, Protocol: wallet.InternalizeProtocolWalletPayment, PaymentRemittance: r})
	}
	if _, err := p.InternalizeAction(ctx, wallet.InternalizeActionArgs{Tx: beef, Description: "bbox payment received",
		Labels: []string{"bbox", "payment"}, Outputs: outs}, g.cfg.Originator); err != nil {
		if strings.Contains(err.Error(), "spent its inputs") {
			return refused("%v: the payment is reclaimed or double-spent", err)
		}
		return err
	}
	rec := h.st.Remember(state.Received{Txid: it.Txid, Office: e.Office, From: hex.EncodeToString(e.From), Box: e.Box, Created: e.Created, Expires: e.Expires, Paid: m.Paid()})
	rec.Internalized, rec.Beef = m.Payment.TxID().String(), ""
	if err := h.st.Save(); err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "internalized %d sat from %s: payment %s, pool %d output(s), %d sat\n",
		m.Paid(), hex.EncodeToString(e.From), rec.Internalized, h.e.Pool.Count(), h.e.Pool.Balance())
	if *noAck || rec.Acked != "" {
		return nil
	}
	// A recipient that internalizes a payment acknowledges its envelope.
	eng, err := g.engine(ctx, h, g.cfg.TreeCount)
	if err != nil {
		return fmt.Errorf("the payment is internalized; acknowledging the envelope: %w (bbox ack %s)", err, it.Txid)
	}
	defer func() { _ = eng.Close(context.WithoutCancel(ctx)) }()
	rc, err := eng.Receipt(ctx, e.Office, []string{it.Txid})
	if err != nil {
		return fmt.Errorf("the payment is internalized; acknowledging the envelope: %w", err)
	}
	fmt.Fprintf(g.stdout, "acknowledged %s with receipt %s\n", it.Txid, rc.Txid)
	g.tallies(eng)
	return nil
}

const historyHelp = `usage: bbox history [-at URL] [-after CURSOR] [-max-sats N] [-office OFFICE]

Ask a host the priced history question (spec section 7.3): the envelopes
to this identity it still keeps that are no longer open (acknowledged,
expired, or out of the answer window). It is asked at the host's terms
route (-at, default the configured history_host) over BRC-104
authentication; the host answers 402 with its price, and the question is
asked again with a BRC-29 payment to the host from this home's pool
(BRC-105) as output 0, which the host records unbroadcast and its payee
settles. A price over -max-sats is not paid.
One payment buys one answer page; -after asks for the page after a cursor
<created>:<txid>. Every envelope answered is checked as list checks it.`

func cmdHistory(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("history", historyHelp)
	at := fs.String("at", "", "the host's terms route base URL (default history_host)")
	after := fs.String("after", "", "the page after this cursor <created>:<txid>")
	maxSats := fs.Uint64("max-sats", limits.DefaultMaxPrice, "the most one question is paid")
	office := fs.String("office", "", "the office (default the configured office)")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usage("history takes no arguments")
	}
	base := *at
	if base == "" {
		base = g.cfg.HistoryHost
	}
	if base == "" {
		return usage("no host to ask: set history_host (or -at) to the host's terms route base URL")
	}
	if err := checkOrigin(base); err != nil {
		return err
	}
	off, err := g.office(*office)
	if err != nil {
		return err
	}
	if *after != "" {
		if _, _, ok := boxrec.ParseAfter(*after); !ok {
			return usage("-after %q is not a cursor <created>:<txid>", termsafe.Abbrev(*after))
		}
	}
	h, err := g.openHome()
	if err != nil {
		return err
	}
	defer h.close()
	hc, err := g.headerClient()
	if err != nil {
		return err
	}
	p, err := g.purse(ctx, h, hc, *maxSats)
	if err != nil {
		return err
	}
	me := h.e.Signer().Identity.Compressed()
	q := map[string]string{"office": off, "history": hex.EncodeToString(me)}
	if *after != "" {
		q["after"] = *after
	}
	body, _ := json.Marshal(lookup.Question{Service: boxrec.LookupService, Query: q})
	af := clients.New(p, clients.WithoutLogging(), clients.WithHttpClient(&http.Client{Timeout: g.cfg.Timeout}))
	resp, err := af.Fetch(ctx, strings.TrimRight(base, "/")+"/lookup", &clients.SimplifiedFetchRequestOptions{
		Method: http.MethodPost, Headers: map[string]string{"content-type": "application/json"}, Body: body})
	if err != nil {
		p.Refund()
		return fmt.Errorf("history at %s: %w", base, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		p.Refund()
		return err
	}
	if resp.StatusCode != http.StatusOK {
		p.Refund()
		return fmt.Errorf("history at %s: status %d: %s", base, resp.StatusCode, termsafe.Abbrev(firstLine(string(raw))))
	}
	paid := resp.Header.Get("x-bsv-payment-satoshis-paid")
	if paid != "" {
		tx := p.Settle()
		if tx != nil {
			fmt.Fprintf(g.stdout, "paid %s sat to %s in %s, recorded by the host for its payee to settle\n", paid, termsafe.Abbrev(resp.Header.Get("x-bsv-auth-identity-key")), tx.TxID())
		}
	} else {
		p.Refund()
	}
	var a lookup.Answer
	if err := json.Unmarshal(raw, &a); err != nil || a.Type != lookup.TypeOutputList {
		return fmt.Errorf("history at %s: the answer is not an output-list", base)
	}
	var bad error
	var last string
	for _, o := range a.Outputs {
		it, err := reader.Verify(ctx, o.Beef, off, boxrec.TxEnvelope, me, hc)
		if err != nil {
			g.say("REFUSED an answered output (%s): %v", boxrec.Reason(err), err)
			bad = refused("the host answered what does not verify; it is not shown")
			continue
		}
		e := it.Envelope()
		fmt.Fprintf(g.stdout, "%s  %s  from %s  box %s  %dB\n", boxrec.RFC3339(e.Created), it.Txid, hex.EncodeToString(e.From), termsafe.Text(e.Box), len(e.Content))
		last = strconv.FormatUint(e.Created, 10) + ":" + it.Txid
	}
	g.say("%d envelope(s) the host keeps that are no longer open", len(a.Outputs))
	if len(a.Outputs) == boxrec.PageEnvelopes && last != "" {
		g.say("a full page: the next is bbox history -after %s", last)
	}
	return bad
}

// checkOrigin refuses a terms base that is not an origin (spec section
// 7.4): the terms document, the BRC-104 handshake and the priced /lookup
// all sit at the root of the origin.
func checkOrigin(base string) error {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return usage("%q is not an origin: the terms route is at the root of its own origin, where BRC-104 shakes hands (docs/host.md)", termsafe.Abbrev(base))
	}
	return nil
}

const termsHelp = `usage: bbox terms [URL]

Fetch and print a host's terms document (spec section 7.4) from the base
URL of its ls_bbox, an origin with no path (default the configured
history_host): the classes it
prices and the price of one question of each. A host that serves none
charges for nothing. The document is informative: the 402 is what a
client pays against, and a free class is never paid for, whatever a
document says.`

func cmdTerms(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("terms", termsHelp)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usage("terms [URL]")
	}
	base := g.cfg.HistoryHost
	if len(pos) == 1 {
		base = pos[0]
	}
	if base == "" {
		return usage("no host: give its ls_bbox base URL, or set history_host")
	}
	if err := checkOrigin(base); err != nil {
		return err
	}
	t, err := reader.FetchTerms(ctx, termsClient(g.cfg.Timeout), base)
	if errors.Is(err, reader.ErrNoTerms) {
		fmt.Fprintf(g.stdout, "%s serves no terms document: it charges for nothing\n", base)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "service %s, terms %d\n", termsafe.Text(t.Service), t.Terms)
	for _, c := range t.Classes {
		cls := slices.IndexFunc(boxrec.Classes, func(x boxrec.Class) bool { return x.Name == c.Class })
		switch {
		case cls < 0:
			continue
		case boxrec.Classes[cls].Free:
			fmt.Fprintf(g.stdout, "%-14s FREE CLASS WITH A PRICE (%d sat): outside the specification; never paid\n", c.Class, c.Satoshis)
		default:
			fmt.Fprintf(g.stdout, "%-14s %d sat a question\n", c.Class, c.Satoshis)
		}
	}
	return nil
}
