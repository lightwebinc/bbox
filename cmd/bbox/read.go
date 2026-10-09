package main

import (
	"bytes"
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
	"sync"
	"time"

	clients "github.com/bsv-blockchain/go-sdk/auth/clients/authhttp"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/acceptance"
	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/lookup"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/purse"
	"github.com/lightwebinc/bcommon/termsafe"

	"github.com/lightwebinc/bbox/boxrec"
	"github.com/lightwebinc/bbox/internal/limits"
	"github.com/lightwebinc/bbox/internal/state"
	"github.com/lightwebinc/bbox/reader"
	"github.com/lightwebinc/bbox/send"
	"github.com/lightwebinc/bcommon/unicast"
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
	if v.box == "" && v.from != "" {
		return reader.FromQuery(v.office, v.to, v.from)
	}
	return reader.BoxQuery(v.office, v.to, v.box)
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
// the same verified set, and exit 3 when a host answered something that
// does not verify (named, and not shown), the hosts disagree, or one could
// not be asked: what is shown comes from what verified, and one host at
// fault does not make a check fail.
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
	refusals := l.Refusals()
	for _, r := range refusals {
		g.say("host %s: REFUSED %s (%s): what this host answered does not verify, so it is not shown: %v", r.Host, r.Txid, r.Reason, r.Err)
		problem = incomplete("a host answered what does not verify; it is not shown")
	}
	if len(refusals) > 0 && len(l.Items) == 0 {
		// The hosts answered, and nothing they answered holds against this
		// home's headers: as likely the header source as the hosts.
		problem = incomplete("the hosts answered %d envelope(s) and none verifies against the header source %s: nothing is shown. A header source that does not answer fails every answer the same way (bbox doctor), and a proof a reorganization displaced is replaced only from a chain view (config key chain)", len(refusals), g.cfg.Headers())
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
never merged silently (exit 3), and a host that answers what does not
verify is named and what it answered is not shown (exit 3). One line per
envelope: created, txid,
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
			field(e.Box), len(e.Content))
		if e.Expires > 0 {
			line += "  expires " + boxrec.RFC3339(e.Expires)
		}
		if hs := hostsOf(l, it.Txid, len(rd.Hosts)); hs != "" {
			line += "  [" + hs + "]"
		}
		fmt.Fprintln(g.stdout, line)
	}
	g.say("%d envelope(s) in the %s of %s, %d of %d host(s) answering", len(l.Items), v, field(v.office), l.Answered(), len(l.Answers))
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
			if it == nil && l.Refused(id) {
				return incomplete("%s: the hosts answered it, each answer named above, and none verifies against the header source %s: nothing is shown, and it is not absent. Ask other hosts, or check the header source (bbox doctor)", id, g.cfg.Headers())
			}
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
	fmt.Fprintf(out, "office   %s\n", field(e.Office))
	fmt.Fprintf(out, "from     %s\n", hex.EncodeToString(e.From))
	fmt.Fprintf(out, "box      %s\n", field(e.Box))
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
		fmt.Fprintf(out, "message  REFUSED (%s): %s\n\n", label, field(m.Err.Error()))
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
		fmt.Fprintf(out, "ref      %s %d bytes sha256 %s%s\n", field(r.URL), r.Length, hex.EncodeToString(r.SHA256[:]), enc)
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
	chain, err := g.chain(hc)
	if err != nil {
		return nil, err
	}
	settler, _, err := g.settler(chain)
	if err != nil {
		return nil, err
	}
	fees, err := g.fees(ctx)
	if err != nil {
		return nil, err
	}
	tip, err := hc.CurrentHeight(ctx)
	if err != nil {
		return nil, fmt.Errorf("chain tip: %w", err)
	}
	send.CollectChange(ctx, h.e.Pool, chain)
	if h.st.KeepPayments(h.e.Pool.UnprovenTxids()) {
		if err := h.st.Save(); err != nil {
			return nil, err
		}
	}
	s := h.e.Signer()
	newPayer := func() *producer.Payer {
		return &producer.Payer{Pool: h.e.Pool, Tip: tip, Keys: map[string]*bwallet.Signer{s.IdentityHex(): s},
			Settler: settler, Chain: chain, Fees: fees, Poll: poll, Note: g.say}
	}
	return &purse.Purse{Embedded: h.e, NewPayer: newPayer, Fees: fees, MaxPay: maxPay,
		Settler: settler, Chain: chain, Headers: hc, Wait: 10 * time.Minute, Poll: poll}, nil
}

