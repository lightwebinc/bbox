# The bbox command

`bbox` sends and reads bbox envelopes ([spec.md](spec.md)). A sender seals
a message to a recipient's identity key, in a box of an office the
recipient reads, and publishes it: once on the plane, or to every host. A
recipient lists and reads its box from one or several hosts, checking every
answer itself against block headers it chooses, acknowledges what it read
with a signed receipt, and takes a payment that rode inside an envelope into
its wallet. A reader pays a host's 402 for the priced history question, and
a host's payee settles the payments the host recorded.

```text
  sender home                                         overlay hosts (tm_bbox_<office>, ls_bbox)
  ───────────                                         ─────────────────────────────────────────
  init ─▶ fund ─▶ send ──carrier(E)──────────────────▶ facade (mode plane) or each host (unicast)
                  │  └─funding tree, mined first──▶ settlement leg ─▶ chain
                  └─ drop ─▶ sweep, mined ─▶ facade and every host named
  recipient home
  ──────────────
  list, read ◀── ls_bbox answers of every host;  compared, and checked against header_url
  ack ──carrier(R)──▶ facade or each host
  internalize ─▶ the payment inside: checked, broadcast by the settlement leg, pooled; then ack
  history ──BRC-104, 402, BRC-29 payment──▶ the host's terms route (history_host)
  host's payee
  ────────────
  payee settle payments.jsonl ─▶ each payment: checked, broadcast, pooled
```

## Build

Go 1.27.1 or later, or Docker.

```console
$ make bbox              # bin/bbox
$ go build ./cmd/bbox    # or directly
$ make docker-build      # ghcr.io/lightwebinc/bbox and bbox-devchain, locally
```

In the image the home is `/home/nonroot/.bbox`; mount a named volume there
(`-v bbox-home:/home/nonroot/.bbox`) and pass settings as `BBOX_<KEY>`
variables.

## Commands

| Command | Needs a home | Needs | What it does |
| --- | --- | --- | --- |
| `init` | creates it | nothing | identity key and empty coin pool; prints the identity (the address senders seal to) and the fund address |
| `fund -txid TXID` | yes | `asset`, `header_url` | imports a mined payment to the fund address, once its proof checks |
| `fund [-blocks N]` | yes | `rpc`, `asset` | mines coinbase to the fund address through `generatetoaddress`: a chain you run. Refused on network `main` |
| `office new <name>` | optional | nothing | draws the 10-letter random suffix; prints the office, its topic, and the host's `BBOX_OFFICES` line |
| `office list` | yes | nothing | the offices this home created |
| `send <recipient> [box]` | yes | `office`, `settle`, `asset`; `facade` (plane) or `hosts` and `header_url` (unicast) | seals a message, with an optional payment and references, and publishes it |
| `drop <txid>...` | yes | as `send` | retracts sent envelopes: sweeps their funding outputs, and publishes the sweep once it mines. Work an earlier command left unfinished does not stop it |
| `list` | optional | `office`, `hosts`, `header_url` | the open envelopes in the inbox, one box or from one sender, from every host, compared; `-fill` copies an envelope a host lacks across to it |
| `read [txid...]` | yes | as `list` | verifies, decrypts and prints envelopes, and records them for `ack` and `internalize` |
| `ack <txid>...` | yes | as `send` | acknowledges envelopes read, one receipt per 64 |
| `internalize <txid>` | yes | as `list`, and `asset`, `settle` | takes the payment inside an envelope into the wallet, then acknowledges it |
| `history` | yes | `office`, `history_host`, `header_url`, `asset`, `settle` | the priced question: pays the host's 402 from the pool, prints what the host keeps that is no longer open; one page, or every page with `-all`. One payment a question, at most `-max-sats`, and at most `-budget` in all |
| `terms [URL]` | no | `history_host` or URL | prints a host's terms document |
| `payee key -out FILE` | yes | nothing | writes `BBOX_PAYEE_KEY` for this home's identity to a new file, mode 0600 |
| `payee settle <payments.jsonl>...` | yes, the payee's | `header_url`, `asset`, `settle` | internalizes every payment in a host's ledger this home has not: all broadcast, then awaited together (`-in-flight`) |
| `doctor` | optional | nothing | the home's state and whether the node, headers, hosts and priced host answer; reads only |
| `version` | no | nothing | prints the version |

