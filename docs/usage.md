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

Go 1.27.2 or later, or Docker.

```console
$ make bbox              # bin/bbox
$ go build ./cmd/bbox    # or directly
$ make docker-build      # ghcr.io/lightwebinc/bbox and bbox-devchain (regtest, development), locally
$ go install github.com/lightwebinc/bbox/cmd/bbox@latest
```

In the image the home is `/home/nonroot/.bbox`; mount a named volume there
(`-v bbox-home:/home/nonroot/.bbox`) and pass settings as `BBOX_<KEY>`
variables.

## Commands

| Command | Needs a home | Needs | What it does |
| --- | --- | --- | --- |
| `init` | creates it | nothing | identity key and empty coin pool; prints the identity (the address senders seal to) and the fund address |
| `fund -txid TXID` | yes | `chain`, `header_url` (defaults on main and test) | imports a payment you sent from your own wallet to the fund address, once it is mined and its proof checks: how a home on mainnet or testnet is funded |
| `fund -beef FILE\|-` | yes | `header_url` | imports that payment as the BEEF your wallet hands over, with no lookup; one not mined yet is held until it mines, or refused with `-mined-only` |
| `fund [-blocks N] [-batch N] [-rescan]` | yes | `rpc`, `chain = asset:URL` | coinbase: only on a regtest chain you run (development and tests). Mines coinbase to the fund address through `generatetoaddress`, or with `-rescan` re-reads recent blocks for coinbase the pool lacks. Refused on network `main` |
| `office new <name>` | optional | nothing | draws the 10-letter random suffix; prints the office, its topic, and the host's `BBOX_OFFICES` line |
| `office list` | yes | nothing | the offices this home created |
| `send <recipient> [box]` | yes | `office`; `facade` (plane) or `hosts` (unicast); `settle`, `chain`, `header_url` (defaults on main and test) | seals a message, with an optional payment and references, and publishes it |
| `drop <txid>...` | yes | as `send` | retracts sent envelopes: sweeps their funding outputs, and publishes the sweep once it mines. Work an earlier command left unfinished does not stop it |
| `list` | optional | `office`, `hosts`, `header_url` | the open envelopes in the inbox, one box or from one sender, from every host, compared; `-fill` copies an envelope a host lacks across to it |
| `read [txid...]` | yes | as `list` | verifies, decrypts and prints envelopes, and records them for `ack` and `internalize` |
| `ack <txid>...` | yes | as `send` | acknowledges envelopes read, one receipt per 64 |
| `internalize <txid>` | yes | as `list`, and `chain`, `settle` | takes the payment inside an envelope into the wallet, then acknowledges it |
| `history` | yes | `office`, `history_host`, `header_url`, `chain`, `settle` | the priced question: pays the host's 402 from the pool, prints what the host keeps that is no longer open; one page, or every page with `-all`. One payment a question, at most `-max-sats`, and at most `-budget` in all |
| `terms [URL]` | no | `history_host` or URL | prints a host's terms document |
| `payee key -out FILE` | yes | nothing | writes `BBOX_PAYEE_KEY` for this home's identity to a new file, mode 0600 |
| `payee settle <payments.jsonl>...` | yes, the payee's | `header_url`, `chain`, `settle` | internalizes every payment in a host's ledger this home has not: all broadcast, then awaited together (`-in-flight`) |
| `doctor` | optional | nothing | the home's state, the chain services and fee rate in force, and whether the headers, settlement leg, hosts and priced host answer; reads only |
| `version` | no | nothing | prints the version |

`<recipient>`, `-from` and `-to` are identity keys, 66 lowercase hex
characters. An office is always the full identifier `<name>_<suffix>`; the
readable name alone is refused. A txid is 64 lowercase hex characters, in
display order. `bbox <command> -h` prints a command's flags.

## Configuration

