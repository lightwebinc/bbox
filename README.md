# bbox

[![CI](https://github.com/lightwebinc/bbox/actions/workflows/ci.yml/badge.svg)](https://github.com/lightwebinc/bbox/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/lightwebinc/bbox)](https://github.com/lightwebinc/bbox/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/lightwebinc/bbox.svg)](https://pkg.go.dev/github.com/lightwebinc/bbox)
[![Go version](https://img.shields.io/github/go-mod/go-version/lightwebinc/bbox)](go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

> [!WARNING]
> **Experimental software.** bbox is part of [bstack](https://github.com/lightwebinc/bstack),
> the applications and patterns built on [BSV Layered Multicast](https://github.com/lightwebinc/bsv-multicast).
> It is published to be built on and improved. Interfaces, formats and behavior may change
> rapidly between releases; pin an exact version.

A message box on Bitcoin, replicated by overlay hosts. A sender addresses an
encrypted envelope to a recipient's identity key and a box; every host that
carries the office holds it; the recipient lists its box from any host,
verifies every envelope against block headers it checks itself, and
acknowledges with a signed receipt that every host applies. A payment can
travel inside an envelope, unbroadcast, for the recipient to internalize.

## Install

```console
$ go install github.com/lightwebinc/bbox/cmd/bbox@latest
```

Or download a release tarball (linux and darwin, amd64 and arm64, with
`SHA256SUMS`) from [Releases](https://github.com/lightwebinc/bbox/releases),
or build the container image from this repository (`make docker-build`):
`ghcr.io/lightwebinc/bbox` (the command), `ghcr.io/lightwebinc/bbox-host`
(an overlay host with the bbox module) and `ghcr.io/lightwebinc/bbox-devchain`
(a local regtest chain for development).

## On mainnet, in short

```console
$ bbox -network main init
created /home/user/.bbox
identity     02c6...9a1e
fund address 1Kq3...Vb7 (main)
# send about 10,000 satoshis from your own wallet to the fund address,
# then import that payment by its txid once mined (or its BEEF at once):
$ bbox fund -txid 5e1f...77ab
imported 1 of 1 output(s) paying 1Kq3...Vb7, 10000 sat, mined at height 970041; pool 1 output(s), 10000 sat
$ bbox send 03a1...77c2 -m 'the invoice is attached'
$ bbox list                                  # as the recipient
```

No node is needed. Every command reads the overlay hosts that carry your
office from `~/.bbox/config` ([docs/configuration.md](docs/configuration.md));
the chain services default to WhatsOnChain (headers, transactions, proofs,
spends) and GorillaPool's public arcade (broadcast), and your own node or
header service is the better option. Testnet works the same with
`-network test`. An envelope costs its sender about 9 satoshis (its share
of a 32-output funding tree at the network's rate of 100 satoshis per 1000
bytes); reading is free.

## How it works

- **Envelopes are never mined.** Each rides an unmined carrier transaction
  that spends one output of the sender's mined funding tree, so it verifies
  by SPV, it is bound to the sender's key, and the sender can retract it by
  spending that output.
- **Hosts are replicas, not authorities.** A host can withhold an envelope; it
  cannot forge or alter one. Reading from several hosts shows a host that
  withholds.
- **Content is a BRC-169 envelope**, encrypted to the recipient under BRC-78.
  Who wrote to whom, and when, is public; what was written is not.
- **Two ways to reach the hosts.** On the multicast plane a publisher submits
  each object once and every subscribed host receives it; without it, the
  publisher submits to each host itself.
- **Reading is free.** The base questions are free on every conforming host;
  a host may charge for history, never for the inbox, through a BRC-105 402
  that the client pays and the host's payee settles.
- **Retraction is a mined sweep.** A sender takes an envelope back by
  spending its funding output; honest hosts stop answering it. It is not
  erasure.

[docs/architecture.md](docs/architecture.md) has the components and the
data flow.

## Documentation

| If you want to | Read |
| --- | --- |
| Get started on mainnet or testnet, or try everything on one machine in a regtest sandbox | [QUICKSTART.md](QUICKSTART.md) |
| Use it day to day: sending, reading, paying, retracting, troubleshooting | [docs/user-guide.md](docs/user-guide.md) |
| Copy a command for a common task | [docs/examples.md](docs/examples.md) |
| Look up a command, a flag or an exit code | [docs/usage.md](docs/usage.md) |
| Set every flag, environment variable and config key | [docs/configuration.md](docs/configuration.md) |
| See the components, the data flow and the trust model | [docs/architecture.md](docs/architecture.md) |
| Implement a compatible sender, reader or host | [docs/spec.md](docs/spec.md) (the specification) |
| Know what cannot change once published | [docs/frozen.md](docs/frozen.md) |
| Know the defaults and caps, and why each is set where it is | [docs/limits.md](docs/limits.md) |
| Run a host: the module, its configuration, the terms route, settling, backups, metrics | [docs/host.md](docs/host.md) |
| Send and read bbox envelopes from another Go program | [docs/packages.md](docs/packages.md) |

## Build and test

```console
$ (cd host && npm ci)   # once
$ make verify           # gofmt, vet, dependency set, typecheck, vectors, tests
$ make bbox             # the command, bin/bbox
$ make module           # the host module, host/bundle/bbox-module.js
$ make docker-build     # the bbox and bbox-devchain images, locally
```

Go 1.27.1 or later and Node 24; `NODE=/path/to/node` selects a Node 24
binary that is not on `PATH`. The golden vectors in `testdata/vectors` are
generated by the Go codec (`boxrec`), and `testdata/ts` by the TypeScript
codec in `host/`; each side's tests read the other's, byte for byte. Two
direct Go dependencies, asserted by `make verify`:
[go-sdk](https://github.com/bsv-blockchain/go-sdk) at exactly v1.7.1 and
[bcommon](https://github.com/lightwebinc/bcommon), whose TypeScript package
at the same version is vendored under `host/vendor`.

## License

Apache 2.0; see [LICENSE](LICENSE), [NOTICE](NOTICE) and
[LICENSE-THIRD-PARTY](LICENSE-THIRD-PARTY).