`<recipient>`, `-from` and `-to` are identity keys, 66 lowercase hex
characters. An office is always the full identifier `<name>_<suffix>`; the
readable name alone is refused. A txid is 64 lowercase hex characters, in
display order. `bbox <command> -h` prints a command's flags.

## Configuration

Settings come from, highest first: the global flags, `BBOX_<KEY>` in the
environment (the key in upper case), the config file, and the defaults. The
config file is `-config PATH`, else `$BBOX_HOME/config`, else
`$XDG_CONFIG_HOME/bbox/config`, else `~/.bbox/config`. Its grammar is one
`key = value` per line, `#` comments, blank lines ignored; a key it does not
define, a key given twice, or a line without `=` is an error, never
ignored.

| Key | Default | Meaning |
| --- | --- | --- |
| `arcade_key` | none | bearer token for an arcade installation named in `settle` |
| `asset` | none | the node's asset API base URL: proofs, the chain tip, blocks, raw transactions |
| `box` | none | the box `send` addresses and `list`, `read` ask about when none is named. None: `send` uses `inbox`, and `list` and `read` ask about every box |
| `facade` | none | mode `plane`: the overlay host a publisher submits to (its `/submit` route), from which the plane delivers to every subscribed host. Refused in mode `unicast` |
| `header_url` | none | the header source every proof is checked against: an overlay bridge's base URL (native `/v1/root/<height>` and `/v1/tip`), `woc:main`, `woc:test` or `chaintracks:URL` |
| `history_host` | none | the base URL of `ls_bbox` on the host `history` asks and `terms` reads: the host's terms route (spec section 7.4). An origin, with no path: BRC-104 authenticates at the origin's `/.well-known/auth`, and the route answers `/lookup` and `/ls_bbox/terms` at its root |
| `home` | `~/.bbox` | the identity's home |
| `hosts` | none | the overlay hosts, comma separated, at most 16: every host a reader asks and compares; in mode `unicast` every host a publisher submits to; in mode `plane` the hosts other than `facade` a sweep is also sent to directly |
| `mode` | `plane` | `plane` or `unicast`: how a publisher's objects reach the hosts (spec section 9) |
| `network` | `main` | `main`, `test` or `regtest`: the fund address prefix and the header source's proof-of-work floor |
| `object_bound` | `1048576` | the plane's object bound in bytes, at most 8388608; a carrier whose BEEF is larger is not published |
| `office` | none | the office identifier a command uses when it names none |
| `originator` | `bbox` | the BRC-100 originator presented to the wallet |
| `quorum` | `all` | how many of `hosts` must hold a carrier (an envelope or a receipt) for it to count as published: `all`, `majority`, `one`, or a number up to the host count. In unicast a host that took it counts; on the plane a host that answers it by lookup does. A sweep always needs every host |
| `rpc` | none | the node's JSON-RPC URL (`fund`) |
| `rpc_pass`, `rpc_user` | `bitcoin` | the node's basic auth, also for `settle = rpc:` |
| `settle` | none | the settlement leg for what is mined (funding trees, sweeps, a payment a recipient or a payee takes): `tcp:<host:port>` (bare EF to an ingress), `rpc:<url>` (a node's `sendrawtransaction`) or `arcade:<url>` |
| `timeout` | `15s` | bounds each request; `1s` to `5m` |
| `tree_count` | `32` | outputs of a new funding tree, 1 to 1000 |

Global flags, before the command: `-config PATH`, `-home DIR`,
`-hosts URLS`, `-header-url SOURCE`, `-mode MODE`, `-network NET`,
`-office OFFICE`, `-quorum Q`, `-timeout DUR`, `-v` (also accepted after
the command), `-version`.

Every limit on these settings and on the flags below, with its reason, is
in [limits.md](limits.md). A cap is refused with exit 2 and a message
naming the limit; a recommendation passed prints `warning: ...` and goes
on. The addresses have no default on purpose: a default would send an
envelope, or a question, to a server nobody configured. A command that
needs one and finds none exits 2 naming the key.

A sender's and reader's config file, with documentation addresses:

```text
# ~/.bbox/config
network      = test
office       = support_qzxkvbmwtr
asset        = http://192.0.2.10:8090
settle       = rpc:http://192.0.2.10:9292
facade       = https://host-a.example.com
hosts        = https://host-a.example.com,https://host-b.example.com
header_url   = https://headers.example.com
history_host = https://terms.host-a.example.com
```

The same without the plane, submitting to both hosts itself:

```text
mode         = unicast
hosts        = https://host-a.example.com,https://host-b.example.com
quorum       = all
```

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | done, or everything asked verified and the hosts agree |
| 1 | refused: a host answered something that does not verify (it is not shown), a message or a payment the recipient's checks refuse, a sweep or a payment the network refuses, or a host that asks for a second payment for one question |
| 2 | usage, configuration, local or transport error, a quorum not met, a price over `-max-sats` or `-budget`, or a sweep a leg refused that may still mine (it stays in flight); what was persisted is sent or published by the next command |
| 3 | incomplete: the hosts disagree, a host could not be asked, no host answers what was named, a budget ran out before the history was read to its end, or an output to sweep is spent by a transaction not yet mined. It proves nothing either way |

## An identity and its home

```console
$ bbox init
created /home/user/.bbox
identity     02c6...9a1e
fund address mv4r...Q8x (test)
$ bbox fund -txid 5e1f...77ab
imported 1 of 1 output(s) paying mv4r...Q8x, 100000 sat, mined at height 1702; pool 1 output(s), 100000 sat
```

The home (mode 0700) holds the identity key (`identity.json`), the coin
pool (`wallet.json`, basket `bbox fund`), the state (`state.json`) and a
lock: one command that writes it at a time. The lock is taken before the
wallet is opened: the coin pool is read whole and written back whole, so a
pool read before the lock could put back coins another command spent. The identity key is the
recipient's address: a sender seals to it, and a host indexes by it. The
fund address is the home's funding key (`[1, "bbox message"]`, key id
`fund`) for the configured network; `fund -blocks` mines to it on a chain
you run, and refuses a mainnet address on a test network and every
network `main`.

