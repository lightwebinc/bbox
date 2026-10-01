# bbox user guide

How to send someone a private message that several independent hosts keep
for them, read your own box, acknowledge what you read, pay inside a
message, and be paid for answering questions. New here? Start with
[QUICKSTART.md](../QUICKSTART.md). Every command, flag and message is in
[usage.md](usage.md), every default and cap in [limits.md](limits.md),
running a host in [host.md](host.md), and the normative reference in
[spec.md](spec.md). Every key, txid, office and hostname below is
illustrative.

## 1. What this is

A mailbox someone else runs can lose your mail, show it to others, or claim
it never arrived, and you cannot tell which. bbox gives a box several
keepers and lets you check each of them. A sender seals a message to your
identity key and hands it to overlay hosts that carry your **office**. Each
host keeps a copy and answers questions about your box. Your client asks
every host it knows, checks every answer against block headers it chooses
itself, and shows you where they disagree.

What a host can do: withhold an envelope, which reading from several hosts
exposes. What it cannot do: forge an envelope, alter one, read one (the body
is encrypted to you), or answer one you acknowledged as if it were still
open. What is public: who wrote to whom, when, in which box, and how large
the envelope is.

What it costs: a sender pays about 53 satoshis an envelope at the default
fee (its share of a funding tree); a recipient pays the same for a receipt;
reading the box is free on every conforming host. A host may charge for
history (section 9).

## 2. Install

- **Container image**: `ghcr.io/lightwebinc/bbox` (the command),
  `ghcr.io/lightwebinc/bbox-host` (a host), and
  `ghcr.io/lightwebinc/bbox-devchain` (a local chain for trying things).
  Mount a named volume at `/home/nonroot/.bbox` and pass settings as
  `BBOX_<KEY>` variables.
- **From source**: `make bbox` builds `bin/bbox` (Go 1.27.1 or later). The
  only direct dependencies are go-sdk and bcommon, both pinned.

`bbox -version` prints the version. `bbox doctor` reads your settings and
home and asks every configured endpoint whether it answers; it changes
nothing, and is the first thing to run when something is wrong.

## 3. Concepts

| Word | What it is |
| --- | --- |
| **Identity** | A key in a home (`~/.bbox`). It is your address: senders seal to it, hosts index your box by it |
| **Office** | `<name>_<suffix>`, for example `support_qzxkvbmwtr`: a readable name and ten random letters drawn once, so nobody else's office shares its topic `tm_bbox_support_qzxkvbmwtr`. Hosts carry offices; a recipient reads in one |
| **Box** | A label inside your office (`inbox` by default, or `invoices`, `alerts`...). A sender picks it; you list one box or all of them |
| **Envelope** | One message: a BRC-169 envelope signed by the sender's derived key, with the body, any payment and any references encrypted to you (BRC-78) |
| **Carrier** | The unmined transaction an envelope rides in. It spends one output of the sender's mined **funding tree**, so it verifies by SPV without paying for block space. One funding output carries one answered envelope, ever |
| **Receipt** | Your signed acknowledgement of up to 64 envelopes, itself a carrier on your own funding tree. Every host that applies it stops answering those envelopes as open |
| **Sweep** | A mined transaction that spends funding outputs. Every carrier on a swept output is retracted: honest hosts stop answering it. It is how a sender takes an envelope back |
| **Host** | An overlay host carrying the office's topic and the `ls_bbox` lookup service. A replica, not an authority |
| **Plane or unicast** | How a publisher reaches the hosts: once to a facade on the multicast plane, which delivers to every subscribed host and repairs loss; or to each host itself (section 11) |
| **Terms route** | A host's own HTTP listener for priced questions: BRC-104 authentication, a 402 with the price, a BRC-29 payment (section 9) |
| **Payee** | The identity a host is paid to. Payments are recorded by the host and settled by the payee (section 12) |

## 4. Configuration

Settings come from global flags, then `BBOX_<KEY>` variables, then the
config file (`$BBOX_HOME/config`, else `~/.bbox/config`), then defaults.
The file is one `key = value` per line; an unknown key is an error, never
ignored. The addresses have no default: a default would send your
envelopes, or your questions, to a server nobody configured.

```text
# ~/.bbox/config
network      = test
office       = support_qzxkvbmwtr
asset        = http://192.0.2.10:8090
settle       = arcade:https://arcade.example.com
facade       = https://host-a.example.com
hosts        = https://host-a.example.com,https://host-b.example.com
header_url   = https://headers.example.com
history_host = https://terms.host-a.example.com
```

`asset` is a node's asset API (proofs, the tip, raw transactions);
`settle` is where mined transactions go (`arcade:`, `rpc:` or `tcp:`);
`header_url` is the header source every proof is checked against, and the
one thing you should choose yourself. The full table is in
[usage.md](usage.md).

## 5. Getting started

