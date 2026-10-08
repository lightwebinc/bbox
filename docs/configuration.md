# Configuration

Every setting of the `bbox` command, the container images and the host
module. Settings come from, highest first: the global flags, `BBOX_<KEY>`
in the environment (the key in upper case: `header_url` is
`BBOX_HEADER_URL`), the config file, and the built-in defaults.

No node of your own is needed. On `main` and `test` the chain services
default to public ones: WhatsOnChain for the headers every proof is checked
against and for the chain view (transactions, proofs, spends), and
GorillaPool's public arcade to broadcast. A node is the better option, and
every one of them is a setting (see [Chain services](#chain-services)). A
regtest chain has no public services and names its own.

The overlay hosts, the facade and the host a priced question goes to have
no default, on purpose: a default there would send your envelopes or your
questions to a server nobody configured, so a command that needs one and
finds none exits 2 naming the key.

## The config file

The file is `-config PATH`, else `$BBOX_HOME/config`, else
`$XDG_CONFIG_HOME/bbox/config`, else `~/.bbox/config`. A missing file is
fine. The grammar is one `key = value` per line, `#` comments, blank lines
ignored; a key it does not define, a key given twice, or a line without
`=` is an error (exit 2), never ignored.

### Mainnet, no node

```text
# ~/.bbox/config
network      = main
office       = support_qzxkvbmwtr
hosts        = https://host-a.example.com,https://host-b.example.com
facade       = https://host-a.example.com
history_host = https://terms.host-a.example.com
```

`header_url`, `chain` and `settle` are left at their defaults, `woc:main`,
`woc:main` and `arcade:main`.

### Testnet, no node

```text
network    = test
office     = support_qzxkvbmwtr
hosts      = https://host-a.example.com,https://host-b.example.com
facade     = https://host-a.example.com
```

The fund address is then a testnet address, and the defaults are
`woc:test` and `arcade:test`. A public service named for another network
than `network` (`woc:main` on test, `arcade:test` on main) is refused.

### Mainnet, with your own node and services

```text
network      = main
office       = support_qzxkvbmwtr
hosts        = https://host-a.example.com,https://host-b.example.com
facade       = https://host-a.example.com
chain        = asset:https://node.example.com
header_url   = bhs:https://headers.example.com
header_token = <the block-headers-service token>
settle       = arcade:https://arcade.example.com
```

### Without the plane

Submit every object to each host yourself:

```text
mode   = unicast
hosts  = https://host-a.example.com,https://host-b.example.com
quorum = all
```

### Development sandbox (regtest)

The sandbox in [QUICKSTART.md](../QUICKSTART.md) sets these as `BBOX_*`
variables in its compose file: `network = regtest`, and `asset` (the older
spelling of `chain = asset:URL`), `rpc`, `settle = rpc:...` and
`header_url` all pointing at the local `bbox-devchain`. `rpc` is used only to mine coinbase there (coinbase: only
on a regtest chain you run, for development and tests).

## Keys

| Key | Default | Meaning |
| --- | --- | --- |
| `accept_payer_limit` | the threshold | satoshis one sender may have taken fast and not yet mined (`internalize`); past it a payment waits for its block |
| `accept_threshold_sats` | 25000000 | the largest payment `internalize` takes on the network's acceptance; above it, it waits for its block |
| `accept_total_limit` | ten thresholds | the same across every sender |
| `accept_watch` | `0s` | how long `internalize` watches for a conflict after arcade's acceptance |
| `accept_window` | `1h` | how long a fast payment counts against the limits unless it mines first |
| `arcade_key` | none | bearer token for an arcade installation named in `settle` |
| `asset` | none | the older spelling of `chain = asset:<URL>`: a Teranode node's asset API base URL (paths under `/api/v1/`). Refused beside `chain` |
| `box` | none | the box `send` addresses and `list`, `read` ask about when none is named. None: `send` uses `inbox`, and `list` and `read` ask about every box |
| `chain` | `woc:<network>` on main and test | the chain view transactions, proofs and spends are read from: `woc:main`, `woc:test`, `asset:<node URL>`, or a list (see [Chain services](#chain-services)). Needed by `fund -txid` and every command that publishes or takes a payment |
| `facade` | none | mode `plane`: the overlay host a publisher submits to (its `/submit` route), from which the plane delivers to every subscribed host. Refused in mode `unicast` |
| `fee_dust` | `100` | the least change kept as an output, in satoshis; less goes to the fee |
| `fee_floor` | `100` | the least fee one transaction pays, in satoshis |
| `fee_max_rate` | `100/1000` | the most a fee rate may be, `SATS/BYTES`: a rate above it, configured or published, is lowered to it |
| `fee_max_tx` | none | the most one transaction may pay in fee, in satoshis; a fee above it is refused, not paid |
| `fee_min_rate` | `100/1000` | with `fee_source = arc`, the least a published rate may be |
| `fee_policy_urls` | the arcade or ARC in `settle` | with `fee_source = arc`, the broadcasters whose published policy is read, comma separated; the highest rate is taken |
| `fee_rate` | `100/1000` | the miner fee rate, `SATS/BYTES`: 100 satoshis per 1000 bytes is the network's rate today. A bare number is satoshis a byte |
| `fee_source` | `static` | `static` (`fee_rate`) or `arc`: the broadcaster's published policy (`GET /v1/policy`), held between `fee_min_rate` and `fee_max_rate`, and `fee_rate` when it cannot be read |
| `fund_mined_only` | `false` | `fund -beef` refuses a payment that has not mined yet (the flag `-mined-only`) |
| `header_token` | none | the bearer token a block-headers-service (`bhs:`) asks for |
| `header_url` | `woc:<network>` on main and test | the header source every proof is checked against: `woc:main`, `woc:test`, `bhs:URL`, `arcade:URL`, `chaintracks:URL`, or an overlay bridge's base URL (native `/v1/root/<height>` and `/v1/tip`) |
| `history_host` | none | the base URL of `ls_bbox` on the host `history` asks and `terms` reads: the host's terms route (spec section 7.4). An origin, with no path: BRC-104 authenticates at the origin's `/.well-known/auth`, and the route answers `/lookup` and `/ls_bbox/terms` at its root |
| `home` | `~/.bbox` | the identity's home |
| `hosts` | none | the overlay hosts, comma separated, at most 16: every host a reader asks and compares; in mode `unicast` every host a publisher submits to; in mode `plane` the hosts other than `facade` a sweep is also sent to directly |
| `mode` | `plane` | `plane` or `unicast`: how a publisher's objects reach the hosts (spec section 9) |
| `network` | `main` | `main`, `test` or `regtest`: the fund address prefix and the header source's proof-of-work floor |
| `object_bound` | `1048576` | the plane's object bound in bytes, at most 8388608; a carrier whose BEEF is larger is not published |
| `office` | none | the office identifier a command uses when it names none |
| `originator` | `bbox` | the BRC-100 originator presented to the wallet |
| `quorum` | `all` | how many of `hosts` must hold a carrier (an envelope or a receipt) for it to count as published: `all`, `majority`, `one`, or a number up to the host count. In unicast a host that took it counts; on the plane a host that answers it by lookup does. A sweep always needs every host |
| `rpc` | none | the node's JSON-RPC URL, used only by `fund` without `-txid` or `-beef` (coinbase: only on a regtest chain you run, for development and tests) |
| `rpc_pass`, `rpc_user` | `bitcoin` | the node's basic auth, also for `settle = rpc:` |
| `settle` | `arcade:<network>` on main and test | the settlement leg for what is mined (funding trees, sweeps, a payment a recipient or a payee takes): `arcade:main`, `arcade:test`, `arcade:<url>`, `arc:<url>`, `rpc:<url>` or `tcp:<host:port>` (see [Chain services](#chain-services)) |
| `timeout` | `15s` | bounds each request; `1s` to `5m` |
| `tree_count` | `32` | outputs of a new funding tree, 1 to 1000 |
| `woc_key` | none | a WhatsOnChain API key, for `chain` and the free tier's limit of 3 requests a second |
| `woc_rate` | `3` | the WhatsOnChain requests a second the key's plan allows |

`network`, `mode` and `box` accept only the values listed; numbers and
durations must parse; anything else exits 2 naming the key. Every cap is in
[limits.md](limits.md).

## Chain services

Three settings name the chain services, each a specification with a
public default on `main` and `test`:

| Role | Key | Default | Trusted for |
| --- | --- | --- | --- |
| Headers | `header_url` | `woc:<network>` | nothing it cannot prove: each header is hashed and must carry the work its bits claim |
| Chain view | `chain` | `woc:<network>` | "unspent", "not known" and "not mined"; a transaction it serves must hash to its txid, and every proof must verify against your headers |
| Broadcast | `settle` | `arcade:<network>` | its verdict, which is also held to the chain view's spends |

A node of your own (`chain = asset:URL`, or `spend=asset:URL` for its
spend view alone) removes the one trust the chain view adds. Running
block-headers-service or arcade yourself does the same for the headers.

### Header sources

Every proof is checked against the headers of one source, so `header_url`
is the root of trust for every envelope you read.

| `header_url` | Source |
| --- | --- |
| `woc:main`, `woc:test` | the public WhatsOnChain API; must match `network` |
| `bhs:https://host:8080` | a block-headers-service you run; set `header_token` when it asks for one |
| `arcade:https://host` | an arcade installation's header server |
| `chaintracks:https://host/v2` | a chaintracks v2 service, public or your own |
| `https://bridge.example.com/v1` | an overlay bridge's native `/v1/root/<height>` and `/v1/tip` (also what `bbox-devchain` serves) |

### The chain view

| `chain` | Source |
| --- | --- |
| `woc:main`, `woc:test` | WhatsOnChain: transactions, proofs and spends; must match `network`. Its spent endpoint tells an unknown output from an unspent one, and only "unspent" reads as unspent |
| `asset:https://node.example.com` | a Teranode node's asset API, for everything |
| `asset:https://node.example.com,woc:main` | the node, and WhatsOnChain for transactions and proofs the node lacks |
| `woc:test,spend=asset:https://node.example.com` | WhatsOnChain, with the node's spend view |

Transactions and proofs are asked of each backend in turn, since every
answer is checked; spends come from one backend only, the first that
serves them. WhatsOnChain's free tier allows 3 requests a second, and bbox
paces itself to it; a busy home sets `woc_key` and `woc_rate`, or uses a
node.

### Settlement legs

| `settle` | Leg |
| --- | --- |
| `arcade:main`, `arcade:test` | GorillaPool's public arcade, no key: the default |
| `arcade:https://arcade.example.com` | an arcade installation, your own included; `arcade_key` is its bearer token |
| `arc:https://arc.example.com` | an ARC installation; `/v1` is added to a URL with no path, and `arcade_key` is sent for one that needs a key |
| `rpc:http://node.example.com:8332` | a node's `sendrawtransaction` |
| `tcp:ingress.example.com:9000` | a fabric ingress: bare EF, no answer |

Arcade can answer "accepted" for a transaction whose input was already
spent and mined, so its verdict is held to the chain view's spends.

### Fees

The miner fee is the network's rate, 100 satoshis per 1000 bytes, with a
floor of 100 satoshis a transaction (what 1000 bytes pay). `fee_rate` and `fee_floor` change
them. `fee_source = arc` follows the rate the broadcaster publishes
instead, read at most every 5 minutes and held between `fee_min_rate` and
`fee_max_rate`, so a mistaken or compromised policy endpoint cannot drain
the pool; when it cannot be read, the last answer is used for a day, then
`fee_rate`. `fee_max_tx` refuses any one fee above it. `bbox doctor` shows
the rate in force.

## Global flags

Before the command: `bbox [global flags] <command> [flags]`.

| Flag | Key | Meaning |
| --- | --- | --- |
| `-config PATH` | none | the config file |
| `-chain SPEC` | `chain` | the chain view |
| `-fee-floor SATS` | `fee_floor` | the least fee a transaction pays |
| `-fee-max-rate RATE` | `fee_max_rate` | the most a fee rate may be |
| `-fee-rate RATE` | `fee_rate` | the miner fee rate, `SATS/BYTES` |
| `-fee-source SRC` | `fee_source` | `static` or `arc` |
| `-home DIR` | `home` | the identity's home |
| `-hosts URLS` | `hosts` | overlay hosts, comma separated |
| `-header-url SOURCE` | `header_url` | header source |
| `-mode MODE` | `mode` | `plane` or `unicast` |
| `-network NET` | `network` | `main`, `test` or `regtest` |
| `-office OFFICE` | `office` | the office identifier |
| `-quorum Q` | `quorum` | `all`, `majority`, `one` or N |
| `-settle SPEC` | `settle` | the settlement leg |
| `-timeout DUR` | `timeout` | per request |
| `-v` | none | verbose; also accepted after the command |
| `-version` | none | print the version and exit |

## Command flags

| Command | Flag | Default | Meaning |
| --- | --- | --- | --- |
| `fund` | `-txid TXID` | none | import a mined payment you sent from your own wallet to the fund address, read from the chain view |
| `fund` | `-beef FILE` | none | import that payment as the BEEF your wallet hands over (`-` for standard input; binary or hex), with no lookup |
| `fund` | `-mined-only` | `fund_mined_only` | with `-beef`, refuse a payment that has not mined yet |
| `fund` | `-blocks N` | 101 | coinbase: only on a regtest chain you run (development and tests); blocks to mine, or with `-rescan` blocks to re-read |
| `fund` | `-batch N` | 30 | coinbase: only on a regtest chain you run; blocks per `generatetoaddress` call |
| `fund` | `-rescan` | false | coinbase: only on a regtest chain you run; re-read recent blocks for coinbase the pool lacks |
| `send` | `-m TEXT`, `-file PATH` | standard input | the message body |
| `send` | `-pay SATS` | none | a payment inside the envelope |
| `send` | `-ref URL,SHA256,LENGTH[,KEY]` | none | a reference, repeatable, at most 32 |
| `send` | `-expires DUR` | none; 24h with `-pay` | the envelope's life |
| `send` | `-rate R` | 1 | envelopes a second at most |
| `send`, `ack` | `-tree-count N` | `tree_count` | outputs of a new funding tree |
| `send`, `ack`, `history`, `list`, `read`, `internalize` | `-office OFFICE` | `office` | the office |
| `ack` | `-all` | false | every envelope read and not yet acknowledged |
| `drop` | `-tree TXID`, `-yes` | none | sweep every output of a funding tree |
| `list`, `read` | `-box BOX`, `-from KEY`, `-to KEY` | every box; any sender; this home | the view |
| `list` | `-fill` | false | copy each envelope a host lacks across to it |
| `internalize` | `-no-ack` | false | do not acknowledge the envelope |
| `history` | `-at URL` | `history_host` | the host's terms route |
| `history` | `-after CURSOR` | none | the page after `<created>:<txid>` |
| `history` | `-all` | false | every page from there on |
| `history` | `-max-sats N` | 1000 | the most one question is paid |
| `history` | `-budget N` | 16000 | the most the command pays in all |
| `payee key` | `-out FILE` | none | where `BBOX_PAYEE_KEY` is written (`-` for standard output) |
| `payee settle` | `-in-flight N` | 16 | payments broadcast and not yet mined at once |

`bbox <command> -h` prints a command's own help. [usage.md](usage.md) has
each command in full.

## Environment

| Variable | Meaning |
| --- | --- |
| `BBOX_<KEY>` | any key above, in upper case |
| `BBOX_HOME` | the home, and where the config file is looked for first |
| `XDG_CONFIG_HOME` | `$XDG_CONFIG_HOME/bbox/config` when `BBOX_HOME` is unset |

## The container image

`ghcr.io/lightwebinc/bbox` is the static binary on a distroless nonroot
base, with no `ENV` defaults. The home is `/home/nonroot/.bbox`; mount a
named volume there and pass settings as `BBOX_<KEY>` variables:

```console
$ docker run --rm -v bbox-home:/home/nonroot/.bbox \
    -e BBOX_NETWORK=main ghcr.io/lightwebinc/bbox doctor
```

The image carries that directory, empty, owned by nonroot and mode 0700,
so a new named volume starts with the same owner and mode. A bind mount
keeps the host directory's owner: make it writable by uid 65532, or run
with `--user`. The licenses are in `/usr/share/doc/bbox/`.

`ghcr.io/lightwebinc/bbox-devchain` is the local regtest chain for the
development sandbox (port 8080, journal in `/var/lib/devchain`). Coinbase
from it: only on a regtest chain you run (development and tests); nothing
it mines is money.

## The host

A host is configured by `BBOX_OFFICES`, `BBOX_STATE_DIR` and the variables
for its terms route, prices, payee and payment acceptance, plus the
reference overlay host's own `OVERLAY_*` settings. They are all in
[host.md](host.md#configuration).
