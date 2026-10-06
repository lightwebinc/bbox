# Running a bbox host

A bbox host is an ordinary overlay host that loads the bbox module: one
file, `host/bundle/bbox-module.js`, which mounts the topic manager
`tm_bbox_<name>_<suffix>` for each office it carries and the lookup service
`ls_bbox` for all of them ([spec.md](spec.md) sections 7 and 8). The module
imports nothing but `@bsv/sdk` (the host's own copy) and Node's built-ins;
bcommon is inlined.

## Build and deploy

```console
$ (cd host && npm ci)   # once
$ make module           # writes host/bundle/bbox-module.js
```

Copy the file into a directory beside the host's `node_modules` (for
example `<host>/modules/bbox/bbox-module.js`) and name it by absolute path:

```sh
OVERLAY_TOPICS=tm_bbox_example_office_qzxkvbmwtr
OVERLAY_MODULES=/srv/overlay/modules/bbox/bbox-module.js
BBOX_OFFICES=example_office_qzxkvbmwtr
BBOX_STATE_DIR=/var/lib/bbox
```

Or build the host image, a reference overlay host image with the module
built in (`host/Dockerfile`): its entrypoint requires `BBOX_OFFICES`,
derives `OVERLAY_TOPICS` from it when unset, and keeps the state directory
at `/var/lib/bbox`, where a volume belongs.

```console
$ make docker-build-host REFERENCE_HOST_IMAGE=<reference overlay host image>
```

The host must run with no broadcaster: a carrier is a record, never a
transaction for the chain (spec section 8.1). The reference host has none.
The module's own arcade (`BBOX_ARCADE_URL`) carries payments only, never a
carrier.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `BBOX_OFFICES` | required | the office identifiers this host carries, a comma list. Every `tm_bbox_` topic in `OVERLAY_TOPICS` must be one of them, or the module refuses to start: a bbox topic left to the host's default manager would admit anything |
| `BBOX_STATE_DIR` | required | an absolute directory kept for the host's life (below) |
| `BBOX_RETENTION_DAYS` | 31 | how long what a host no longer answers is kept, from first sight. At least 31 |
| `BBOX_MAX_BEEF` | 262144 | the host's BEEF bound, in bytes. At least 262144 |
| `BBOX_LISTEN` | none | `host:port` of the terms route (below) |
| `BBOX_PRICES` | none | the classes this host prices, for example `history=5,history-after=5`. Only the priceable classes; a price of 0 charges nothing. A class and its `-after` form are priced together or not at all |
| `BBOX_PAYEE_KEY` | none | the payee's private key, 64 lowercase hex characters: the terms route's BRC-104 identity and the key payments are derived from |
| `BBOX_HEADERS_URL` | `OVERLAY_CHAIN_TRACKER_URL` | the header source payments are verified against, in the reference host's `/v1` shape |
| `BBOX_SESSIONS` | 10000 | the most BRC-104 sessions the terms route keeps. Past it the least recently used is forgotten |
| `BBOX_SESSION_TTL` | 600 | seconds a BRC-104 session is kept once idle (not used for a request) |
| `BBOX_HANDSHAKES_PER_SEC` | 4 | BRC-104 handshakes a second the terms route answers (below); decimals allowed |
| `BBOX_HANDSHAKE_BURST` | 8 | the most handshakes the terms route answers at once |
| `BBOX_HANDSHAKES_PER_ADDR_PER_SEC` | 1 | handshakes a second one remote address is answered; decimals allowed |
| `BBOX_HANDSHAKE_ADDR_BURST` | 4 | the most handshakes one remote address is answered at once |
| `BBOX_RESPONSES_PER_SEC` | 5 | signed responses a second one BRC-104 session is given; decimals allowed |
| `BBOX_RESPONSE_BURST` | 20 | the most signed responses one session is given at once |
| `BBOX_ARCADE_URL` | none | arcade, which the host broadcasts each payment through before it answers (`POST /tx`, `GET /tx/<txid>`) |
| `BBOX_ASSET_URL` | none | the node's asset service: the spend view of a payment's inputs and the proof of a held or watched payment. Without both URLs every payment is held until it mines |
| `BBOX_ACCEPT_THRESHOLD_SATS` | 25000000 | the largest payment answered on the network's acceptance; above it a payment is held until it mines |
| `BBOX_ACCEPT_PAYER_LIMIT` | the threshold | satoshis one payer may have answered fast and not yet mined; past it, held |
| `BBOX_ACCEPT_TOTAL_LIMIT` | ten thresholds | the same across every payer |
| `BBOX_ACCEPT_WINDOW` | `1h` | how long a fast payment counts against the limits unless it mines first (`ms`, `s`, `m`, `h`) |
| `BBOX_ACCEPT_WATCH` | `0s` | how long the host keeps watching for a conflict after arcade's acceptance before it answers |