```console
$ bbox init
created /home/user/.bbox
identity     02c6...9a1e
fund address mv4r...Q8x (test)
$ bbox fund -txid 5e1f...77ab          # a payment you made to the fund address
$ bbox office new support              # once, as a recipient
office support_qzxkvbmwtr
topic  tm_bbox_support_qzxkvbmwtr
host   BBOX_OFFICES=support_qzxkvbmwtr
```

The identity is what you give senders. The office, and the hosts that carry
it, you tell them out of band (or in a published profile). A host operator
adds the `host` line to the host's configuration.

## 6. Sending

```console
$ bbox send 03a1...77c2 -m 'the invoice is attached' \
    -ref https://files.example.com/inv-7.pdf,4f2a...9c1d,48211
sent    8b0e...12fa
office  support_qzxkvbmwtr
to      03a1...77c2
box     inbox
```

The message is at most 16 KiB; larger content goes by reference, with its
SHA-256 and length (and its key, when the file is encrypted), inside the
encrypted message. On the plane keep it under 6 KiB: a larger envelope is
more packets, which are lost more often over distance (`warning:` says so).

`send` checks its own message against the recipient's rules and its own
carrier against the host's before anything leaves the machine, so what a
host or the recipient would refuse is never sent. The carrier is written to
your home before it is published: if the command stops part way, `doctor`
names it and the next `send`, `ack` or `drop` publishes the same bytes
first. A second carrier is never made on the same funding output.

## 7. Reading and acknowledging

```console
$ bbox list
2026-01-05T10:02:11Z  8b0e...12fa  from 02c6...9a1e  box inbox  1874B  [https://host-a.example.com, https://host-b.example.com]
1 envelope(s) in the inbox of support_qzxkvbmwtr, 2 of 2 host(s) answering
$ bbox read 8b0e...12fa
$ bbox ack 8b0e...12fa
acknowledged 1 envelope(s) in support_qzxkvbmwtr with receipt 1c7d...0b3e
```

Every host in `hosts` is asked; every envelope is checked (the host's own
admission rules, the signatures, SPV of its funding tree against your
`header_url`, the office, that it is to you) before it is shown. A host
that answers something that does not verify is named and nothing it
answered is shown (exit 1). Hosts that answer different sets are reported
host by host (exit 3); the union is shown, never merged silently.

**A host that lacks an envelope** another host has (it was off the plane,
or a unicast sender did not reach it) can be filled by anyone who holds
the envelope, since it verifies on its own:

```console
$ bbox list -fill
host https://host-b.example.com: DISAGREES: it does not answer 1 envelope(s) another host answered: 8b0e...12fa
host https://host-b.example.com: filled 1 of 1 envelope(s) it lacked
```

`read` decrypts, applies your checks in the spec's order, and filters
everything the sender wrote before it reaches your terminal: control
characters, escape sequences, bidirectional and zero-width characters are
removed. A message your key cannot open is shown as `UNDECRYPTABLE`, never
as empty.

`ack` writes one receipt for up to 64 envelopes you read. Hosts that apply
it stop answering those envelopes to the free questions, everywhere at
once; they keep them, for as long as their policy says, for `history`.

## 8. Payments inside an envelope

```console
$ bbox send 03a1...77c2 -m 'a coffee' -pay 5000
$ bbox read 3f9a...c410               # as the recipient
payment  5000 sat in 77d2...e1a0: acceptable until 2026-01-06T09:02:11Z; take it with bbox internalize 3f9a...c410
$ bbox internalize 3f9a...c410
internalized 5000 sat from 02c6...9a1e: payment 77d2...e1a0, pool 3 output(s), 105000 sat
acknowledged 3f9a...c410 with receipt 5a0c...9d12
```

The payment is a BRC-29 transaction to a key derived for you, sealed inside
the encrypted message. The sender never broadcasts it; nobody but you can
see it. `internalize` checks it, broadcasts it, waits for it to mine, puts
it in your pool and acknowledges the envelope. Take it promptly: until it
mines, the sender can still spend the coin elsewhere, and is entitled to
an hour after `expires` (one day by default). A payment the sender
double-spent is reported as reclaimed or double-spent (exit 1).

## 9. Priced questions

The inbox, a box, a sender's envelopes, receipts and sweeps are free on
every conforming host, always. A host may price `history`: what it still
keeps that is no longer open (acknowledged, expired).

```console
$ bbox terms
service ls_bbox, terms 1
history        5 sat a question
history-after  5 sat a question
$ bbox history
paid 5 sat to 03d4f2a9c1b7 in 0e8f...4c21, recorded by the host for its payee to settle
```

`history` speaks BRC-104 to the host's terms route (`history_host`), is
answered 402 with the price, pays it from your pool (BRC-29, unbroadcast,
in the `x-bsv-payment` header) and asks again. It never pays more than
`-max-sats` (1000 by default) for one question, and never pays for a free
class whatever a host says.