The state is written before anything leaves the machine. A carrier is
persisted, with the funding output it spends marked used, before it is
submitted: a command that stops part way leaves it in the outbox, `doctor`
names it, and the next `send`, `ack` or `drop` publishes the same bytes
first. A second carrier is never made on a funding output
(a host answers one carrier per funding output for its life). Every piece
of unfinished work is tried, whatever became of the one before it: a sweep
that cannot be sent does not keep an envelope from the hosts. The command
then stops on the first failure (exit 2), except `drop`, which goes on.

A coin leaves `wallet.json` the moment it is taken, before the transaction
that spends it is recorded in `state.json`. While a command that spends is
running, the state records the pool as it stood (`taking`); a command that
ends clears it. The next command after one that stopped in between asks
the node about a coin in that record that neither file accounts for: one
the node shows unspent goes back in the pool, one it shows spent is
reported, and one it cannot answer for is looked at again next time. A
coin taken while a funding tree was being minted is the exception: see
[limits.md](limits.md).

The proof a home keeps of a funding tree is the one the node gave when it
mined. When `header_url` is set, the kept proof is checked against it
before an output of the tree is spent or swept; one that no longer
verifies (a reorganisation mined the tree again elsewhere) is replaced by
the tree's current proof from `asset`, in the home too.

## Offices

```console
$ bbox office new support
office support_qzxkvbmwtr
topic  tm_bbox_support_qzxkvbmwtr
host   BBOX_OFFICES=support_qzxkvbmwtr
```