const internalizeHelp = `usage: bbox internalize <txid> [-no-ack] [-office OFFICE]

Take the payment inside an envelope to this identity into the wallet. The
envelope is fetched and checked again, and the payment checked in the order
of spec section 4.7, at this moment: it is taken only while it is more than
an hour before the envelope expires. It is then broadcast through the
settlement leg. A payment at or below accept_threshold_sats, within the
sender's and the total limits, is received as soon as arcade says the
network took it and the node shows its inputs spent by it alone: the
envelope is acknowledged at once, and the next internalize of the same
txid waits for its proof and adds it to the pool. A larger payment, or one
the network has not clearly taken, waits for its proof first, as before.
The envelope is acknowledged with a receipt, unless -no-ack. A payment
whose inputs the sender spent elsewhere is refused by the network and
reported as reclaimed.`

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
	if it == nil && l.Refused(pos[0]) {
		return incomplete("%s: the hosts answered it, each answer named above, and none verifies against the header source %s: nothing is taken, and it is not absent. Ask other hosts, or check the header source (bbox doctor)", pos[0], g.cfg.Headers())
	}
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
		if it, err = rd.Verify(ctx, raw, kept.Office, boxrec.TxEnvelope, h.e.Signer().Identity.Compressed()); err != nil {
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
	ia, err := m.Internalize("bbox payment received", []string{"bbox", "payment"})
	if err != nil {
		return refused("%v", err)
	}
	rec := h.st.Remember(state.Received{Txid: it.Txid, Office: e.Office, From: hex.EncodeToString(e.From), Box: e.Box, Created: e.Created, Expires: e.Expires, Paid: m.Paid()})
	if rec.Accepted == "" {
		v, err := g.acceptPayment(ctx, p, rd.Headers, m.Payment, hex.EncodeToString(e.From), ia)
		if err != nil {
			return err
		}
		switch v.Decision {
		case acceptance.Refuse:
			return refused("%s: the payment is refused: %s", pos[0], v)
		case acceptance.Fast:
			rec.Accepted = m.Payment.TxID().String()
			if rec.Beef == "" {
				rec.Beef = hex.EncodeToString(it.Beef)
			}
			if err := h.st.Save(); err != nil {
				return err
			}
			fmt.Fprintf(g.stdout, "received %d sat from %s: payment %s, taken on the network's acceptance (%s); run bbox internalize %s again once it is mined to add it to the pool\n",
				m.Paid(), hex.EncodeToString(e.From), rec.Accepted, v.Reason, it.Txid)
			return g.ackInternalized(ctx, h, *noAck, rec, e.Office, it.Txid)
		}
		g.say("payment %s: %s; waiting for its block", m.Payment.TxID(), v)
	}
	if _, err := p.InternalizeAction(ctx, ia, g.cfg.Originator); err != nil {
		var re *purse.RefusedError
		if errors.As(err, &re) || strings.Contains(err.Error(), "spent its inputs") {
			return refused("%v: the payment is reclaimed or double-spent", err)
		}
		return payWords(err, 0)
	}
	rec.Internalized, rec.Beef = m.Payment.TxID().String(), ""
	if err := h.st.Save(); err != nil {
		return err
	}
	fmt.Fprintf(g.stdout, "internalized %d sat from %s: payment %s, pool %d output(s), %d sat\n",
		m.Paid(), hex.EncodeToString(e.From), rec.Internalized, h.e.Pool.Count(), h.e.Pool.Balance())
	return g.ackInternalized(ctx, h, *noAck, rec, e.Office, it.Txid)
}