Settings come from, highest first: the global flags, `BBOX_<KEY>` in the
environment, the config file (`~/.bbox/config` by default), and the
defaults. No node is needed: on `main` and `test` the header source and
the chain view default to WhatsOnChain (`woc:main`, `woc:test`) and the
settlement leg to GorillaPool's public arcade (`arcade:main`,
`arcade:test`); a node of your own (`chain = asset:URL`) is the better
option. The hosts and the facade have no default on purpose: a default
would send an envelope, or a question, to a server nobody configured, so a
command that needs one and finds none exits 2 naming the key. Every key, flag and variable, with its
default, and example files for mainnet, testnet and unicast, are in
[configuration.md](configuration.md). Every limit on these settings and on
the flags below, with its reason, is in [limits.md](limits.md).

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | done, or everything asked verified and the hosts agree |
| 1 | refused: a message or a payment the recipient's checks refuse, a paid `history` page that holds what does not verify (the one host asked, named), a sweep or a payment the network refuses, or a host that asks for a second payment for one question |
| 2 | usage, configuration, local or transport error, a quorum not met, a price over `-max-sats` or `-budget`, or a sweep a leg refused that may still mine (it stays in flight); what was persisted is sent or published by the next command |
| 3 | incomplete: the hosts disagree, a host answered something that does not verify (it is named and not shown), a host could not be asked, no host answers what was named, a budget ran out before the history was read to its end, or an output to sweep is spent by a transaction not yet mined. It proves nothing either way |

## An identity and its home

```console
$ bbox init
created /home/user/.bbox
identity     02c6...9a1e
fund address 1Kq3...Vb7 (main)
$ bbox fund -txid 5e1f...77ab        # after sending coin from your own wallet to 1Kq3...Vb7
imported 1 of 1 output(s) paying 1Kq3...Vb7, 10000 sat, mined at height 970041; pool 1 output(s), 10000 sat
```

Or hand over the payment as the BEEF your wallet gives you, with no lookup
and no wait for a block:

```console
$ bbox fund -beef payment.beef
imported 1 of 1 output(s) paying 1Kq3...Vb7, 10000 sat, not mined yet: held until it mines, when a later command collects its proof; pool 1 output(s), 10000 sat
```

An unmined payment is taken only when every transaction it spends carries
a proof your headers hold and its scripts verify against them; its coin is
spendable once it mines. `-mined-only` (`fund_mined_only = true`) refuses
it instead.

The home (mode 0700) holds the identity key (`identity.json`), the coin
pool (`wallet.json`, basket `bbox fund`), the state (`state.json`) and a
lock: one command that writes it at a time. The lock is taken before the
wallet is opened: the coin pool is read whole and written back whole, so a
pool read before the lock could put back coins another command spent. The identity key is the
recipient's address: a sender seals to it, and a host indexes by it. The
fund address is the home's funding key (`[1, "bbox message"]`, key id
`fund`) for the configured network: a mainnet address on `main`, a
testnet address on `test` and `regtest`. A home is funded by sending coin
from your own wallet to it and importing the payment with `fund -txid` or
`fund -beef`.
`fund -blocks` mines coinbase to it (coinbase: only on a regtest chain you
run, for development and tests), and refuses a mainnet address on a test
network and every network `main`.

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
the chain view about a coin in that record that neither file accounts for: one
the node shows unspent goes back in the pool, one it shows spent is
reported, and one it cannot answer for is looked at again next time.

A funding tree has a record of its own. Before a tree reaches the
settlement leg, the state records it with the coin it spends
(`pendingTrees`), and the save that records the tree as adopted drops that
record. A command that stopped in between, while the tree was on its way
to the chain or, minted ahead, before the command ended, leaves the
record, and `doctor` names it (`tree ... signed for coin ... and not
recorded as minted`). The next `send`, `ack` or `drop` asks the node what
became of the tree before it spends anything, and says what it found:

| The node shows | The command does | It says |
| --- | --- | --- |
| the tree, its coin not spent by another transaction, and the current tree is used up or there is none | adopts the tree and takes its unspent change into the pool | `funding tree ... is recovered` |
| the tree, its coin not spent by another transaction, and the current tree still has outputs | holds the tree, behind any tree already held, takes its change, and adopts it when the trees before it run out; the record stays until then | `funding tree ... is recovered and waits for the switch` |
| no such tree, and the coin unspent | puts the coin back in the pool and keeps the record for one more command; the second such answer drops it | `... never reached the chain: its fee coin ... is unspent and back in the pool`, then `its record is kept for one more command` |
| no such tree, and the coin spent by another transaction | drops the record | `... never reached the chain: its fee coin ... is spent by ...` |
| the tree, with no proof, and the coin spent by another transaction | drops the record and adopts nothing: the tree lost a double spend and never mines, though the node still serves it | `... lost a double spend: the node still serves it, without a proof, and its fee coin ... is spent by ...` |
| the tree, with no proof, and it cannot say who spent the coin | keeps the record and goes on; the next command asks again | `... could not be settled now (...): its record is kept and the next command asks again` |
| nothing: it cannot be asked, or cannot say | keeps the record and goes on; the next command asks again | `... could not be settled now (...): its record is kept and the next command asks again` |

