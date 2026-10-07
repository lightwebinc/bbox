# Configuration

Every setting of the `bbox` command, the container images and the host
module. Settings come from, highest first: the global flags, `BBOX_<KEY>`
in the environment (the key in upper case: `header_url` is
`BBOX_HEADER_URL`), the config file, and the built-in defaults.

The deployment addresses have no default, on purpose: the overlay hosts,
the facade, the header source, the node, the settlement leg and the host a
priced question goes to. A default there would send your envelopes or your
questions to a server nobody configured, so a command that needs one and
finds none exits 2 naming the key.

## The config file

The file is `-config PATH`, else `$BBOX_HOME/config`, else
`$XDG_CONFIG_HOME/bbox/config`, else `~/.bbox/config`. A missing file is
fine. The grammar is one `key = value` per line, `#` comments, blank lines
ignored; a key it does not define, a key given twice, or a line without
`=` is an error (exit 2), never ignored.

### Mainnet

```text
# ~/.bbox/config
network      = main
office       = support_qzxkvbmwtr
hosts        = https://host-a.example.com,https://host-b.example.com
facade       = https://host-a.example.com
header_url   = woc:main
asset        = https://node.example.com
settle       = arcade:https://arc.example.com/v1
history_host = https://terms.host-a.example.com
```

### Testnet

```text
network    = test
office     = support_qzxkvbmwtr
hosts      = https://host-a.example.com,https://host-b.example.com
facade     = https://host-a.example.com
header_url = woc:test
asset      = https://testnet-node.example.com
settle     = arcade:https://testnet-arc.example.com/v1
```

The fund address is then a testnet address, and `woc:test` must match
`network = test` (a `woc:` source for another network is refused).

### Without the plane

Submit every object to each host yourself:

```text
mode   = unicast
hosts  = https://host-a.example.com,https://host-b.example.com
quorum = all
```

### Development sandbox (regtest)

The sandbox in [QUICKSTART.md](../QUICKSTART.md) sets these as `BBOX_*`
variables in its compose file: `network = regtest`, and `asset`, `rpc`,
`settle = rpc:...` and `header_url` all pointing at the local
`bbox-devchain`. `rpc` is used only to mine coinbase there (coinbase: only
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
| `asset` | none | a Teranode node's asset API base URL (paths under `/api/v1/`): proofs, the chain tip, blocks, raw transactions. Needed by `fund -txid` and every command that publishes or takes a payment |
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
| `rpc` | none | the node's JSON-RPC URL, used only by `fund` without `-txid` (coinbase: only on a regtest chain you run, for development and tests) |
| `rpc_pass`, `rpc_user` | `bitcoin` | the node's basic auth, also for `settle = rpc:` |
| `settle` | none | the settlement leg for what is mined (funding trees, sweeps, a payment a recipient or a payee takes): `tcp:<host:port>` (bare EF to an ingress), `rpc:<url>` (a node's `sendrawtransaction`) or `arcade:<url>` (any ARC-compatible API; bbox posts to `<url>/tx`) |
| `timeout` | `15s` | bounds each request; `1s` to `5m` |
| `tree_count` | `32` | outputs of a new funding tree, 1 to 1000 |

`network`, `mode` and `box` accept only the values listed; numbers and
durations must parse; anything else exits 2 naming the key. Every cap is in
[limits.md](limits.md).

## Header sources

Every proof is checked against the headers of one source, so `header_url`
is the root of trust for every envelope you read.

| `header_url` | Source |
| --- | --- |
| `woc:main`, `woc:test` | the public WhatsOnChain API; each header is hashed here and must carry the work its bits claim, and must match `network` |
| `chaintracks:https://host/v2` | a chaintracks v2 service, public or your own |
| `https://bridge.example.com/v1` | an overlay bridge's native `/v1/root/<height>` and `/v1/tip` (also what `bbox-devchain` serves) |

## Global flags

Before the command: `bbox [global flags] <command> [flags]`.

| Flag | Key | Meaning |
| --- | --- | --- |
| `-config PATH` | none | the config file |
| `-home DIR` | `home` | the identity's home |
| `-hosts URLS` | `hosts` | overlay hosts, comma separated |
| `-header-url SOURCE` | `header_url` | header source |
| `-mode MODE` | `mode` | `plane` or `unicast` |
| `-network NET` | `network` | `main`, `test` or `regtest` |
| `-office OFFICE` | `office` | the office identifier |
| `-quorum Q` | `quorum` | `all`, `majority`, `one` or N |
| `-timeout DUR` | `timeout` | per request |
| `-v` | none | verbose; also accepted after the command |
| `-version` | none | print the version and exit |

## Command flags

| Command | Flag | Default | Meaning |
| --- | --- | --- | --- |
| `fund` | `-txid TXID` | none | import a mined payment you sent from your own wallet to the fund address |
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
    -e BBOX_NETWORK=main -e BBOX_HEADER_URL=woc:main \
    ghcr.io/lightwebinc/bbox doctor
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