// acceptPayment holds a payment to bcommon's acceptance rule before it is
// taken: checked (outputs, finality, no more out than in, SPV), broadcast,
// and decided on value, arcade's verdict and the node's spend view. With
// no arcade settlement leg or no node every payment is held for its block.
func (g *global) acceptPayment(ctx context.Context, p *purse.Purse, hc chaintracker.ChainTracker, tx *transaction.Transaction, from string, ia wallet.InternalizeActionArgs) (acceptance.Verdict, error) {
	pays := make([]acceptance.Output, 0, len(ia.Outputs))
	for _, o := range ia.Outputs {
		if int(o.OutputIndex) >= len(tx.Outputs) {
			return acceptance.Verdict{Decision: acceptance.Refuse, Reason: acceptance.ReasonMalformed}, nil
		}
		out := tx.Outputs[o.OutputIndex]
		pays = append(pays, acceptance.Output{Vout: o.OutputIndex, Script: *out.LockingScript, Sats: out.Satoshis})
	}
	v := &acceptance.Verifier{Policy: g.cfg.AcceptPolicy(), Exposure: acceptance.NewExposure(), Headers: hc}
	if _, arc, err := g.settler(p.Chain); err == nil && arc != nil && p.Chain != nil {
		v.Settler, v.Status, v.Spends, v.Proofs = arc, []acceptance.StatusSource{arc}, p.Chain, p.Chain
	}
	return v.Accept(ctx, acceptance.Payment{Tx: tx, Payer: from, Pays: pays})
}

// ackInternalized acknowledges an envelope whose payment was taken, unless
// -no-ack or it is acknowledged already.
func (g *global) ackInternalized(ctx context.Context, h *home, noAck bool, rec *state.Received, office, txid string) error {
	if noAck || rec.Acked != "" {
		return nil
	}
	// A recipient that internalizes a payment acknowledges its envelope.
	eng, err := g.engine(ctx, h, g.cfg.TreeCount)
	if err != nil {
		return fmt.Errorf("the payment is taken; acknowledging the envelope: %w (bbox ack %s)", err, txid)
	}
	defer func() { _ = eng.Close(context.WithoutCancel(ctx)) }()
	rc, err := eng.Receipt(ctx, office, []string{txid})
	if err != nil {
		return fmt.Errorf("the payment is taken; acknowledging the envelope: %w", err)
	}
	fmt.Fprintf(g.stdout, "acknowledged %s with receipt %s\n", txid, rc.Txid)
	g.tallies(eng)
	return nil
}

// limitWatch is the HTTP transport of a history question. A host gives
// each BRC-104 session a budget of signed responses and refuses a request
// over it 429 without a signature, which the SDK's client reports only as a
// failed authentication; the transport sees the status and the wait the
// host named.
type limitWatch struct {
	next http.RoundTripper
	mu   sync.Mutex
	hit  bool
	wait time.Duration
	// held is the host's word when it held a payment for confirmation
	// (402 ERR_PAYMENT_HELD, which carries no BRC-105 headers).
	held string
}

// The wait a host names is taken, within these bounds.
const (
	minLimitWait = time.Second
	maxLimitWait = 30 * time.Second
)