## 10. Taking an envelope back

```console
$ bbox drop 8b0e...12fa
retracted 1 funding output(s) of 6a41...e3b0: sweep 9e2c...5f18 mined at height 1710, published to support_qzxkvbmwtr
retraction reaches honest hosts only: it is not erasure, and a copy anyone kept stays a copy
```

`drop` mines a sweep of the envelope's funding output and publishes it to
every host. After that no honest host answers the envelope, read or not,
and the output can never carry another one (a host answers one carrier per
funding output, for its life). It is not erasure: whoever already read or
copied it keeps that copy.

A drop waits for a block. If the sweep could not be settled (the leg was
unreachable, busy, or gave no verdict) it stays in flight, `doctor` says
so, and the next `drop` finishes it first. If the network refuses it for
good (one of its inputs was spent elsewhere, say) it is marked `FAILED`,
its fee coin goes back to your pool when the node shows it unspent, and the
next `drop` builds a new sweep: a refused sweep never blocks you.

## 11. Plane or unicast

| | plane | unicast |
| --- | --- | --- |
| You submit each object | once, to `facade` | to every host in `hosts` |
| Other hosts receive it | from the plane, which repairs loss | only from you, or from whoever copies it across (`list -fill`) |
| A host that was down | receives what it missed from the plane | misses it until someone submits it again |
| Sweeps | to the facade and directly to every other host named | to every host named; all must take it |
| Settings | `facade`, and `hosts` for reading | `mode = unicast`, `hosts`, `quorum` |

On the plane a host off it learns of a sweep only because the client also
sends it directly to every host named. In unicast an object counts once
`quorum` hosts take it; a host that answers it admitted nothing is checked
by a lookup, since a duplicate and a refusal look the same.

## 12. Being paid: the payee

A host that prices a question records each payment in its
`payments.jsonl` and does not broadcast it. **Until it is settled, the
payer can spend the same coins elsewhere.** The payee settles on a
schedule:

```console
$ bbox -home /srv/payee payee settle /var/lib/bbox/payments.jsonl
settled 0e8f...4c21: 5 sat for history from 03a1b2c3d4e5
1 payment(s) settled, 5 sat; 0 settled before; 0 not settled; 0 refused (0 before); pool 1 output(s), 5 sat
```

One run takes every host's ledger, broadcasts every new payment, then
waits for them together (about one block, however many). A payment its
payer already double-spent is reported once as `REFUSED, NEVER SETTLES`,
recorded, and passed over afterwards: the question was answered and the
payee has nothing for it. That is the price of the window between
answering and settling, and why the window should be short.
[host.md](host.md) covers the host side: the key, the ledger, backups.

## 13. Limits

The defaults keep a client inside what hosts and the plane carry well;
the caps are where a setting stops making sense. The ones you meet first:

| Setting | Default | Cap |
| --- | --- | --- |
| Envelope content | small | 16 KiB; plane warns above 6 KiB |
| References per message | none | 32 |
| Funding tree outputs | 32 | 1000; plane warns above 100 |
| Send rate | 1 a second | 20 a second |
| Hosts | none | 16; unicast warns above 5 |
| Price paid for one question | at most 1000 sat | `-max-sats` |
| Payments settled at once | 16 | 64 |

The full table, with the reasons and the measurements behind them, is
[limits.md](limits.md).

## 14. Troubleshooting

Run `bbox doctor` first: it shows the home, the pool, the tree, anything
persisted and not yet published, a sweep in flight or failed, and whether
every endpoint answers.

| Exit | Meaning | What to do |
| --- | --- | --- |
| 0 | done; everything asked verified and the hosts agree | nothing |
| 1 | refused: a host answered something that does not verify (not shown), a message or payment your checks refuse, or the network refused a sweep or a payment | read the message: it names the host or the reason. A refused sweep is marked failed; drop again |
| 2 | usage, configuration, transport, a quorum not met, or a price over `-max-sats` | fix the named setting; what was persisted is published by the next command |
| 3 | incomplete: hosts disagree, a host could not be asked, or no host answers what you named | `list -fill` copies across what one host lacks; a host that is down answers later |

Common messages:

- `fee input: ... no spendable output`: fund the home (`bbox fund`). If it
  says coins are change not yet mined, wait for a block.
- `a sweep is still in flight`: the previous drop did not finish; `bbox
  drop` again finishes it.
- `host ...: DISAGREES`: one host lacks an envelope another has; `bbox list
  -fill`.
- `NOT SETTLED` from `payee settle`: the payment did not mine in time; the
  next run tries again.

## 15. Where to go next

- [usage.md](usage.md): every command and flag.
- [host.md](host.md): run a host, price a question, settle, back up.
- [limits.md](limits.md): every default and cap, and why.
- [spec.md](spec.md) and [frozen.md](frozen.md): the protocol, and what
  cannot change once published.