`<name>` is 1 to 31 lowercase letters and single underscores; the suffix is
ten letters drawn uniformly at random, once. It keeps unrelated offices off
each other's topic; it is not a secret. The `host` line is what a host that
carries the office sets (docs/host.md). A recipient tells its senders the
office, and in mode unicast its hosts, out of band.

## Sending

```console
$ bbox send 03a1...77c2 -m 'the invoice is attached' \
    -ref https://files.example.com/inv-7.pdf,4f2a...9c1d,48211
sent    8b0e...12fa
office  support_qzxkvbmwtr
to      03a1...77c2
box     inbox
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-m TEXT`, `-file PATH` | standard input | the message body |
| `[box]` (positional) | `box`, else `inbox` | the recipient's box: 1 to 50 lowercase letters, digits and single underscores, starting with a letter |
| `-office OFFICE` | `office` | the office |
| `-pay SATS` | none | a BRC-29 payment of SATS to the recipient, inside the encrypted message |
| `-ref URL,SHA256,LENGTH[,KEY]` | none | a reference to content too large for an envelope, at most 32: an `https://` or `uhrp://` locator, the SHA-256 of the bytes as fetched, their length, and when they are encrypted their AES-256 key (64 hex) |
| `-expires DUR` | none; `24h` with `-pay` | the envelope's life; at least `2h` with a payment, at most `720h` |
| `-rate R` | `1` | envelopes a second at most, at most `20` |
| `-tree-count N` | `tree_count` | outputs of a new funding tree |

The body, the payment and the references are encrypted to the recipient
(BRC-78) inside a BRC-169 envelope signed by the sender's derived key. Who
wrote to whom, when, in which box and office, and the envelope's size, are
public to every host on the office. `send` applies the recipient's rules
to its own message before it seals it, and the host's rules to its own
carrier before it persists it: what a host or the recipient would refuse is
not sent.

A carrier spends one output of the home's funding tree: a mined
transaction of `tree_count` outputs, minted from the pool the first time,
and minted ahead, before the current tree runs out, so a send waits for a
block only for the first tree. A funding tree is not published to an
office: every carrier carries its own, with its proof.

**A payment inside an envelope** is built from the pool and never
broadcast by the sender: it is a transaction of one output to the key
BRC-29 derives for the recipient, with the sender's change, carried as
Atomic BEEF in the encrypted message. Its fee coin is spent, and its change
is held in the pool until the payment mines. The recipient internalizes it,
which broadcasts it, until an hour before the envelope expires. A sender
may spend the coin elsewhere only once `now` is more than an hour past
`expires` and the payment has not mined (spec section 10); this version of
the command does not reclaim.

On the plane an object is submitted once to the facade. The facade's
answer is not a host's: it says "nothing admitted" for a duplicate and for
a refusal alike, and the plane can lose an object on the way to any host.
So when `hosts` and `header_url` are set, each host named is then asked
for the object by lookup (for up to 3.5 s), and a host that lacks it is
offered it directly. An envelope or a receipt counts as published once
`quorum` hosts answer it, a sweep once every host does; until then it
stays in the outbox (exit 2) and the next command publishes it. A sender
that names no host has nothing to ask, says so once, and takes the
facade's word.

In mode `unicast` every object goes to each host, retried on its own, and
is published once `quorum` hosts take it. A host that answers that it
admitted nothing (a duplicate, or a refusal: they look the same) counts
only once a lookup at that host answers the object; for an envelope, a
receipt the host answers that names it counts too, since an acknowledged
envelope is no longer in any box. After each command a
line per host says what it took and missed:

```text
host https://host-a.example.com: took 1 object(s), missed 0
host https://host-b.example.com: took 0 object(s), missed 1: envelope 8b0e12fa0c1d
```

## Reading