func (w *limitWatch) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := w.next.RoundTrip(r)
	if err == nil && resp.StatusCode == http.StatusPaymentRequired && resp.Header.Get("x-bsv-payment-version") == "" {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
		var e struct{ Code, Description string }
		if json.Unmarshal(body, &e) == nil && e.Code == "ERR_PAYMENT_HELD" {
			w.mu.Lock()
			w.held = e.Description
			w.mu.Unlock()
		}
	}
	if err != nil || resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("x-bsv-auth-version") != "" || strings.HasSuffix(r.URL.Path, "/.well-known/auth") {
		return resp, err
	}
	wait := minLimitWait
	if s, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && s > 0 {
		wait = min(time.Duration(s)*time.Second, maxLimitWait)
	}
	w.mu.Lock()
	w.hit, w.wait = true, wait
	w.mu.Unlock()
	return resp, err
}

// taken reports a refusal seen since the last call, and the wait named.
func (w *limitWatch) taken() (time.Duration, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	hit := w.hit
	w.hit = false
	return w.wait, hit
}

// heldWhy is the host's word on a payment it held, or "".
func (w *limitWatch) heldWhy() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.held
}

// errLimited is a request refused for its session's budget twice.
var errLimited = errors.New("the host limits how fast one session is answered")

// recordPayment records a payment this home made, so the home knows its
// own coin is spent and where its change is. It is recorded before it is
// sent: from
// the moment it leaves this process the host holds a valid transaction
// that spends the coin.
func recordPayment(h *home, tx *transaction.Transaction) error {
	beef, err := funding.BEEF(tx)
	if err != nil {
		return err
	}
	h.st.Payments = append(h.st.Payments, state.Payment{Txid: tx.TxID().String(), Beef: hex.EncodeToString(beef)})
	return h.st.Save()
}

// broadcastPayment hands a payment this home made, and a host did not
// answer for, to the settlement leg: it is this home's own transaction, and
// once it mines its change is an ordinary coin whether or not the host's
// payee ever settles it. A leg that does not take it changes nothing: the
// payee can broadcast the same bytes.
func (g *global) broadcastPayment(ctx context.Context, p *purse.Purse, tx *transaction.Transaction) {
	if p.Settler == nil {
		return
	}
	if err := p.Settler.Submit(ctx, tx); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already") {
		g.say("payment %s: not broadcast from here (%s); the payee broadcasts it when it settles", tx.TxID().String(), firstLine(err.Error()))
	}
}

// errPaidOnce refuses a second payment for one question.
var errPaidOnce = errors.New("one question is paid for once")

// oncePurse is a purse that pays once, and records what it pays before it
// is sent. The SDK's AuthFetch answers every 402 with a new payment,
// several times over: a host that keeps answering 402 would be handed a
// signed payment each time, and each is a transaction it can broadcast.
// One question is one payment. And the SDK sends a payment as soon as the
// wallet returns it, so the wallet is where it must be recorded: once the
// action returns, the payment is made, its coin spent and its change in the
// pool, whatever the host then answers.
type oncePurse struct {
	*purse.Purse
	record func(tx *transaction.Transaction) error
	// tx is the payment made, and sats what it pays the host.
	tx   *transaction.Transaction
	sats uint64
}

// CreateAction makes the one payment and records it, and refuses any after
// it.
func (o *oncePurse) CreateAction(ctx context.Context, args wallet.CreateActionArgs, originator string) (*wallet.CreateActionResult, error) {
	if o.tx != nil {
		return nil, errPaidOnce
	}
	res, err := o.Purse.CreateAction(ctx, args, originator)
	if err != nil {
		return nil, err
	}
	tx := o.Purse.Settle()
	if tx == nil {
		return nil, errors.New("the wallet made no payment")
	}
	if err := o.record(tx); err != nil {
		return nil, fmt.Errorf("the payment %s could not be recorded in the home, and is not sent: %w", tx.TxID(), err)
	}
	o.tx, o.sats = tx, args.Outputs[0].Satoshis
	return res, nil
}

// budget is what a command that asks several priced questions may pay: at
// most per for one question, and at most total for all of them. A host
// sets its own prices and its own pages; the command sets what they may
// add up to.
type budget struct {
	per, total, spent uint64
}