A tree that reaches the node after its coin went back is found by the next
command, which adopts it and takes the coin, now spent, out of the pool
(`... its fee coin ... is spent and is taken out of the pool`). Several
trees found on the chain while the current tree has outputs are all held,
and are spent one after the other, in the order of their records. A command
that only reads (`list`, `history`, `doctor`, and `read` when it
acknowledges nothing) asks nothing about a tree and needs no node for it.
What is left open is in [limits.md](limits.md).

The home pays for its trees from its own pool. A deployment that funded
trees through a BRC-100 wallet instead (bcommon's `Trees.Fund`) would not
be covered by any of this: such a wallet chooses the coins, signs and
broadcasts inside one call, so there is no moment before the broadcast to
record the tree in, and a run that stopped after the broadcast and before
the tree was adopted would leave a tree only that wallet knows.

The proof a home keeps of a funding tree is the one it was given when the
tree mined. The kept proof is checked against `header_url` before an
output of the tree is spent or swept; one that no longer verifies (a
reorganization mined the tree again elsewhere) is replaced by the tree's
current proof from `chain`, in the home too.

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
reorganization mined the tree again elsewhere) is replaced by the tree's
current proof from `chain`, when one is configured (on main and test it
always is), and verified against `header_url` like any other; without one
such an answer is refused.
What one page and one walk hold is bounded whatever a host answers: at
most 128 outputs are taken from a page, an answer is at most 16 MiB, and a
host whose full page does not move past its cursor is asked no further. A
host that answers something that does not verify is named, what it answered is
not shown, and the exit is 3: what is shown comes from the hosts whose
answers verify, and one host at fault does not make a check fail. When
every envelope the hosts answered is refused, the command says so and names
the header source it was checked against, since a header source that does
not answer fails every answer the same way; an envelope named that every
host answered and none verified is not called absent. Hosts that answer different sets are
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
paid 5 sat to 03d4f2a9c1b7 in 0e8f...4c21, broadcast by the host for its payee to settle
2026-01-05T10:02:11Z  8b0e...12fa  from 02c6...9a1e  box inbox  1874B
1 envelope(s) the host keeps that are no longer open
```

`history` asks `history_host` (or `-at URL`), the host's terms route, over
BRC-104 authentication with the home's identity key. The host answers 402
with its price and a derivation prefix (BRC-105); the command pays: one
output to the key BRC-29 derives for the host's identity key, the prefix
and a suffix of its own, from the pool, unbroadcast, as Atomic BEEF in the
`x-bsv-payment` header as output 0, and asks again. The host verifies the payment,
records it, broadcasts it, and answers once the network took it (or, for a
payment over its threshold, once it mines: until then it answers 402
`ERR_PAYMENT_HELD`); its payee settles it (below). A price over `-max-sats`
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
holds an envelope at or before the cursor it was asked after is refused.
A page is one host's paid answer, with no other host beside it: an
envelope in it that does not verify is refused (exit 1). The free classes are never asked
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
and broadcasts it. **Until a payment mines, its payer can race a
conflicting spend**, so the payee settles on a schedule.
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
headers     woc:test tip 1761988
chain       woc:test
settle      arcade:test answering
fee         100/1000 satoshis/bytes, floor 100 sat (static)
identity    02c6...9a1e
pool        4 output(s), 96200 sat, 0 immature
tree        6a41...e3b0 27 of 32 left
sent        3 envelope(s), 1 receipt(s), 1 sweep(s)
mode        plane
facade      https://host-a.example.com
host        https://host-a.example.com answering
host        https://host-b.example.com answering
history     https://terms.host-a.example.com serves terms (2 priced class(es))
```

It reads only: it publishes nothing and spends nothing.