```console
$ bbox list
2026-01-05T10:02:11Z  8b0e...12fa  from 02c6...9a1e  box inbox  1874B  [https://host-a.example.com, https://host-b.example.com]
1 envelope(s) in the inbox of support_qzxkvbmwtr, 2 of 2 host(s) answering
$ bbox read 8b0e...12fa
envelope 8b0e...12fa
office   support_qzxkvbmwtr
from     02c6...9a1e
box      inbox
created  2026-01-05T10:02:11Z
hosts    https://host-a.example.com, https://host-b.example.com
ref      https://files.example.com/inv-7.pdf 48211 bytes sha256 4f2a...9c1d

the invoice is attached
```

`list`, `read` and `internalize` take `-box BOX` or `-from KEY` (one
question names a box or a sender, never both) and `-office OFFICE`; `list`
also takes `-to KEY`, to list another key's open envelopes, which are
public. Every host in `hosts` is asked the same question, page after page
(64 envelopes a page, at most 64 pages). Every answered carrier is checked
before it is shown: the host's own admission rules, including the BRC-169
envelope's signature and the encrypted payload's header; SPV through its
funding tree against `header_url`; the office; and the recipient. A proof
of the funding tree that a host stored and that no longer verifies (a
reorganisation mined the tree again elsewhere) is replaced by the tree's
current proof from `asset`, when one is configured, and verified against
`header_url` like any other; without `asset` such an answer is refused.
What one page and one walk hold is bounded whatever a host answers: at
most 128 outputs are taken from a page, an answer is at most 16 MiB, and a
host whose full page does not move past its cursor is asked no further. A
host that answers something that does not verify is named, what it answered is
not shown, and the exit is 1. Hosts that answer different sets are
reported, host by host, with the envelopes each lacks, and the exit is 3:
the union is shown, never merged silently. An empty answer proves nothing.

```console
$ bbox list -fill
2026-01-05T10:02:11Z  8b0e...12fa  from 02c6...9a1e  box inbox  1874B  [https://host-a.example.com]
host https://host-b.example.com: DISAGREES: it does not answer 1 envelope(s) another host answered: 8b0e...12fa
1 envelope(s) in the inbox of support_qzxkvbmwtr, 2 of 2 host(s) answering
host https://host-b.example.com: filled 1 of 1 envelope(s) it lacked
```

`list -fill` repairs that: each envelope a host lacks is copied across to
it (spec section 9: a carrier verifies on its own, so anyone holding one
may submit it), the carrier as it verified here, submitted to that host
alone and counted once a lookup there answers it. The exit is 0 once every
copy is confirmed. A host that dropped the carrier (a superseded or
retracted one) does not take it back and stays reported, exit 3. On the
plane a host that was down receives what it missed from the plane's
repair; `-fill` is for a host off the plane, or one a unicast publisher
did not reach.

`read` then decrypts and applies the recipient's checks in the order of
spec section 4.7: a message the key cannot open is shown as
`UNDECRYPTABLE`, never as empty; a plaintext that breaks the rules is
refused; a payment is checked (its shape, `expires` at least two hours
after `created`, more than an hour before `expires` now, its Atomic BEEF,
each output paying the key the wallet derives, full SPV against the
headers) and shown as acceptable or refused with its label, and is taken
only by `internalize`. Everything someone else wrote (the body, the
references, the box, the office) is filtered before it reaches the terminal
(bcommon `termsafe`): control characters, escape sequences, and
bidirectional and zero-width characters are removed. What is read is
recorded in the home.

```console
$ bbox ack 8b0e...12fa
acknowledged 1 envelope(s) in support_qzxkvbmwtr with receipt 1c7d...0b3e
$ bbox ack -all
```

`ack` acknowledges envelopes this home has read, one receipt per 64: a
receipt is a carrier on one of the home's funding outputs, so a recipient
that acknowledges needs a funded home. Every host that applies it stops
answering those envelopes to the free questions; it keeps them, for as long
as its policy says, for the priced history.