// next is the most the next question may be paid, or an error once the
// total is spent.
func (b *budget) next() (uint64, error) {
	if b.spent >= b.total {
		return 0, fmt.Errorf("this command has paid %d sat, its budget (-budget): it pays for no further question", b.spent)
	}
	return min(b.per, b.total-b.spent), nil
}

func (b *budget) paid(sats uint64) { b.spent += sats }

// askLimited asks and, when the host refused the request for its session's
// budget before any payment was made, waits as the host said and asks once
// more. Once a payment was made nothing is asked again: asking again would
// take a second payment, and one question is paid for once.
func (g *global) askLimited(ctx context.Context, paid func() bool, watch *limitWatch, ask func() (*http.Response, error)) (*http.Response, error) {
	resp, err := ask()
	wait, limited := watch.taken()
	if err == nil || !limited {
		return resp, err
	}
	if paid() {
		return nil, errLimited
	}
	g.say("the host limits how fast one session is answered; waiting %s and asking once more", wait)
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	resp, err = ask()
	if _, again := watch.taken(); err != nil && again {
		return nil, errLimited
	}
	return resp, err
}

// ask asks a host's terms route one question of a priced class (spec
// section 7.3): over BRC-104, answered 402 with the price, and asked again
// with a BRC-29 payment to the host from this home's pool (BRC-105) as
// output 0, which the host records and its payee settles. A price over
// maxSats is not paid; a host that prices nothing answers at no price. It
// returns what was paid, answered or not: a payment that left this process
// is made.
func (g *global) ask(ctx context.Context, h *home, hc chaintracker.ChainTracker, base string, q map[string]string, maxSats uint64) ([]lookup.Output, uint64, error) {
	p, err := g.purse(ctx, h, hc, maxSats)
	if err != nil {
		return nil, 0, err
	}
	// The pool is recorded before a coin is taken from it, and a coin a
	// run that stopped took and never recorded goes back (send.Journal).
	send.Journal(h.st, h.e.Pool, send.Reconcile(ctx, h.st, h.e.Pool, p.Chain, g.say))
	if err := h.st.Save(); err != nil {
		return nil, 0, err
	}
	defer func() {
		send.Unjournal(h.st)
		_ = h.st.Save()
	}()
	once := &oncePurse{Purse: p, record: func(tx *transaction.Transaction) error { return recordPayment(h, tx) }}
	body, _ := json.Marshal(lookup.Question{Service: boxrec.LookupService, Query: q})
	watch := &limitWatch{next: http.DefaultTransport}
	af := clients.New(once, clients.WithoutLogging(), clients.WithHttpClient(&http.Client{Timeout: g.cfg.Timeout, Transport: watch}))
	resp, ferr := g.askLimited(ctx, func() bool { return once.tx != nil }, watch, func() (*http.Response, error) {
		return af.Fetch(ctx, strings.TrimRight(base, "/")+"/lookup", &clients.SimplifiedFetchRequestOptions{
			Method: http.MethodPost, Headers: map[string]string{"content-type": "application/json"}, Body: body})
	})
	var ans []byte
	status, paid, payee := 0, "", ""
	if ferr == nil {
		defer resp.Body.Close()
		status, paid, payee = resp.StatusCode, resp.Header.Get("x-bsv-payment-satoshis-paid"), resp.Header.Get("x-bsv-auth-identity-key")
		ans, ferr = io.ReadAll(io.LimitReader(resp.Body, limits.MaxAnswer+1))
		if ferr == nil && len(ans) > limits.MaxAnswer {
			ferr = fmt.Errorf("the answer is over %d bytes and is not read", limits.MaxAnswer)
		}
	}
	answered := ferr == nil && status == http.StatusOK
	switch {
	case once.tx == nil:
		// No payment was made: there is nothing to keep.
	case answered && paid != "":
		// The host recorded it, unbroadcast, and its payee broadcasts it
		// when it settles: the change is held until then.
		fmt.Fprintf(g.stdout, "paid %s sat to %s in %s, broadcast by the host for its payee to settle\n", termsafe.Abbrev(paid), termsafe.Abbrev(payee), once.tx.TxID())
	default:
		// A payment was handed to the host and the host did not answer for
		// it. Its coins are NOT given back to the pool: the host holds a
		// valid transaction that spends them, and a coin spent again would
		// make this home's next transaction a double spend. The payment is
		// kept as made, and broadcast from here, so that its change does
		// not wait on a host that may never settle it.
		g.say("a payment of this home's, %s, was sent to %s and the question was not answered: it left this process, so it is kept as made and its coins are not reused; the host's payee can still settle it", once.tx.TxID(), base)
		g.broadcastPayment(ctx, p, once.tx)
	}
	switch {
	case once.tx != nil && watch.heldWhy() != "":
		return nil, once.sats, incomplete("history at %s: the host broadcast payment %s and holds it for confirmation: %s", base, once.tx.TxID(), termsafe.Abbrev(watch.heldWhy()))
	case errors.Is(ferr, errLimited) && once.tx != nil:
		return nil, once.sats, incomplete("history at %s: the host refused the paid request for its session's budget (429); nothing is asked again, since that would take a second payment; ask again later", base)
	case errors.Is(ferr, errLimited):
		return nil, once.sats, incomplete("history at %s: %v (429) twice; ask again later", base, ferr)
	case errors.Is(ferr, errPaidOnce) || (ferr == nil && status == http.StatusPaymentRequired && once.tx != nil):
		return nil, once.sats, refused("history at %s: the host asked for a payment again after one was sent; one question is paid for once", base)
	case ferr != nil:
		return nil, once.sats, fmt.Errorf("history at %s: %w", base, payWords(ferr, maxSats))
	case status != http.StatusOK:
		return nil, once.sats, fmt.Errorf("history at %s: status %d: %s", base, status, termsafe.Abbrev(firstLine(string(ans))))
	}
	var a lookup.Answer
	if err := json.Unmarshal(ans, &a); err != nil || a.Type != lookup.TypeOutputList {
		return nil, once.sats, fmt.Errorf("history at %s: the answer is not an output-list", base)
	}
	return a.Outputs, once.sats, nil
}

