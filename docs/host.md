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
| `BBOX_ASSET_URL` | none | the node's asset service: the spend view of a payment's inputs and the proof of a held or watched payment. Without arcade and a spend view (this, or `BBOX_CHAIN`) every payment is held until it mines |
| `BBOX_CHAIN` | none | `woc:main` or `woc:test`: WhatsOnChain as that spend view and proof source, for a host with no node; refused beside `BBOX_ASSET_URL`. Its word that an input is unspent is trusted; every proof is checked against the host's headers |
| `BBOX_WOC_KEY` | none | a WhatsOnChain API key for `BBOX_CHAIN`, past the free tier's 3 requests a second |
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

The terms route, its session, handshake and response budgets, payment
acceptance, the generic state files (`payments.jsonl`,
`unsettleable.jsonl`), settling, backup and restore are bcommon's: see
[the host module](https://github.com/lightwebinc/bcommon/blob/main/docs/host-module.md).
What follows is only what is bbox's own.

## The state directory

`BBOX_STATE_DIR` holds bcommon's `payments.jsonl` and `unsettleable.jsonl`
and bbox's journal, `outpoints.jsonl`: the outpoint rows of spec sections
8.2 and 8.4, three row kinds (every carrier any office admitted with the
funding output it spent, every drop, and every outpoint an admitted sweep
spent). It keeps a funding output from paying for a second answered
carrier, so it is kept for the host's life, backed up with the database,
and never edited. Each line is flushed to disk before the admission it
records completes. A host that loses it can answer a second carrier on a
funding output whose first it already answered, and nothing else holds it.

The module holds every carrier line in memory for the host's life, and
nothing bounds it: about 1 KB of memory and 400 bytes of journal for each
envelope ever admitted, dropped or not (a receipt of 8 acknowledgements
about 2 KB and 900 bytes), so 1,000,000 carriers cost about 1 to 2 GB of
memory and up to 1 GB of disk (`bbox_carrier_lines` counts them). Give
Node the heap for the carriers the host expects (`--max-old-space-size`),
and watch the gauge.

A state directory written by an earlier build loads unchanged, and an
earlier build loads this one's: the journal's lines are the same three
kinds. A sweep admitted in a second office is named there by one more line
of the kind a sweep always had, and a `payments.jsonl` line without its
outpoints has them read from its transaction.

Back up and restore with the host module's script, with
`src=/var/lib/bbox`. On start the module writes again the row of every
carrier and sweep its storage still holds, but a carrier dropped after the
copy is no longer in storage, so its row is gone and its funding output
could answer a second carrier: restore only when the file is lost or
damaged, never to roll the host back.

## The terms route

With `BBOX_LISTEN` set, the module serves the host module's terms route:
`GET /ls_bbox/terms` (the terms document of spec section 7.4; 404 when the
host prices nothing), `POST /.well-known/auth` and `POST /lookup` (the same
BRC-24 questions as the host's `/lookup`). Its base URL is what the host
publishes as `ls_bbox`'s base (BRC-180 `metanet.overlays`), an origin with
no path. Only the `history` classes are priceable, and one payment buys
one question (one page). The same class asked on the host's own `/lookup`
is refused with an error that names this route. A payment held for
confirmation is answered 202 `ERR_PAYMENT_HELD`. Over a budget the route
answers 429, and `bbox history` waits as `Retry-After` says (1 to 30
seconds) and asks once more.

## Settling payments

The bbox command settles with a home whose identity is the payee:

```console
$ bbox -home /srv/payee init                          # once: the payee's wallet
$ bbox -home /srv/payee payee key -out /etc/bbox/payee.env
$ # BBOX_PAYEE_KEY from payee.env goes into the host's environment
$ bbox -home /srv/payee payee settle /var/lib/bbox/payments.jsonl
settled 7c1e...: 5 sat for history from 03a1b2c3d4e5
1 payment(s) settled, 5 sat; 0 settled before; 0 not settled; 0 refused (0 before); pool 1 output(s), 5 sat
```

`payee key` writes the home's identity key as `BBOX_PAYEE_KEY=<hex>` to a
new file at mode 0600. `payee settle` needs the home's `header_url`,
`chain` and `settle` ([usage.md](usage.md)); it takes several hosts'
ledgers in one run (a payment in two is settled once), and `-in-flight`
(default 16, at most 64) bounds how many are broadcast and not yet mined
at once. A payment its payer double-spent is reported `REFUSED, NEVER
SETTLES`, recorded as refused in the home, and the command exits 1; any
other failure is `NOT SETTLED`, left for the next run, and also exits 1.
`bbox_payments_total{decision="fast"}` minus what settled is what the
payee lost or has still to settle.

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

## The Go module

The same module exists in Go, `module/` in this repository, written to
bcommon's engine-neutral contract (`github.com/lightwebinc/bcommon/module`)
for a Go overlay host that carries the adapter: `tm_bbox_<name>_<suffix>`
for each office `BBOX_OFFICES` names and `ls_bbox` for all of them, with the
same admissions and refusals (the rules are `boxrec.Admit`, and every
refusal is raised), the same statuses, evidence cap, retention, restore and
question classes, the same terms route (bcommon's Go `termsroute`: the same
paths, status codes, budgets, metric names and ledger lines as the route
above) and the same metrics. It reads the same variables and refuses the
same mistakes, and has one of its own:

| Variable | Default | Meaning |
| --- | --- | --- |
| `BBOX_JOURNAL_SYNC_MS` | 100 | the outpoint journal's group commit window: lines are written to `outpoints.jsonl` at once and fsynced together this often, off the admission's path. 0 fsyncs each line before the admission completes, as the module above does. At most 60000 |

The state directory is the same: `outpoints.jsonl` holds the same lines,
byte for byte, so a host can move between the two modules with its
journal, and `payments.jsonl` has the same layout, which `bbox payee
settle` reads as before. With a window, a process that dies loses no line
(each is written before the admission completes) and a power loss loses at
most the last window's, as a store that syncs its write-ahead log on a
timer does.

On the Go host the topic manager reads the submission's bytes exactly as
they arrived (`module.WithSubmission`), as the module above reads the
submitted BEEF: the beef rule counts what the wire declares. `ls_bbox`
answers the host's `/lookup` with outputs the host hydrates from its
storage, and the terms route hydrates its answers the same way. A dropped
carrier's output is deleted through `DeleteOutput` on the storage a restore
is handed; the host keeps its record that the transaction was applied. The
host logs `bbox module mounted` with the topics, the terms route, the
priced classes, the budgets, the payment acceptance, the journal's window
and the build. Its tests drive it with the transaction, refusal and query
vectors and this module's own cases, through a stand-in for an engine's
admission path and a restart, and the terms route over HTTP with go-sdk's
BRC-104 client paying its 402.

## Metrics

| Name | Labels | Meaning |
| --- | --- | --- |
| `bbox_admitted_total` | `kind` | distinct transactions admitted since the host started: `envelope`, `receipt`, `sweep`, `spend` |
| `bbox_admitted_repeats_total` | `kind` | admissions of a transaction already counted, which the engine lets through only when two submissions of it race (below) |
| `bbox_refused_total` | `reason` | submissions refused, by the reason of spec sections 3.1, 4.6, 5 and 8.1 |
| `bbox_dropped_total` | `why` | carriers and sweeps dropped: `evidence`, `retention` |
| `bbox_retractions_total` | | outpoints recorded as swept, the fee input of each sweep included (a sweep of one funding output counts 2) |
| `bbox_lookups_total` | `class` | questions answered |
| `bbox_payments_total` | `decision`, `reason` | paid questions by what the host did: `requested` (`no-payment`, a 402 with the price); `fast` (`at-or-below-threshold`, `mined`); `hold`, answered 202 (`above-threshold`, `payer-limit`, `total-limit`, `payer-flagged`, `no-broadcast-leg`, `broadcast-unconfirmed`, `no-network-verdict`, `double-spend-attempted`, `spend-view-unknown`); `refuse` (`malformed`, `wrong-script`, `spv-failed`, `not-final`, `outputs-exceed-inputs`, `network-refused`, `double-spent`, `replayed`, `conflict`) |
| `bbox_payment_events_total` | `kind` | fast payments the watch resolved: `confirmed`, `double-spent`, `refused`, `unmined` (all but the first flag the payer) |
| `bbox_payments_unmined` | | fast payments being watched |
| `bbox_requests_total` | `result` | authenticated requests refused before any signature: `replayed`, `limited` (over the session's budget, answered 429) |
| `bbox_sessions` | | BRC-104 sessions the terms route keeps |
| `bbox_sessions_evicted_total` | `why` | sessions forgotten: `cap`, `idle` |
| `bbox_handshakes_total` | `result` | BRC-104 handshakes at the terms route: `accepted`, `failed`, `limited_address`, `limited_global` (over the budget, answered 429) |
| `bbox_offices`, `bbox_envelopes`, `bbox_receipts`, `bbox_sweeps`, `bbox_outpoint_rows` | | gauges of what the index holds |
| `bbox_carrier_lines` | | carriers ever admitted, each a journal line held in memory for the host's life |
| `bbox_journal_lines_total`, `bbox_journal_wait_seconds_total` | | the Go module: journal lines written, and the time admissions spent writing them |
| `bbox_journal_syncs_total`, `bbox_journal_sync_seconds_total` | `result` (the first) | the Go module: the journal's timed fsyncs (`ok`, `error`), and their time |

Counters and gauges count different things. A `_total` counter counts
events since this process started and starts again at 0 on a restart; a
gauge is what the index holds now, restored on start and lowered by
retention. So `bbox_admitted_total{kind="sweep"}` and `bbox_sweeps` agree
only on a host that has not restarted or dropped anything since the sweeps
arrived. In mode plane a client sends a sweep to the facade, and directly
to a host it names only when that host does not answer it from the plane
(earlier clients sent every host a direct copy at once), so a host may
still be offered the same sweep twice: by a late plane copy, by an older
client, or by anyone filling it across. When both submissions pass the
engine's duplicate check before either is applied, the manager sees it
twice. The
second is counted in `bbox_admitted_repeats_total`, not again in
`bbox_admitted_total`. A manager remembers the last 4096 txids it counted.

The engine checks a submission's scripts and proofs before any topic
manager runs, and the SDK's interpreter itself refuses a high-S input
signature and a non-minimal push, so such a carrier is refused by the host
and never reaches `bbox_refused_total`.

## Checking a build

`make verify` runs both modules' tests: every golden transaction through the
topic manager and through the real overlay engine (the Go module: through a
stand-in for an engine's admission path), every refusal record inside a real
carrier, every question class, the terms route over HTTP with the SDK's own
BRC-104 client paying its 402, and restore from the engine's storage in any
order. `make e2e REFERENCE_HOST=<dir>` runs the bundle on a
local reference host with a throwaway MySQL in Docker, through submit,
lookup, a receipt, a sweep, a paid question, a restart and the restored
index. `make e2e-client REFERENCE_HOST=<dir>` runs the `bbox` command
against two such hosts (docs/usage.md), and `make quickstart-check` runs
QUICKSTART.md against the images.