```console
$ bbox internalize 3f9a...c410
internalized 5000 sat from 02c6...9a1e: payment 77d2...e1a0, pool 3 output(s), 105000 sat
acknowledged 3f9a...c410 with receipt 5a0c...9d12
```

`internalize` fetches the envelope again, checks it and its payment as
`read` does at this moment, and hands the payment to the wallet's
`internalizeAction` (BRC-100, protocol `wallet payment`, each listed
output with its remittance and the sender as its sender): the wallet checks
each output pays the key it derives, verifies the payment against the
headers, broadcasts it through `settle`, waits for its proof, and adds the
outputs to the pool with their derivation, so the pool can spend them. The
envelope is then acknowledged, unless `-no-ack`. A payment the network
refuses because the sender spent its inputs is reported as reclaimed or
double-spent, exit 1. Running it again after a timeout is safe. An
envelope whose payment passed `read`'s checks is kept in the home until its
payment is taken, so `internalize` works after the envelope is
acknowledged too, when no host answers it any more: the kept carrier is
checked again against the headers, as a host's answer is. `ack -all`
leaves such an envelope out, and names it.

## Retracting

```console
$ bbox drop 8b0e...12fa
retracted 1 funding output(s) of 6a41...e3b0: sweep 9e2c...5f18 mined at height 1710, published to support_qzxkvbmwtr
retraction reaches honest hosts only: it is not erasure, and a copy anyone kept stays a copy
```

`drop` sweeps the funding outputs of the named envelopes, one mined
transaction per funding tree (bcommon `carrier.Sweep`: output 0 a
funding-shaped tombstone, so hosts admit it), waits for it to mine, and
publishes it to each office it retracts in: on the plane to the facade and
directly to every other host in `hosts`, in mode unicast to every host. A
host that holds the sweep answers none of those envelopes again, read or
not. `drop -tree TXID -yes` sweeps every output of a funding tree, used or
not, which retracts every carrier on it, receipts included.

A sweep is persisted before it is settled. One the settlement leg did not
take (unreachable, busy, no verdict yet, or refused while every input is
still its own to spend) stays in flight, `doctor` says so, and the next
`drop` sends it again before anything else; it never sweeps an output
twice.

**A sweep is given up only on the node's word that another transaction
spends one of its inputs.** A leg's refusal alone decides nothing: arcade's
`REJECTED` or `DOUBLE_SPEND_ATTEMPTED`, and a node's RPC error -25 or -26,
are also how a second submission of a transaction the network already took
is answered, and a sweep marked failed on that would then mine and retract
what the home believes it did not. While the node shows every input
unspent, spent by this very sweep, or cannot say, the sweep stays in
flight. Once it names another spender the sweep is marked failed, and
`drop` builds another without that input, three tries in all; if none can
be built it exits 1 with the reason:

```text
bbox: sweep 9e2c...5f18 refused by the network: input 1 (41d0...77aa.1) is spent by 7c3e...90ab (arcade reports DOUBLE_SPEND_ATTEMPTED); it is marked failed and retracts nothing; its fee coin 41d0...77aa.1 is spent by 7c3e...90ab and was not given back; drop again to build a new sweep
```

The fee coin goes back to the pool only when the node shows it unspent.
The failed sweep retracts nothing, no longer blocks a drop, and `doctor`
lists it as `FAILED`.

Before a sweep is built the node is asked what spends each output it would
name. An output another transaction already spends is left out, and once
that transaction is mined it is recorded as the sweep it is and published
as one: another copy of this home swept it, or an earlier run gave up on a
sweep that mined all the same. An output whose spender has not mined yet
is left for the next `drop` (exit 2).

Work an earlier command left unfinished (an envelope a host has not taken)
does not stop a `drop`: it is reported and left for the next command.

## The priced question

```console
$ bbox terms
service ls_bbox, terms 1
history        5 sat a question
history-after  5 sat a question
$ bbox history
paid 5 sat to 03d4f2a9c1b7 in 0e8f...4c21, recorded by the host for its payee to settle
2026-01-05T10:02:11Z  8b0e...12fa  from 02c6...9a1e  box inbox  1874B
1 envelope(s) the host keeps that are no longer open
```