const historyHelp = `usage: bbox history [-at URL] [-after CURSOR] [-all] [-max-sats N] [-budget N] [-office OFFICE]

Ask a host the priced history question (spec section 7.3): the envelopes
to this identity it still keeps that are no longer open (acknowledged,
expired, or out of the answer window). It is asked at the host's terms
route (-at, default the configured history_host) over BRC-104
authentication; the host answers 402 with its price, and the question is
asked again with a BRC-29 payment to the host from this home's pool
(BRC-105) as output 0, which the host records unbroadcast and its payee
settles.

One payment buys one answer page; -after asks for the page after a cursor
<created>:<txid>, and -all for every page from there on. One question is
paid for once. The payment is recorded in the home before it is sent, and
once sent it is made, whatever the host then answers: a host that takes it
and does not answer, or asks for another, is reported, and the coin is not
reused. A price over -max-sats is not paid, and the command pays at most
-budget in all, over every page. Every envelope answered is checked as
list checks it.`

func cmdHistory(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("history", historyHelp)
	at := fs.String("at", "", "the host's terms route base URL (default history_host)")
	after := fs.String("after", "", "the page after this cursor <created>:<txid>")
	all := fs.Bool("all", false, "ask every page from there on, each one paid question")
	maxSats := fs.Uint64("max-sats", limits.DefaultMaxPrice, "the most one question is paid")
	total := fs.Uint64("budget", limits.DefaultBudget, "the most this command pays in all, over every page")
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
	// A proof a host stored that a reorganization left stale is replaced
	// by the node's current one.
	rd := &reader.Client{Headers: hc, Timeout: g.cfg.Timeout, HTTP: httpClient}
	g.currentProofs(rd)
	me := h.e.Signer().Identity.Compressed()
	q := map[string]string{"office": off, "history": hex.EncodeToString(me)}
	if *after != "" {
		q["after"] = *after
	}
	b := &budget{per: *maxSats, total: *total}
	var bad error
	n := 0
	last := ""
	seen := map[string]bool{}
	full := false
	for page := 0; page < limits.MaxPages; page++ {
		// A host sets its pages and its prices; what they add up to is
		// this command's to bound.
		per, err := b.next()
		if err != nil {
			if page == 0 {
				return usage("%v", err)
			}
			return incomplete("%v; the history is not read to its end (a larger -budget, or -after %s)", err, last)
		}
		outs, paid, err := g.ask(ctx, h, hc, base, q, per)
		b.paid(paid)
		if errors.Is(err, purse.ErrOverMaxPay) && per < *maxSats {
			// The budget, not -max-sats, is what the price is over.
			if page == 0 {
				return usage("history at %s: the price is more than the %d sat this command may pay in all (-budget)", base, *total)
			}
			return incomplete("history at %s: the next page's price is more than the %d sat left of this command's budget (-budget); the history is not read to its end (-after %s)", base, per, last)
		}
		if err != nil {
			return err
		}
		// A page holds what the contract's page holds; more is cut.
		if len(outs) > boxrec.PageEnvelopes {
			outs = outs[:boxrec.PageEnvelopes]
		}
		full = len(outs) >= historyPage
		grew := false
		for _, o := range outs {
			it, err := rd.Verify(ctx, o.Beef, off, boxrec.TxEnvelope, me)
			if err == nil {
				// A page after a cursor holds only envelopes past it: a
				// host that repeats earlier ones to fill a page, so that a
				// history takes more pages, each one paid for, is refused.
				cur := strconv.FormatUint(it.Envelope().Created, 10) + ":" + it.Txid
				if prev, ok := q["after"]; ok && !cursorAfter(cur, prev) {
					err = fmt.Errorf("a page asked after %s holds an envelope at %s", prev, cur)
				}
			}
			if err != nil {
				g.say("REFUSED an answered output (%s): %v", boxrec.Reason(err), err)
				bad = refused("the host answered what does not verify; it is not shown")
				continue
			}
			e := it.Envelope()
			cur := strconv.FormatUint(e.Created, 10) + ":" + it.Txid
			if last == "" || cursorAfter(cur, last) {
				last = cur
			}
			if seen[it.Txid] {
				continue
			}
			seen[it.Txid], grew = true, true
			n++
			fmt.Fprintf(g.stdout, "%s  %s  from %s  box %s  %dB\n", boxrec.RFC3339(e.Created), it.Txid, hex.EncodeToString(e.From), field(e.Box), len(e.Content))
		}
		if !*all || bad != nil || !grew || !full || last == "" {
			break
		}
		q["after"] = last
	}
	g.say("%d envelope(s) the host keeps that are no longer open", n)
	if full && last != "" && !*all {
		g.say("a full page: the next is bbox history -after %s", last)
	}
	if bad != nil {
		return bad
	}
	return h.st.Save()
}

// historyPage is the envelopes a full page of history holds: a shorter
// page is the last. Tests lower it.
var historyPage = boxrec.PageEnvelopes

// cursorAfter reports whether cursor a sorts strictly after cursor b.
func cursorAfter(a, b string) bool {
	ac, at, aok := boxrec.ParseAfter(a)
	bc, bt, bok := boxrec.ParseAfter(b)
	if !aok || !bok {
		return false
	}
	if ac != bc {
		return ac > bc
	}
	return at > bt
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
	fmt.Fprintf(g.stdout, "service %s, terms %d\n", field(t.Service), t.Terms)
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
