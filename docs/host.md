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

The host must run with no broadcaster: a carrier is a record, never a
transaction for the chain (spec section 8.1). The reference host has none.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `BBOX_OFFICES` | required | the office identifiers this host carries, a comma list. Every `tm_bbox_` topic in `OVERLAY_TOPICS` must be one of them, or the module refuses to start: a bbox topic left to the host's default manager would admit anything |
| `BBOX_STATE_DIR` | required | an absolute directory kept for the host's life (below) |
| `BBOX_RETENTION_DAYS` | 31 | how long what a host no longer answers is kept, from first sight. At least 31 |
| `BBOX_MAX_BEEF` | 262144 | the host's BEEF bound, in bytes. At least 262144 |
| `BBOX_LISTEN` | none | `host:port` of the terms route (below) |
| `BBOX_PRICES` | none | the classes this host prices, for example `history=5,history-after=5`. Only the priceable classes; a price of 0 charges nothing |
| `BBOX_PAYEE_KEY` | none | the payee's private key, 64 lowercase hex characters: the terms route's BRC-104 identity and the key payments are derived from |
| `BBOX_HEADERS_URL` | `OVERLAY_CHAIN_TRACKER_URL` | the header source payments are verified against, in the reference host's `/v1` shape |

A price needs `BBOX_LISTEN`, `BBOX_PAYEE_KEY` and a header source. Any
mistake stops the host before its port opens.

## The state directory

- `outpoints.jsonl` holds the outpoint rows (spec sections 8.2 and 8.4):
  every carrier any office admitted with the funding output it spent, every
  drop, and every outpoint an admitted sweep spent. It is what keeps a
  funding output from paying for a second answered carrier, so it is kept
  for the host's life, backed up with the database, and never edited. Each
  line is flushed to disk before the admission it records completes.
- `payments.jsonl` holds every payment the terms route accepted: the Atomic
  BEEF, the derivation prefix and suffix, the sender's identity key, the
  satoshis and the class. The route answers once the line is on disk; the
  operator's wallet takes each payment with BRC-100 `internalizeAction`
  (protocol `wallet payment`, output 0, the remittance as written), which
  also broadcasts it. Until then a payer can still spend the coins
  elsewhere, so an operator settles promptly.

## The terms route

With `BBOX_LISTEN` set, the module serves, beside the host:

| Route | Answer |
| --- | --- |
| `GET /ls_bbox/terms` | the terms document of spec section 7.4; 404 when the host prices nothing |
| `POST /.well-known/auth` | the BRC-104 handshake |
| `POST /lookup` | the same BRC-24 questions as the host's `/lookup` |

Its base URL is what the host publishes as `ls_bbox`'s base (BRC-180
`metanet.overlays`). A reverse proxy in front of it must pass the path
unchanged, because a BRC-104 signature covers it.

A free question is answered with or without BRC-104. A question of a class
the host prices must be asked over BRC-104: without an `x-bsv-payment`
header it is answered 402 with `x-bsv-payment-version` (`1.0`),
`x-bsv-payment-satoshis-required` and `x-bsv-payment-derivation-prefix`,
signed by the payee's identity key; with one, it is answered once output 0
of the payment pays at least the price, P2PKH, to the key BRC-29 derives for
the prefix, the suffix and the asker, the prefix is one this route issued,
the payment verifies against the host's headers, and its txid was never used
before. One payment buys one question. The same class asked on the host's
own `/lookup` is refused with an error that names this route.

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
duplicate. Retention runs every hour.

On start the host hands the module the unspent outputs of its topics and
its storage. The module reloads the outpoint rows, indexes every output,
and reads from storage every carrier and sweep the rows name that the
unspent rows did not carry (a sweep's tombstone that something spent).
Statuses are functions of that set, so the order storage returns rows in
changes nothing.

## Metrics

| Name | Labels | Meaning |
| --- | --- | --- |
| `bbox_admitted_total` | `kind` | transactions admitted: `envelope`, `receipt`, `sweep`, `spend` |
| `bbox_refused_total` | `reason` | submissions refused, by the reason of spec sections 3.1, 4.6, 5 and 8.1 |
| `bbox_dropped_total` | `why` | carriers and sweeps dropped: `evidence`, `retention` |
| `bbox_retractions_total` | | outpoints recorded as swept |
| `bbox_lookups_total` | `class` | questions answered |
| `bbox_payments_total` | `result` | `requested` (a 402), `accepted`, `refused`, `replayed` |
| `bbox_offices`, `bbox_envelopes`, `bbox_receipts`, `bbox_sweeps`, `bbox_outpoint_rows` | | gauges of what the index holds |

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
index.