A price needs `BBOX_LISTEN`, `BBOX_PAYEE_KEY` and a header source.
`history` and `history-after` sell one walk, so a host that prices one and
not the other (`history=5` alone) is refused: the free form would answer
every page but the first for nothing. Any mistake stops the host before its
port opens.

## The state directory

- `outpoints.jsonl` holds the outpoint rows (spec sections 8.2 and 8.4):
  every carrier any office admitted with the funding output it spent, every
  drop, and every outpoint an admitted sweep spent. It is what keeps a
  funding output from paying for a second answered carrier, so it is kept
  for the host's life, backed up with the database, and never edited. Each
  line is flushed to disk before the admission it records completes.
- `payments.jsonl` holds every payment the terms route accepted: the Atomic
  BEEF, the derivation prefix and suffix, the sender's identity key, the
  satoshis, the class and the outpoints it spends. The route answers once
  the line is on disk, and each line carries what the host decided
  ([Payment acceptance](#payment-acceptance)). The host broadcasts the
  payment; the payee still settles it: see
  [Settling payments](#settling-payments).
- `unsettleable.jsonl` holds each fast payment the watch found lost
  (double-spent, refused, or never mined), one JSON line each.

The module holds every carrier line of `outpoints.jsonl` in memory for the
host's life, and nothing bounds it: about 1 KB of memory and 400 bytes of
journal for each envelope ever admitted, dropped or not (a receipt of 8
acknowledgements about 2 KB and 900 bytes), so 1,000,000 carriers cost
about 1 to 2 GB of memory and up to 1 GB of disk (`bbox_carrier_lines`
counts them). Give Node the heap for the carriers the host expects
(`--max-old-space-size`), and watch the gauge.

A state directory written by an earlier build loads unchanged, and an
earlier build loads this one's: the journal's lines are the same three
kinds. A sweep admitted in a second office is named there by one more line
of the kind a sweep always had, and a `payments.jsonl` line without its
outpoints has them read from its transaction.

### Backing it up

Both files are append-only, so a copy taken while the host runs is safe:
at worst its last line is cut short, and both readers skip such a line.
`outpoints.jsonl` is the file that matters: a host that loses it can answer
a second carrier on a funding output whose first it already answered, and
nothing else holds it. Copy the whole directory once a day beside the
database backup, and keep several days:

```sh
#!/bin/sh
# /usr/local/sbin/bbox-state-backup: a dated copy of the state directory,
# the last 7 kept.
set -eu
src=/var/lib/bbox
dst=/var/backups/bbox-state
install -d -m 0700 "$dst"
tar -C "$(dirname "$src")" -czf "$dst/bbox-state-$(date -u +%Y%m%d).tar.gz.tmp" "$(basename "$src")"
mv "$dst/bbox-state-$(date -u +%Y%m%d).tar.gz.tmp" "$dst/bbox-state-$(date -u +%Y%m%d).tar.gz"
ls -1 "$dst"/bbox-state-*.tar.gz | sort | head -n -7 | xargs -r rm -f
```

Run it daily, from a systemd timer (`OnCalendar=daily`, `Persistent=true`)
or cron, as a user that can read the directory (it is mode 0700). The
copies hold `payments.jsonl`, which is money until it is settled: keep them
as private as the directory.

To restore, stop the host, unpack the newest copy over `BBOX_STATE_DIR`,
and start it. Rows written after the copy are lost. On start the module
writes again the row of every carrier and sweep its storage still holds,
but a carrier dropped after the copy is no longer in storage, so its row is
gone and its funding output could answer a second carrier. Restore only
when the file is lost or damaged, never to roll the host back.

## The terms route

With `BBOX_LISTEN` set, the module serves, beside the host:

| Route | Answer |
| --- | --- |
| `GET /ls_bbox/terms` | the terms document of spec section 7.4; 404 when the host prices nothing |
| `POST /.well-known/auth` | the BRC-104 handshake |
| `POST /lookup` | the same BRC-24 questions as the host's `/lookup` |

Its base URL is what the host publishes as `ls_bbox`'s base (BRC-180
`metanet.overlays`), and it is an origin with no path: a BRC-104 client
shakes hands at the origin's `/.well-known/auth`, and the route answers at
its root. A reverse proxy in front of it gives it an origin of its own (a
name, or a port) and passes the path unchanged, because a BRC-104
signature covers it.

A free question is answered with or without BRC-104. A question of a class
the host prices must be asked over BRC-104: without an `x-bsv-payment`
header it is answered 402 with `x-bsv-payment-version` (`1.0`),
`x-bsv-payment-satoshis-required` and `x-bsv-payment-derivation-prefix`,
signed by the payee's identity key; with one, it is answered once output 0
of the payment pays at least the price, P2PKH, to the key BRC-29 derives for
the prefix, the suffix and the asker, the prefix is one this route issued,
the payment verifies against the host's headers, its txid was never used
before, it spends no coin an accepted payment spent, and it passes
[payment acceptance](#payment-acceptance). Two transactions spending one
coin can both verify and at most one can ever be mined: the second is refused 409
`ERR_PAYMENT_CONFLICT`, as a used txid is refused 409
`ERR_PAYMENT_REPLAYED`. The coins of every accepted payment are kept in
`payments.jsonl` and refused again after a restart. One payment buys one
question. The same class asked on the host's
own `/lookup` is refused with an error that names this route.

### The session bound

A BRC-104 handshake is unauthenticated, and the SDK's own session store
keeps every session for the life of the process, so the route keeps its
own, bounded: at most `BBOX_SESSIONS` sessions, each forgotten once idle
for `BBOX_SESSION_TTL` seconds, and past the cap the least recently used is
forgotten first. A client whose session was forgotten is refused and shakes
hands again; the SDK's client does that by itself. A flood of handshakes
therefore costs a host a bounded store and makes honest clients shake hands
again, never unbounded memory. `bbox_sessions` is the store's size and
`bbox_sessions_evicted_total{why="cap"|"idle"}` what it forgot.

### The handshake budget

The session bound holds memory, not work: each handshake costs a signature
on the host's own process, about 10 a second on a test host. So the route
answers handshakes from a budget, two token buckets checked before the
request body is read and before any signature work:

- the route's: `BBOX_HANDSHAKES_PER_SEC` a second, up to
  `BBOX_HANDSHAKE_BURST` at once;
- each remote address's: `BBOX_HANDSHAKES_PER_ADDR_PER_SEC` a second, up to
  `BBOX_HANDSHAKE_ADDR_BURST` at once. IPv6 addresses share a bucket per
  /64; an IPv4-mapped address is its IPv4 address.

An address's bucket is checked first, so one address that floods spends its
own budget, not the route's. A handshake over either is answered 429 with
`Retry-After` (seconds) and `ERR_RATE_LIMITED`, and takes nothing from
either bucket. The defaults, 4 a second and 1 per address, leave most of
the process to the host's own work; an honest client shakes hands once per
session, so the budget is far above what clients need. The address is the
connection's: behind a reverse proxy every client shares the proxy's
address, so raise the per-address limit to the route's, or limit at the
proxy instead. `bbox_handshakes_total{result}` counts every handshake:
`accepted`, `failed` (malformed or not verified), `limited_address`,
`limited_global`.

### The response budget

Every authenticated request is answered with a signature, so one handshake
must not buy signatures without end. Each session has a token bucket,
`BBOX_RESPONSES_PER_SEC` a second up to `BBOX_RESPONSE_BURST` at once,
taken only by a request that verified, so nobody spends another session's.
A request over it is answered 429 with `Retry-After` and
`ERR_RATE_LIMITED`, and nothing is signed. A BRC-104 client reports an
unsigned answer as a failed authentication: `bbox history` waits as the
header says (1 to 30 seconds) and asks once more. The defaults, 5 a second
and 20 at once, are far above what a reader walking pages asks.

An authenticated request sent again byte for byte is refused 401
`ERR_AUTH_REPLAYED` before any signature work. The route remembers the
last 65536 requests it verified; the session's budget bounds what an older
replay can cost. `bbox_requests_total{result}` counts both refusals:
`replayed`, `limited`.


## Payment acceptance

A paid question is answered only after the payment is held to bcommon's
acceptance rule (package `acceptance`, the same rule for every layer):

1. the route's own checks (prefix, output 0 pays the derived key, SPV
   against the host's headers, a txid and coins never used before);
2. finality (lock time 0, or every input final) and conservation (every
   input carries its source, and the outputs do not exceed the inputs);
3. the value: at or below `BBOX_ACCEPT_THRESHOLD_SATS`, and within the
   payer's and the total limits, it is fast; otherwise it is held;
4. the host broadcasts it through `BBOX_ARCADE_URL`, unmined ancestors first,
   held or fast: this is how the payee collects;
5. on the fast path the host answers only once arcade reports the network
   took it (`SEEN_ON_NETWORK`, `ACCEPTED_BY_NETWORK`, ...), with no
   `DOUBLE_SPEND_ATTEMPTED` and no competing transaction, and the node shows
   every input unspent or spent by this payment.

A refusal (arcade `REJECTED`, a definitive HTTP refusal, an input spent by
another transaction, a payment that is not final or spends more than it
holds) is answered 400 `ERR_PAYMENT_REFUSED`. Anything else that is not
fast (above the threshold, a limit reached, a conflict reported, no
verdict within 10 s, a spend view that cannot say) is answered 402
`ERR_PAYMENT_HELD` with the reason and no BRC-105 headers, so a client
does not pay again. The held payment is recorded in `payments.jsonl` with
`"decision":"hold"`, keeps its coins, and the same payment sent again is
answered once the node serves its proof and that proof verifies against the
host's headers. A host with no arcade or no node holds every payment (with
arcade alone it still broadcasts, and with the node alone it answers a
payment the node shows mined). At most 100,000 fast payments are watched at
once; past that a payment is held (`watch-limit`).

Every fast payment is watched on the host's background tick (every 30 s,
the 500 least recently checked each time) until it mines. One whose input
another transaction spent, that arcade refuses, or that has not mined
within a day, is written to `unsettleable.jsonl` in the state directory,
logged, and counted; its payer is then held for confirmation on every
later payment. Flags outlive a restart: the host reads `unsettleable.jsonl`
when it starts, so an operator unflags a payer by removing its lines and
restarting. Nothing is clawed back: the answer was given. A restart
watches the fast lines of the last day again, and counts those of the last
window against the limits.

The ledger line carries `decision` (`fast`, `hold` or `mined`) and
`reason`; `bbox payee settle` reads it as before.

## Settling payments

A payment the terms route accepts is a BRC-29 output to a key derived from
`BBOX_PAYEE_KEY`, verified against the host's headers and recorded in
`payments.jsonl` before the question is answered. The host broadcasts it
and, on the fast path, answers once the network took it
([Payment acceptance](#payment-acceptance)). **Until it mines, the payer
can still race a conflicting spend to a miner, and the payment is then
worth nothing**: a payment is money only once it is mined and in the
payee's pool. Settle promptly and on a schedule.

Settling is the payee wallet's `internalizeAction` (BRC-100, protocol
`wallet payment`, output 0, the remittance as written), which broadcasts
the payment and takes its output. The bbox command does it with a home
whose identity is the payee:

```console
$ bbox -home /srv/payee init                          # once: the payee's wallet
$ bbox -home /srv/payee payee key -out /etc/bbox/payee.env
$ # BBOX_PAYEE_KEY from payee.env goes into the host's environment
$ bbox -home /srv/payee payee settle /var/lib/bbox/payments.jsonl
settled 7c1e...: 5 sat for history from 03a1b2c3d4e5
1 payment(s) settled, 5 sat; 0 settled before; 0 not settled; 0 refused (0 before); pool 1 output(s), 5 sat
```

`payee key` writes the home's identity key as `BBOX_PAYEE_KEY=<hex>` to a
new file at mode 0600. `payee settle` reads the ledger (the host writes it;
the command only reads it), and for each payment the home has not settled
checks that output 0 pays the key the home derives for the prefix, the
suffix and the payer, verifies it against the home's headers, broadcasts it
through the home's settlement leg, waits for its proof, adds it to the
home's pool, and records its txid in the home as settled. It needs the
home's `header_url`, `asset` and `settle` ([usage.md](usage.md)). One
payee serving several hosts names every host's ledger in one run (`payee
settle a/payments.jsonl b/payments.jsonl`); a payment in two is settled
once. Every payment is broadcast before any is waited for, so a run takes
about one block however many lines it settles; `-in-flight` (default 16, at most 64)
bounds how many are broadcast and not yet mined at once.

**A payment its payer double-spent before settle.** The payer can spend
the payment's input elsewhere at any time until the payment mines. The
payee then sees this, at once rather than after waiting for a block:

```text
payment 7c1e...: (5 sat, history): REFUSED, NEVER SETTLES: input 0 (41d0...77aa.1) is spent by 9b3f...0c2e; the payer took the coins back after the question was answered
0 payment(s) settled, 0 sat; 3 settled before; 0 not settled; 1 refused (0 before); pool 3 output(s), 15 sat
bbox: 1 payment(s) refused by the network: their payers spent the coins elsewhere, and they will never settle
```

and the command exits 1. A refusal is the settlement leg refusing it for
good, or the node showing one of its inputs spent by another transaction.
The second check matters: a settlement leg's acceptance is not the
network's (arcade has answered `ACCEPTED_BY_NETWORK` for a payment whose
input was already spent and mined), so settle asks the node about the
inputs before it waits. The payment is recorded in the payee's home as
refused and every later run counts it in `refused (N before)` without
trying it again, so a timer does not fail on it forever. Nothing recovers
it: the question it paid for was answered and the payee has nothing. That
is the price of the window between answering and settling, and why the
window should be short. The host's own `bbox_payments_total{decision="fast"}`
counted it when it answered, so accepted minus settled is what the payee
lost or has still to settle.

Any other failure (the leg unreachable, no block inside the wait) is
reported as `NOT SETTLED`, left for the next run, and also exits 1.
Running settle again is safe: what is settled is skipped, and the pool
refuses an outpoint twice. A BRC-100 wallet holding the payee key can
settle the same lines itself.

## Retention and restore

Open envelopes are always kept, and so is an envelope dated ahead of the
host's clock. Everything else (receipts, sweeps, and envelopes that are
acknowledged, expired, out of the window, superseded or retracted) is
dropped once `BBOX_RETENTION_DAYS` have passed since the host first saw it.
Beyond that, at most 8 superseded carriers are kept per funding output (64
once its winner is acknowledged), and the rest are dropped at once. To drop
is to take a carrier out of every answer, mark its outpoint row, and delete
its output from the host's storage; the engine's record that the
transaction was applied stays, so a dropped carrier offered again is a
duplicate. Retention runs every hour. The output is deleted at once: after
the admission that dropped it, and after each retention run. A storage
with no `deleteOutput` keeps what was dropped; the module says so in the
log once and keeps no list of it.

On start the host hands the module the unspent outputs of its topics and
its storage. The module reloads the outpoint rows, indexes every output,
and reads from storage every carrier and sweep the rows name that the
unspent rows did not carry (a sweep's tombstone that something spent).
Statuses are functions of that set, so the order storage returns rows in
changes nothing. An admission that arrives while the index is being rebuilt
waits for the rebuild, and is then decided as any other.

One mined sweep may be published in several offices. It is indexed in each
office that admitted it and named there in the outpoint rows, so each
office answers it, a restart reads it back in each, and dropping it from
one topic leaves it answering in the others.

## Metrics

| Name | Labels | Meaning |
| --- | --- | --- |
| `bbox_admitted_total` | `kind` | distinct transactions admitted since the host started: `envelope`, `receipt`, `sweep`, `spend` |
| `bbox_admitted_repeats_total` | `kind` | admissions of a transaction already counted, which the engine lets through only when two submissions of it race (below) |
| `bbox_refused_total` | `reason` | submissions refused, by the reason of spec sections 3.1, 4.6, 5 and 8.1 |
| `bbox_dropped_total` | `why` | carriers and sweeps dropped: `evidence`, `retention` |
| `bbox_retractions_total` | | outpoints recorded as swept, the fee input of each sweep included (a sweep of one funding output counts 2) |
| `bbox_lookups_total` | `class` | questions answered |
| `bbox_payments_total` | `decision`, `reason` | paid questions by what the host did: `requested` (`no-payment`, a 402 with the price); `fast` (`at-or-below-threshold`, `mined`); `hold` (`above-threshold`, `payer-limit`, `total-limit`, `payer-flagged`, `no-broadcast-leg`, `broadcast-unconfirmed`, `no-network-verdict`, `double-spend-attempted`, `spend-view-unknown`); `refuse` (`malformed`, `wrong-script`, `spv-failed`, `not-final`, `outputs-exceed-inputs`, `network-refused`, `double-spent`, `replayed`, `conflict`) |
| `bbox_payment_events_total` | `kind` | fast payments the watch resolved: `confirmed`, `double-spent`, `refused`, `unmined` (all but the first flag the payer) |
| `bbox_payments_unmined` | | fast payments being watched |
| `bbox_requests_total` | `result` | authenticated requests refused before any signature: `replayed`, `limited` (over the session's budget, answered 429) |
| `bbox_sessions` | | BRC-104 sessions the terms route keeps |
| `bbox_sessions_evicted_total` | `why` | sessions forgotten: `cap`, `idle` |
| `bbox_handshakes_total` | `result` | BRC-104 handshakes at the terms route: `accepted`, `failed`, `limited_address`, `limited_global` (over the budget, answered 429) |
| `bbox_offices`, `bbox_envelopes`, `bbox_receipts`, `bbox_sweeps`, `bbox_outpoint_rows` | | gauges of what the index holds |
| `bbox_carrier_lines` | | carriers ever admitted, each a journal line held in memory for the host's life |

Counters and gauges count different things. A `_total` counter counts
events since this process started and starts again at 0 on a restart; a
gauge is what the index holds now, restored on start and lowered by
retention. So `bbox_admitted_total{kind="sweep"}` and `bbox_sweeps` agree
only on a host that has not restarted or dropped anything since the sweeps
arrived. In mode plane a client sends a sweep to the facade and then
directly to every host it names, so a host on the plane may be offered the
same sweep twice within a second; when both submissions pass the engine's
duplicate check before either is applied, the manager sees it twice. The
second is counted in `bbox_admitted_repeats_total`, not again in
`bbox_admitted_total`. A manager remembers the last 4096 txids it counted.

The engine checks a submission's scripts and proofs before any topic
manager runs, and the SDK's interpreter itself refuses a high-S input
signature and a non-minimal push, so such a carrier is refused by the host
and never reaches `bbox_refused_total`.

## Checking a build

`make verify` runs the module's tests: every golden transaction through the
topic manager and through the real overlay engine, every refusal record
inside a real carrier, every question class, the terms route over HTTP with
the SDK's own BRC-104 client paying its 402, and restore from the engine's
storage in any order. `make e2e REFERENCE_HOST=<dir>` runs the bundle on a
local reference host with a throwaway MySQL in Docker, through submit,
lookup, a receipt, a sweep, a paid question, a restart and the restored
index. `make e2e-client REFERENCE_HOST=<dir>` runs the `bbox` command
against two such hosts (docs/usage.md), and `make quickstart-check` runs
QUICKSTART.md against the images.
