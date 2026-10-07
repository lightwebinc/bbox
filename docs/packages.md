# Go packages

bbox is a command and a host module. Three of its Go packages are also
public, so that another application can send and read bbox envelopes by
bbox's own code instead of a copy of it: the envelope and its plaintext, the
sending, and the listing. Everything else stays under `internal/` and cannot
be imported.

| Package | What it is |
| --- | --- |
| `github.com/lightwebinc/bbox/boxrec` | The byte-level contract ([spec.md](spec.md)): the envelope and receipt records, the BRC-169 content, sealing and opening it, the plaintext and its rules, the payment rules, the name grammars and the bounds. No network. |
| `github.com/lightwebinc/bbox/reader` | A client that asks overlay hosts the `ls_bbox` questions, verifies every answer against the caller's own headers, and applies the recipient's checks to what it decrypts. |
| `github.com/lightwebinc/bbox/send` | A publisher over one identity's home: it seals envelopes, pays inside them, acknowledges them with receipts and retracts them, persisting each before it is published. |

Each package has an example (`go doc -all`, or the `example_test.go`
beside it).

## Supported surface

These are what an importer may rely on. The packages export more than this,
because bbox's own command and host tests use them; anything not listed here
may change at any tag.

**`boxrec`**

- The envelope record: `Envelope` (`Encode`, `Validate`), `DecodeEnvelope`.
- The content: `SealEnvelope` (encrypt the plaintext to the recipient and
  sign the BRC-169 envelope through the sender's wallet, a `Sealer`),
  `CheckContent` (every content rule, the signature included), `Content`,
  and `Open` (decrypt through the recipient's wallet, a `Decrypter`).
- The plaintext: `Object`, `Member`, `Int`, `ParseJSON`, `Canonical`,
  `EncodePlaintext` (canonical, and held to the recipient's rules before it
  is returned), `ParsePlaintext`, `Plaintext` with `Plaintext.Extension`,
  `Ref`, `Payment`, `PaymentOutput`, and the member names `PlainBody`,
  `PlainPayment`, `PlainRefs`.
- An application's own plaintext member: any member the contract does not
  define is carried, covered by the encryption, and ignored by bbox (spec
  section 4.5). A sender sets it on the `Object` it passes to
  `EncodePlaintext`; a recipient reads it with `Plaintext.Extension`, which
  answers it when it is an object, with every member the sender wrote,
  unknown ones included. bbox gives such a member no meaning and checks
  nothing in it: its rules are the application's.
- The receipt record: `Receipt`, `DecodeReceipt`.
- Names and keys: `CheckOffice`, `CheckBox`, `CheckIdentity`, `Topic`,
  `RFC3339`.
- The payment rules: `CheckPayment`, `PaymentMinLife`, `PaymentMargin`,
  `PaymentProtocol`.
- The registry values (`ProtocolName`, the key ids, `TopicPrefix`,
  `LookupService`), the question members (`Q*`), the bounds (`Max*`,
  `Min*`, `SuffixLen`), the `Err*` values and `Reason`, the fixed label a
  refusal is counted and shown by.

**`reader`**

- `Client`, with `Client.List` (one question to every host, every page,
  the answers compared), `Client.Pages`, `Client.Verify` and `Client.Ask`.
- `BoxQuery` (one box, or every box) and `FromQuery` (one sender's
  envelopes): the questions a recipient asks of its office. A sender reads
  its own envelopes to a recipient with `FromQuery` and its own key.
- `Listing` (with `Agree`, `Answered`, `Find`, `Missing`, `Refusals`,
  `Refused`), `HostAnswer`, `Refusal`, `Item` (with `Envelope` and
  `Created`), `Hash`.
- `Open`: the recipient's checks of spec section 4.7, in order, on an item
  that verified, returning a `Message`. `Message.Err` refuses the message;
  `Message.PayErr` refuses only its payment. `Message.Payment` is set only
  for a payment that passed all nine checks, and `Message.Internalize`
  makes the arguments the recipient's wallet's `internalizeAction` takes
  for it. `Message.Paid` is what its listed outputs hold.
- `Recipient` (the wallet methods `Open` needs), `ChainSource`, `MaxBEEF`,
  `ErrHost`.

An envelope's `From` and `To` (`Item.Envelope`) are the record's, and the
record is bound to the carrier the sender's key signed and to the content
whose BRC-78 header names the same two keys (spec sections 4.3 and 8.1). An
application that derives a value from the two parties (a conversation id,
for one) derives it from those, never from anything in the plaintext.

**`send`**

- `Engine`: `New`, `Engine.Start` (finish what a previous run left),
  `Engine.Payment` (a BRC-29 payment for an envelope, never broadcast by
  the sender), `Engine.Envelope`, `Engine.Receipt`, `Engine.Retract`,
  `Engine.FinishSweep`, `Engine.Close`.
- `Letter`, `Pay` (with `Pay.Abort`), `Legs`, `Options`, `Profile`,
  `Retries`, `SweepRefusedError`, `ErrSweepInFlight`.
- The home: `LockHome`, `ErrLocked`, `LoadState`, and the types the
  engine reads and returns, `State`, `Sent`, `Receipt`, `Sweep`, and
  `Unicast` (a host set, for `Legs.Hosts` and `Legs.Direct`).

A home is a directory holding one identity's wallet, opened with bcommon's
`bwallet` under `send.Profile`, and its state. Every object is persisted in
the state, with the funding output it spends marked used, before it is
published; a run that stops part way publishes the same bytes at the next
`Start`. An importer therefore:

- gives bbox a home of its own: the pool under `send.Profile` funds bbox's
  carriers and nothing else, since two tools drawing on one set of outputs
  double-spend each other. The identity may be the application's own
  (bcommon's `bwallet.OpenIdentity` opens an identity file with another
  pool);
- takes the lock (`LockHome`) before it opens the wallet, and holds it
  while the engine runs: one process at a time per home;
- needs no node: `Legs.Chain` is any `nodeapi.Chain` (bcommon's
  `nodeapi.ParseChain("woc:main", ...)`, for one), `Legs.Settler` an
  arcade (`publish.ParseSettler("arcade:main", ...)`) and `Legs.Headers`
  any chain tracker; nothing asks for a node's asset API or RPC;
- calls `Start` before the first object, and `Close` when done;
- seals a payment in the same envelope it was built for: a `Pay` that is
  not sent is given back with `Pay.Abort`. A recipient takes a payment
  only from an envelope whose `Expires` is at least `PaymentMinLife` after
  its `Created` (spec section 10); `Engine.Envelope` does not check it, so
  the caller sets it.

## What the packages do not do

- They resolve no recipient. The office and box an envelope goes to are
  the caller's, from wherever the recipient names them.
- `reader` keeps no state and remembers nothing it verified. What it
  returns verified against the caller's headers; it may not be everything:
  hosts that all withhold an envelope answer without it, and a comparison
  across hosts is the only evidence of a withholding one (`Listing.Missing`).
  An empty answer proves nothing.
- `reader` does not internalize. The caller's wallet does, with the
  arguments `Message.Internalize` makes, while `now < expires - 3600`, and
  the caller acknowledges the envelope afterwards with a receipt
  (`Engine.Receipt`).
- Nothing here renders. A body, a reference's locator and every member of
  an application's own member are text someone else wrote; they pass
  through a sanitizer before display (spec section 11).

## Versions

The packages are versioned by the repository's tags, as plain semantic
versions (`v0.1.0`). An importer pins a tag in its `go.mod`; there is no
other release channel, and an untagged commit carries no promise. While the
major version is 0, a minor version may change the supported surface. The
bytes the packages read and write are the contract of [spec.md](spec.md)
and [frozen.md](frozen.md), held by the golden vectors under
`testdata/vectors`; a tag never changes those.

bbox pins go-sdk at exactly v1.7.1 and bcommon at the version in its
`go.mod`. Go's minimal version selection gives an importer the higher of
its own pin and bbox's, so an importer that needs the same bytes pins the
same versions and asserts them, as bbox's `make deps-check` does.

## Importing while the repository is private

While the repository is private, the public Go proxy and checksum database
cannot see it, so the `go` command has to be told to go to the repository directly,
and needs credentials for it:

```console
$ go env -w GOPRIVATE='github.com/lightwebinc/*'
$ git config --global url."ssh://git@github.com/lightwebinc/".insteadOf "https://github.com/lightwebinc/"
$ go get github.com/lightwebinc/bbox@v0.1.0
```

`GOPRIVATE` turns off the proxy and the checksum database for the matching
paths. The `insteadOf` line makes git fetch over SSH with the key the
machine already uses; an HTTPS credential helper or a token in `~/.netrc`
with read access to the repository does the same. A build without
credentials (a CI job with none, for instance) can instead resolve the
module from a copy of its tagged archive served as a file proxy
(`GOPROXY=file://...`), which `go.sum` still verifies against the tag.
