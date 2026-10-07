# Examples

Commands for the common tasks, on a real network, with no node of your
own. They assume a config file naming your overlay hosts
([configuration.md](configuration.md)); the header source, the chain view
and the settlement leg default to WhatsOnChain and GorillaPool's public
arcade; every
key, txid, office and hostname here is illustrative. Exit codes: 0 done, 1
refused, 2 usage or transport error, 3 incomplete (the hosts disagree).

## Set up a home on mainnet

```console
$ bbox -network main init
created /home/user/.bbox
identity     02c6...9a1e
fund address 1Kq3...Vb7 (main)
```

Send coin from your own wallet to the fund address (10,000 satoshis is
about a thousand envelopes), wait for one confirmation, then import it:

```console
$ bbox fund -txid 5e1f...77ab
imported 1 of 1 output(s) paying 1Kq3...Vb7, 10000 sat, mined at height 970041; pool 1 output(s), 10000 sat
```

Or import it at once from the BEEF your wallet hands over:

```console
$ bbox fund -beef payment.beef
imported 1 of 1 output(s) paying 1Kq3...Vb7, 10000 sat, not mined yet: held until it mines, when a later command collects its proof; pool 1 output(s), 10000 sat
$ bbox fund -beef - < payment.hex       # from standard input, hex or binary
```

A transaction that pays someone else is refused, and adds nothing:

```console
$ bbox fund -txid 5285...5be6
bbox: payment 5285...5be6 pays nothing to the fund address 1Kq3...Vb7
```

Check everything at once; `doctor` reads only:

```console
$ bbox doctor
```

## Set up a home on testnet

```console
$ bbox -network test init
fund address mv4r...Q8x (test)
$ bbox -network test fund -txid 9c0a...41de
```

Keep testnet in its own home so mainnet and testnet coin never mix:
`bbox -home ~/.bbox-test -network test ...`, or `BBOX_HOME=~/.bbox-test`
with `network = test` in `~/.bbox-test/config`.

## Check a header source

```console
$ bbox -network main -header-url woc:main doctor | grep headers
headers     woc:main tip 970055
```

## Use your own node and services

Each chain service is one setting; `doctor` shows what is in force:

```console
$ bbox -chain asset:https://node.example.com doctor | grep chain
chain       asset:https://node.example.com
$ bbox -header-url bhs:https://headers.example.com doctor | grep headers   # header_token in the config
$ bbox -settle arcade:https://arcade.example.com doctor | grep settle
$ bbox -fee-source arc doctor | grep fee        # the broadcaster's published rate
fee         100/1000 satoshis/bytes, floor 250 sat (the broadcaster's published policy)
```

## Create an office to receive in

```console
$ bbox office new support
office support_qzxkvbmwtr
topic  tm_bbox_support_qzxkvbmwtr
host   BBOX_OFFICES=support_qzxkvbmwtr
```

Give senders your identity key and the office; give each host operator the
`host` line.

## Send a message

```console
$ bbox send 03a1...77c2 -m 'the invoice is attached'
$ bbox send 03a1...77c2 invoices -file invoice.txt        # to the invoices box
$ echo 'from a script' | bbox send 03a1...77c2            # body from standard input
```

With a reference to a larger file, by its SHA-256 and length:

```console
$ bbox send 03a1...77c2 -m 'the report' \
    -ref https://files.example.com/report.pdf,$(sha256sum report.pdf | cut -c1-64),$(stat -c%s report.pdf)
```

## Send a payment inside a message

```console
$ bbox send 03a1...77c2 -m 'for the coffee' -pay 5000
```

The recipient takes it with `bbox internalize <txid>` within a day (the
default `-expires` with a payment).

## Read and acknowledge

```console
$ bbox list                              # the inbox, from every host, compared
$ bbox list -box invoices                # one box
$ bbox list -from 02c6...9a1e            # one sender
$ bbox read                              # verify, decrypt and print every open envelope
$ bbox ack -all                          # one receipt for everything read
```

## Take a payment

```console
$ bbox internalize 3f9a...c410
internalized 5000 sat from 02c6...9a1e: payment 77d2...e1a0, pool 3 output(s), 105000 sat
```

## Copy an envelope to a host that lacks it

```console
$ bbox list -fill
```

## Take back an envelope you sent

```console
$ bbox drop 8b0e...12fa
```

## Ask a priced question

```console
$ bbox terms                              # what the host charges
$ bbox history -max-sats 10               # one page, at most 10 sat
$ bbox history -all -budget 100           # every page, at most 100 sat in all
```

## Settle what a host was paid, as its payee

```console
$ bbox -home /srv/payee payee key -out payee.env        # once: BBOX_PAYEE_KEY for the host
$ bbox -home /srv/payee payee settle /var/lib/bbox/payments.jsonl
```

## Use the container image

```console
$ docker run --rm -v bbox-home:/home/nonroot/.bbox \
    -e BBOX_NETWORK=main ghcr.io/lightwebinc/bbox init
```

## Try everything with no coin

The development sandbox in [QUICKSTART.md](../QUICKSTART.md) runs a
private regtest chain, two hosts and three identities on one machine. Its
homes are funded with `bbox fund` (no `-txid` or `-beef`), which mines
coinbase.
Coinbase: only on a regtest chain you run (development and tests).