`history` asks `history_host` (or `-at URL`), the host's terms route, over
BRC-104 authentication with the home's identity key. The host answers 402
with its price and a derivation prefix (BRC-105); the command pays: one
output to the key BRC-29 derives for the host's identity key, the prefix
and a suffix of its own, from the pool, unbroadcast, as Atomic BEEF in the
`x-bsv-payment` header as output 0, and asks again. The host verifies the payment,
records it and answers; the host does not broadcast it, and its payee
settles it (below). A price over `-max-sats`
(default 1000) is not paid (exit 2). One payment buys one answer page;
`-after <created>:<txid>` asks for the next, and `-all` for every page
from there on.

**One question is paid for once.** The SDK answers every 402 with a new
payment; the command does not. A host that answers 402 again after a
payment was sent is refused (exit 1), and handed nothing more. The payment
is recorded in the home before it is sent, and from then on it is made:
the host holds a valid transaction that spends the coin. A host that takes
it and does not answer (a 500, a dropped connection, a 429 on the paid
request) is reported; the payment is kept, broadcast from here so that its
change does not wait on the host, and its coin is never given back to the
pool, where the next payment would spend it a second time. Only a payment
that was never made (the price was over the bound) leaves its coin in the
pool.

**A budget for the whole command.** `-max-sats` bounds one question,
`-budget` (default 16000) everything the command pays. With `-all` each
page is one paid question; when the budget cannot cover the next page the
command stops and names the cursor to go on from (exit 3). A page that
holds an envelope at or before the cursor it was asked after is refused. The free classes are never asked
here: a client never pays for them, whatever a document or a 402 says.

## A host's payee

```console
$ bbox -home /srv/payee init
$ bbox -home /srv/payee payee key -out /etc/bbox/payee.env
wrote BBOX_PAYEE_KEY to /etc/bbox/payee.env for payee 03d4...a2b1
$ bbox -home /srv/payee payee settle /var/lib/bbox/payments.jsonl
settled 0e8f...4c21: 5 sat for history from 03a1b2c3d4e5
1 payment(s) settled, 5 sat; 0 settled before; 0 not settled; 0 refused (0 before); pool 1 output(s), 5 sat
```

A host records every payment its terms route accepts in `payments.jsonl`
and does not broadcast it. **Until a payment is settled, its payer can
spend the same coins elsewhere**, so the payee settles on a schedule.
`payee settle` internalizes each payment the home has not settled, as
`internalize` does, and records it. One the network refuses for good (its
payer spent the inputs elsewhere, which the node's view of the inputs
shows even when the settlement leg accepted it) is reported as `REFUSED,
NEVER SETTLES`, recorded, and passed over by later runs; the exit is 1 on
the run that finds it. One that merely did not mine in time is `NOT
SETTLED`, tried again next run, exit 1. It checks every payment first, broadcasts
them all, and then waits for their proofs together, so a run takes about
one block however many it settles; `-in-flight N` (default 16, at most 64)
bounds how many are broadcast and not yet mined at once. It takes several
ledgers at once (one payee for several hosts), settling a payment found in
two only once. docs/host.md says
how a host is configured with the key.

## Doctor

```console
$ bbox doctor
home        /home/user/.bbox
network     test
office      support_qzxkvbmwtr
node        http://192.0.2.10:8090 tip 1712
identity    02c6...9a1e
pool        4 output(s), 96200 sat
tree        6a41...e3b0 27 of 32 left
sent        3 envelope(s), 1 receipt(s), 1 sweep(s)
headers     https://headers.example.com tip 1712
mode        plane
facade      https://host-a.example.com
settle      rpc:http://192.0.2.10:9292
host        https://host-a.example.com answering
host        https://host-b.example.com answering
history     https://terms.host-a.example.com serves terms (2 priced class(es))
```

It reads only: it publishes nothing and spends nothing.
