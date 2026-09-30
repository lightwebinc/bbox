# bbox: the message box contract

bbox is a message box whose store-and-forward server is replaced by overlay
hosts fed by the BEEF object plane. A sender addresses an envelope to a
recipient's identity key and a box name, encrypted to the recipient; every
host that carries the envelope's office holds it; the recipient lists its box
from any host, or from several and compares, and acknowledges by publishing a
receipt that every host applies. It keeps the semantics of a BRC-33 message
box and carries a BRC-169 section 7.2 envelope inside every record, so a
payment can travel inside an envelope, unbroadcast, as BRC-169 section 6.1
describes, encrypted with the rest of the message (section 10). Where it
departs from BRC-169, section 14 says so. A host is an ordinary overlay host running the
`tm_bbox_<name>_<suffix>` topic manager and the `ls_bbox` lookup service.

This document is normative. MUST, MUST NOT, SHOULD and MAY are used as in
RFC 2119. Every byte-level and name-level choice that cannot change once a
record is on a public host is listed in [frozen.md](frozen.md). The golden
vectors in `testdata/vectors` are part of the contract (section 16).

## 1. Objects

| Object | On chain | On the plane | What it is |
| --- | --- | --- | --- |
| **Envelope record** `E` | never | inside a carrier | canonical CBOR: the addressing, and the BRC-169 envelope as content (sections 3 and 4) |
| **Receipt record** `R` | never | inside a carrier | canonical CBOR: a recipient's acknowledgement of up to 64 envelopes (section 5) |
| **Carrier** `K` | never mined | a BEEF object | one unmined transaction whose one output holds `E` or `R` (section 6.2) |
| **Commitment** `C` | no | no | `C = txid(K)`, 32 bytes, hash byte order |
| **Funding tree** `F` | mined | inside each carrier's BEEF | one transaction of tagged outputs; each carrier spends one |
| **Sweep** | mined | a BEEF object | a spend of funding outputs that retracts the carriers they funded (section 6.4) |

Envelopes are events, not state, so nothing per envelope is mined: a sender
pays one mined transaction per funding tree, and every envelope and receipt
rides an unmined carrier.

```text
 sender ── carrier(E) ──▶ plane ──▶ host A ─┐  ls_bbox: {office, to, box}
   funding tree F (mined) ──▶ out k ──▶ K     ├─▶ recipient lists, verifies,
                                  └──▶ host B ─┘    decrypts, internalizes
 recipient ── carrier(R: acks C1..Cn) ──▶ plane ──▶ hosts mark C1..Cn acknowledged
 sender ── sweep of F (mined) ──▶ plane ──▶ hosts drop every carrier F funded
```

An **office** is the rendezvous: one topic, `tm_bbox_<name>_<suffix>`, that a
set of hosts carries and a set of recipients reads (section 7.1). A recipient
tells its senders which office to use; any number of senders and recipients
share one office, and a box is an index inside it, not a chain: no
per-recipient serialization point exists.

## 2. Conventions

- **CBOR.** Every record is one CBOR map in the core deterministic encoding
  of RFC 8949 section 4.2.1, written and read by bcommon's `cbor` package,
  which enforces it on encode and on decode. The subset is unsigned and
  negative integers, byte strings, UTF-8 text strings, arrays, maps, `false`,
  `true` and `null`; tags, floats, indefinite lengths and nesting deeper than
  16 are refused. One value has exactly one encoding a reader accepts.
- **Record keys.** A record's top-level keys are unsigned integers, at most 64
  of them. A key this version defines is checked as its table says. A key
  above the last defined one is unknown: a reader MUST preserve it verbatim
  when it re-encodes and MUST otherwise ignore it.
- **Hashes.** SHA-256 unless stated. A commitment inside a record is in hash
  byte order (the order SHA-256d produces). A txid in JSON (a lookup
  question, an answer, a cursor) is in display order, the byte-reversed hex
  the engine and every explorer print. Both are lowercase hex.
- **Keys.** An identity key is a compressed secp256k1 point: 33 bytes, prefix
  `0x02` or `0x03`, an x-coordinate below the field prime, and a point on the
  curve (bcommon `guard.ParsePubKey`). A reader MUST refuse any other byte
  string, including an x-coordinate at or above the prime that some parsers
  reduce and accept (the `envelope-to-alias` vector), because that would give
  one identity two names. In JSON a key is its 66 lowercase hex digits.
- **Signatures** a host checks are strict DER with a low S value. A wallet
  writes them that way; a high-S or loosely encoded copy is a different byte
  string for the same signature and is refused.
- **Time.** Times are Unix seconds (UTC) on the writer's clock, from 1 to
  253402300799 (`9999-12-31T23:59:59Z`, the last second RFC 3339 can write).
  They are claims. A host uses them only to decide what a free question
  answers (section 8.3), never to admit or refuse.

## 3. The envelope record

Magic `"bbe"` `0x01`. The payload of an envelope carrier's one output.

| Key | Field | Type and bounds | Rule |
| --- | --- | --- | --- |
| 0 | `magic` | bytes(4) | `62 62 65 01` (`"bb"`, `'e'` for envelope, version 1). Always the first key |
| 1 | `office` | text; the office identifier grammar (section 7.1) | the full `<name>_<suffix>`; MUST equal the topic's office |
| 2 | `to` | bytes; the key rule | the recipient's identity key. Public: the index key |
| 3 | `box` | text; the box name grammar (below) | the recipient's box, as BRC-33's `messageBox` |
| 4 | `from` | bytes; the key rule | the sender's identity key. The carrier's lock and funding are derived from it (section 8.1), so it is bound, not claimed |
| 5 | `created` | uint, 1 to 253402300799 | when the sender made the envelope. A claim |
| 6 | `expires` | uint, 0 to 253402300799 | after this second no free question answers the envelope; 0 means no expiry of the sender's own |
| 7 | `content` | bytes, 1 to 16384 | the BRC-169 envelope, as carried (section 4) |

Keys 0 to 7 are always present. Keys 8 and above are unknown to this version.
The encoded record is at most 20480 bytes (the content bound plus 4 KiB).

A **box name** is 1 to 50 bytes of lowercase ASCII letters, digits and single
underscores, starting with a letter and ending with a letter or a digit
(`inbox`, `payment_inbox`, `files2`). A box is an index key, never a topic,
so box names may hold digits, which BRC-87 topic names do not.

There is no salt and no reference list in the record. The record is public,
so a salt would hide nothing, and a host answers at most one carrier per
funding output (section 8.2), so a salt would distinguish nothing a host
uses; a reference to larger content travels inside the encrypted payload
(section 4.5), where it does not disclose what is attached.

### 3.1 The refusal order

A record that breaks several rules is refused for the first, so that every
implementation reports the same reason:

1. `too-large`: the encoded length is over the record's bound;
2. `cbor`: the bytes are not exactly one canonical item of the subset, or the
   item is not a map;
3. `too-large`: the map has more than 64 entries;
4. `key-type`: a key is not an unsigned integer;
5. `magic`: key 0 is absent, is not a byte string, or is not the magic;
6. each defined key in ascending order: `missing` if it is absent, then
   `type` if it has the wrong CBOR type, then the key's own rule: `office`
   for key 1, `identity` for keys 2 and 4, `box` for key 3, and `range` for
   every other bound in the table;
7. across fields: `expires` (it is 0 or greater than `created`).

The receipt record (section 5) follows the same steps with its own keys and
has no step 7.

## 4. The content: a BRC-169 envelope

Key 7 holds a BRC-169 section 7.2 envelope: metadata in the clear, which the
record repeats and binds, and a payload encrypted to the recipient. Hosts
check it so that every replica enforces what BRC-169 asks of a messagebox ("A
messagebox MUST reject an envelope whose signature does not verify"); the
carrier's own field signature, which covers the whole record, content
included, is what binds the envelope to its sender on chain (section 8.1).

### 4.1 The JSON subset and its canonical form

The content is exactly the RFC 8785 (JCS) serialization of one JSON object
in this subset, and nothing else:

- I-JSON (RFC 7493): valid UTF-8, no byte order mark, member names unique
  within an object, no lone surrogate;
- numbers are integers from -(2^53 - 1) to 2^53 - 1, written without a
  fraction, an exponent, a leading zero or a minus zero;
- nesting at most 16 deep.

With integers only, JCS writes every number as plain decimal, so Go and
JavaScript reach the same bytes without floating-point formatting.

A host decides membership of the subset on the parsed value, so that a host
whose parser is more permissive (JavaScript's `JSON.parse` accepts duplicate
names, lone surrogates and any number) reaches the same verdict. In order:

1. the bytes are valid UTF-8 with no byte order mark, and parse as one JSON
   value that is an object, with nothing after it but white space;
2. no object repeats a member name;
3. no string, member names included, holds a lone surrogate, written raw or
   escaped;
4. every number is an integer from -(2^53 - 1) to 2^53 - 1 (in JavaScript,
   `Number.isSafeInteger` on the parsed value; the spelling is then pinned
   by step 6);
5. nesting, counting the top-level object as depth 1, is at most 16;
6. the bytes equal the RFC 8785 serialization of the parsed value.

Step 6 refuses any other spelling of the same object (white space, member
order, an escape JCS does not write, such as
`\u0041` for `A`, a fraction or an exponent). One envelope therefore has one
encoding. BRC-169 canonicalizes so that a messagebox may re-serialize an
envelope; here nothing re-serializes (the carrier binds the exact bytes), so
the carried bytes are required to be the canonical ones. `json-v1.json`
pins each step case by case.

### 4.2 Members

| Member | Rule |
| --- | --- |
| `metanetHandles` | the string `"1.0"` |
| `recipient` | an object whose `identityKey` is the record's `to` in lowercase hex. `handle`, `tag` and `domain`, when present, are strings (BRC-169 claims; a host does not resolve them) |
| `sender` | an object whose `identityKey` is the record's `from` in lowercase hex. `handle` and `domain`, when present, are strings, and are claims a recipient client resolves before it shows the sender as a named identity (BRC-169 section 7.2 rule 3) |
| `created` | the record's `created` written exactly as `YYYY-MM-DDTHH:MM:SSZ` |
| `quoteId` | optional; a string when present |
| `payment` | `null`. A payment rides inside the encrypted payload (section 10) |
| `content` | the BRC-78 message (section 4.3) in standard base64 with padding (RFC 4648 section 4) |
| `signature` | lowercase hex of the signature (section 4.4) |

Any other member is allowed, is covered by the signature, and is ignored by a
host. The recipient's `identityKey` is a member BRC-169's `recipient` object
does not name: a bbox envelope is addressed to a key, and a handle, when the
sender used one, rides beside it.

### 4.3 The encrypted payload

`content` decodes to a BRC-78 message from `from` to `to`: the version bytes
`42 42 10 33`, the sender's identity key, the recipient's identity key, a
32-byte key id drawn at random for this envelope, then the AES-256-GCM
ciphertext with its 32-byte IV prepended and its 16-byte tag appended (BRC-2
under BRC-42 derivation, protocol `[2, "message encryption"]`, key id the
base64 of the random key id). The 32-byte IV and the appended tag are BRC-2's
symmetric form as go-sdk implements it. A host checks only the header: at least 150
bytes (the header, the IV and the tag around an empty plaintext), the version,
and the two keys equal to the record's `from` and `to`. It cannot read the
rest; neither can anyone but the two parties (BRC-169 section 7.2 rule 1).

The version bytes are those go-sdk writes (`42 42 10 33`); BRC-78's hex
example prints them as `10334242`, which a host refuses
(`content-cipher-version-prose-order`). A wallet that exposes only the BRC-100
interface produces the same message: the ciphertext is its `encrypt` under
protocol `[2, "message encryption"]` and counterparty `to`, which the vectors
test by decrypting through the recipient's wallet.

### 4.4 The signature

`signature` is a strict low-S DER ECDSA signature over
`SHA-256(JCS(envelope without content and signature))`, BRC-169 section 7.2
rule 2's construction, made with the sender's key derived under
`[1, "bbox message"]`, key id `signature`, counterparty `anyone`: the key any
reader derives from `from` alone (section 6.1). This is a wallet's
`createSignature` with those arguments. BRC-169's text names the key in
`sender.identityKey` itself, but a wallet behind the BRC-100 interface
cannot sign with its identity key (every signature it makes is under a
derived key), so the signature key is the derived one, and a signature by the
identity key is refused (`content-signature-identity-key`). This is a
departure from BRC-169: a verifier that checks the signature against
`sender.identityKey` will not verify a bbox envelope.

### 4.5 The plaintext

What the recipient decrypts is the recipient client's contract, not a host's,
and a host never sees it. It is a JSON object in the subset of section 4.1,
serialized canonically. Its members:

| Member | Rule |
| --- | --- |
| `body` | optional; the message text, UTF-8. Rendered only through a sanitiser (section 11) |
| `payment` | optional; a payment made as BRC-169 section 6.1 describes: an object with `beef`, the payment transaction as Atomic BEEF (BRC-95) in standard base64; `derivationPrefix`; and `outputs`, an array with one object per output paid to the recipient, each with `outputIndex`, `derivationSuffix` and `satoshis`. The names follow BRC-169's worked example (appendix A.7); the per-output list is what BRC-29 requires ("the derivation suffix and output index for every output intended for the recipient") and what BRC-100 `internalizeAction` takes, with `from` as the sender's identity key |
| `refs` | optional; an array of references to content larger than the bound, each an object with `url` (an `https://` or BRC-26 `uhrp://` locator, a hint), `sha256` (64 lowercase hex, the SHA-256 of the bytes as fetched), `length` (their size) and, when the bytes are encrypted, `key` (64 lowercase hex, an AES-256 key drawn at random for that reference and never derived from an identity; the bytes are then an AES-256-GCM ciphertext with its 32-byte IV prepended and its tag appended, BRC-2's symmetric form) |

Unknown members are preserved and ignored. A reference is fetched only by the
recipient, checked against `length` and `sha256` before it is decrypted or
used, and never fetched merely because it is named.

### 4.6 The content rule order

A host applies the content rules after the carrier rules (section 8.1), in
this order, and refuses for the first broken:

1. `content-json`: the bytes are not the canonical serialization of a JSON
   object in the subset;
2. `content-shape`: a member of section 4.2 is missing, has the wrong type or
   value, or disagrees with the record (`identityKey`s, `created`,
   `payment` not `null`);
3. `content-cipher`: `content` is not standard padded base64, or its BRC-78
   header is short, has another version, or names another sender or
   recipient;
4. `content-signature`: `signature` is not lowercase hex of a strict low-S DER
   signature, or does not verify.

## 5. The receipt record

Magic `"bbr"` `0x01`. The payload of a receipt carrier's one output.

| Key | Field | Type and bounds | Rule |
| --- | --- | --- | --- |
| 0 | `magic` | bytes(4) | `62 62 72 01` (`"bb"`, `'r'` for receipt, version 1) |
| 1 | `office` | text; the office identifier grammar | MUST equal the topic's office |
| 2 | `by` | bytes; the key rule | the acknowledging recipient's identity key |
| 3 | `acks` | array of 1 to 64 bytes(32) | envelope commitments, hash byte order, strictly ascending by their bytes (so one set has one encoding). The whole array is one rule: `acks` |
| 4 | `created` | uint, 1 to 253402300799 | when the recipient acknowledged. A claim |

Keys 0 to 4 are always present; keys 5 and above are unknown to this version.
The encoded record is at most 4096 bytes.

A receipt acknowledges an envelope only when the envelope's `to` equals the
receipt's `by` (section 8.2): the receipt's carrier is derived from `by`
(section 8.1), so only the recipient can acknowledge its own envelopes, and a
receipt naming someone else's envelope is inert. BRC-33's "After
acknowledgment, the server deletes a message" becomes a status every replica
computes from the same signed fact.

## 6. Carriers, funding trees and derivations

Everything here is bcommon's, parameterised by the values in section 6.1.

### 6.1 Derivations and tags

| What | Value |
| --- | --- |
| BRC-43 protocol | `[1, "bbox message"]` (security level 1) |
| Carrier record output and its field signature (envelopes and receipts), funding-tree outputs | key id `envelope`, invoice `1-bbox message-envelope` |
| The BRC-169 envelope signature (section 4.4) | key id `signature`, invoice `1-bbox message-signature` |
| Embedded wallet funding key | key id `fund`, invoice `1-bbox message-fund`, counterparty `self` |
| Funding-output tag | `62 62 02` (`"bb"` `0x02`) |

Every derivation a reader or a host checks (`envelope` and `signature`) is
BRC-42 derivation with BRC-43's `anyone` counterparty: the owner's wallet derives with
`forSelf = true`, and a reader derives the same public key from the identity
key alone. That is the one setting under which a reader can recompute a
locking key and an embedded signature verifies under it. The funding key is
the exception: bcommon's embedded wallet derives it with counterparty `self`,
and no host rule reads it.

The protocol name is not the application's name because BRC-43 key
derivation refuses a protocol name under five characters: `[1, "bbox"]`
cannot derive (the `refusedProtocol` entry of `derivation-v1.json` records
the SDK's refusal).

### 6.2 The carrier

Built by bcommon `carrier.Mint` and checked by `carrier.Validate` plus the
rules of section 8.1:

- **exactly one input**, spending one output of the owner's mined funding
  tree, `nSequence = 0`, unlocked by exactly one push of the owner's
  signature (strict DER, low S, sighash byte `0x41`, `SIGHASH_ALL|FORKID`);
- **exactly one output**, a lock-before PushDrop of one field, the record,
  with the field signature, locked to `derive(owner, "envelope")`, carrying
  the whole value of the output it spends (fee zero);
- **`nLockTime = 4102444800`** (2100-01-01T00:00:00Z) with a non-final input,
  so it cannot be mined: a non-final transaction, as BRC-60 uses one, here
  with a far-future lock time so that the record stays off the chain while
  SPV still verifies it through its funding parent.

The owner is `from` for an envelope and `by` for a receipt. `C = txid(K)` is
what a receipt acknowledges and what hosts key the carrier by. The
unlocking-script rule keeps `C` the owner's: script verification does not
require a canonical unlocking script, so without it anyone could rewrite the
input's script (prefix a push and a drop, or negate S) and present the same
record under a new txid, which would make an acknowledgement miss and a
sweep's evidence ambiguous. The owner itself can make many carriers from one
funding output (they are never mined, so they never conflict on chain); a
host answers one of them (section 8.2).

### 6.3 Funding trees and publication

A funding tree is a mined transaction of equal outputs, each a PushDrop of
the one field `62 62 02` with no signature, locked to
`derive(owner, "envelope")`: `<key> OP_CHECKSIG <62 62 02> OP_DROP`. A carrier
spends only a **mined** tree's output, and its BEEF carries that tree with its
proof, so every carrier's ancestry is one mined parent and no carrier can be
stranded by a different version of its parent being mined. bcommon
`producer` runs the tree's lifecycle, including minting the next tree before
the current one runs out. The tree's size is the owner's choice and is
per-carrier overhead ([limits.md](limits.md)).

A funding tree is not published to an office: every carrier carries its own.
A sweep is published, once, after it mines (section 6.4).

A carrier is published once per destination, naming
`tm_bbox_<name>_<suffix>` as its first topic (section 9). A host answers a
submission with the outputs its topic manager admitted; an answer that admits
nothing is the same whether the host already held the transaction or refused
it, so a publisher cannot tell a duplicate from a refusal by the answer
alone. The publisher's own copy of the admission rules keeps it from building
a carrier a host would refuse, and a lookup shows whether a host holds it.

### 6.4 Retraction

An owner retracts carriers by sweeping a funding tree: one mined transaction
spending the tree's outputs, used and unused (bcommon `carrier.Sweep`, whose
output 0 is a funding-shaped tombstone, which is what makes a host admit it).
Once it is mined, every carrier that spent one of those outputs is a double
spend and can never be valid again. A sender uses it to take back envelopes a
recipient has not read; a recipient that sweeps its own used outputs retracts
its receipts, and with them its acknowledgements (section 8.2), so a
recipient that only wants unused value back spends only unused outputs.

Only a mined spend retracts. Retraction reaches honest hosts only: it is not
erasure, and a copy anyone kept stays a copy.

## 7. Offices and lookup

### 7.1 Office identifiers and topics

An office's topic is `tm_bbox_<name>_<suffix>`, and its **office
identifier** is `<name>_<suffix>`:

- `<name>` is the readable part: 1 to 31 bytes of lowercase ASCII letters and
  single underscores, starting and ending with a letter;
- `<suffix>` is exactly 10 lowercase ASCII letters, each drawn uniformly at
  random from `a` to `z` (26^10 values, about 2^47) by whoever creates the
  office, once, when it creates it. It never changes for the office's life,
  and it MUST NOT be chosen other than by a uniform random draw.

The identifier is 12 to 42 bytes and splits at its last underscore. The whole
topic is a BRC-87 topic manager name of at most 50 characters: the prefix
`tm_bbox_` takes 8, the separating underscore 1 and the suffix 10, which
leaves at most 31 for `<name>`. The `tm_bbox_` namespace belongs to bbox.

The identifier, never the readable part, is what every record's `office`,
every lookup question and every topic carries. A renderer that shows the
readable part SHOULD show the suffix beside it.

**Why a suffix.** The object plane is shared: anyone may publish to any
topic, and a topic is only a name. Readable names collide (every team would
like `mail` or `support`), and two unrelated communities on one topic would
receive, store and answer each other's envelopes. A suffix drawn at random
once per office makes an accidental collision negligible (about 2^-47 for any
one pair). It is not a secret and not an access control: anyone who knows it
can publish to it.

**Who creates one, and how a sender learns it.** Anyone can: a host operator
for its users, a community, a workspace, or one person. A recipient tells a
sender its office (and, for unicast, its hosts) out of band in this version;
a later version names it in a member of an identity record the recipient
publishes, which a sender resolves from the recipient's key or handle. A host
advertises the offices it carries by BRC-88 SHIP like any other topic.
Several offices shard load by construction: a recipient that reads two offices
lists both.

### 7.2 The lookup service `ls_bbox`

One lookup service per host answers for every office the host carries. It
answers BRC-24 questions of exactly these classes:

| Class | Question (`query` object) | Answer (`output-list`) | Price |
| --- | --- | --- | --- |
| `inbox` | `{"office": o, "to": k}` | the open envelopes to `k` in any box, first page | free |
| `inbox-after` | `{"office": o, "to": k, "after": a}` | the same, after the cursor `a` | free |
| `box` | `{"office": o, "to": k, "box": b}` | the open envelopes to `k` in box `b`, first page | free |
| `box-after` | `{"office": o, "to": k, "box": b, "after": a}` | the same, after the cursor `a` | free |
| `sender` | `{"office": o, "to": k, "from": f}` | the open envelopes to `k` from `f` in any box, first page | free |
| `sender-after` | `{"office": o, "to": k, "from": f, "after": a}` | the same, after the cursor `a` | free |
| `receipt` | `{"office": o, "by": k, "receiptFor": t}` | the answered receipts by `k` whose `acks` list the txid `t`, at most 8 (whether or not the host still holds the envelope; the reader checks that the envelope's `to` is `k`) | free |
| `sweep` | `{"office": o, "spent": p}` | the admitted sweeps that spend the outpoint `p`, at most 8 | free |
| `history` | `{"office": o, "history": k}` | envelopes to `k` that are no longer open (acknowledged, expired or out of the window) and that the host still keeps, first page | priceable |
| `history-after` | `{"office": o, "history": k, "after": a}` | the same, after the cursor `a` | priceable |

An **open** envelope is one a free question answers: section 8.3.

The query is validated as parsed, so hosts in any language reach the same
verdict. A member given twice takes its last value, as JSON parsers do. Every
member is a string: `office` meets the office identifier grammar (the full
`<name>_<suffix>`); `to`, `from`, `by` and `history` are 66 lowercase hex
digits whose bytes meet the key rule; `box` meets the box name grammar;
`receiptFor` is a txid, 64 lowercase hex digits in display order; `spent` is
an outpoint as BRC-100's `OutpointString` writes one (`<TXID>.<index>`): a
txid in display order, a dot, and the output index in decimal with no leading
zero; `after` is a **cursor**, the
decimal `created` of an envelope (1 to 253402300799, no leading zero), a
colon, and its txid in display order. A question MUST carry exactly the
members of one class. A missing member, a member no class defines, or a value
that breaks these rules is refused with an error, never answered empty and
never ignored. A question that names an office the host does not carry is
refused with an error too: an empty answer there would read as an empty
box.

**Order and pages.** Envelopes in an answer are in ascending order of
`created`, then of txid (display order, as lowercase hex strings). A page
holds at most 64 envelopes. The `-after` classes answer only envelopes that
sort strictly after the cursor, so a client pages by passing the last
envelope it received; a cursor of a time and 64 zeros asks for everything
created at or after that second. A cursor pages within one read and never
carries across reads: an envelope that arrives late, or whose sender's clock
is behind, can sort before a cursor saved earlier, so a client that polls
starts each poll from the first page. Receipts and sweeps in an answer are in
the same order, by their own `created` (a sweep's block height) and txid.

The `sender` classes are how a recipient reads the senders it knows when a
box is crowded: any key can write to any box, and the oldest-first page
belongs to whoever wrote earliest, so a recipient that has more than it will
page through reads its contacts by `from` and acknowledges the rest in
batches.

An empty answer means only that this host holds nothing that answers: a
reader MUST NOT read it as proof that an envelope was never sent. The engine
hydrates each answered output as BEEF: the record is inside the carrier's
BEEF, with the funding tree and its proof, so a reader verifies an answered
carrier with nothing but its own block headers (whether it was since
retracted is section 13 step 5).

There is no class that lists box names with counts: the reference engine's
lookup answers are output lists, and a count a host asserts is not something
a reader can check. A client counts what `inbox` returns.

### 7.3 Free and priceable

A price attaches to a question class, never to part of an answer:

- the eight free classes are answered at no price, without authentication, on
  every conforming host, and their answers are the same on a host that
  charges for nothing and on one that charges for everything else;
- no priced class is a superset of a free class: the priceable classes carry
  `history`, never `to`, `by` or `spent`, so no spelling turns a free
  question into a priced one;
- an unknown member is refused, never ignored, because ignoring one would let
  any extra word make a priceable alias of a free class.

A price on a lookup sells availability, latency and retention, never
exclusivity: every host on the office holds the same envelopes, and a reader
refused at one host can ask another. The priceable classes are sold under
BRC-105: a priced question is asked on the same BRC-24 `/lookup` route as a
free one, over BRC-104 authentication; the host answers 402 with BRC-105's
headers (the price, the payee's identity key and the derivation prefix), and
answers the question once paid. A price is for one question, which is one
answer page. A host MAY offer a
watch stream that notifies a recipient of new envelopes; it is priceable, it
carries nothing a free class does not answer, and its route and event format
are not part of this version.

### 7.4 The host terms document

A host that prices anything publishes its terms at

```text
GET <base>/ls_bbox/terms
```

where `<base>` is the base URL of `ls_bbox`, the value BRC-180's
`metanet.overlays` names for it (BRC-180 rule 2: a service defines its own
routes, and the manifest does not respecify them). The answer is JSON:

```json
{
  "service": "ls_bbox",
  "terms": 1,
  "classes": [
    { "class": "history", "satoshis": 5 },
    { "class": "history-after", "satoshis": 5 }
  ]
}
```

- `service` is `ls_bbox`; `terms` is the document's version, `1`;
- `classes` lists each class the host prices, by the class name of section
  7.2, with `satoshis`, the price of one question, an integer from 0 to
  2^53 - 1;
- a client ignores members it does not know, and entries for classes it does
  not know;
- a free class never appears with a price. A client MUST NOT pay for a free
  class whatever a document or a 402 says, and a host that answers a free
  question with 402 is outside this specification;
- a host that serves no terms document (404) charges for nothing;
- the document is informative: the 402 response's own headers are what a
  client pays against, and the payee and the derivation prefix are only
  there.

No price, payee or term is ever in a record: a price is the most changing
attribute in the system, and a record is delivered to, and paid for by, every
host on the office.

## 8. The host

### 8.1 Admission by `tm_bbox_<name>_<suffix>`

**What a verdict may depend on.** An admission verdict depends only on the
transaction's own bytes, the bytes of the transactions its inputs name (bound
to it by their txids), the rest of the BEEF it arrived in, and the host's
block headers: never on what else the topic holds or the order in which
objects arrived. No verdict reads the topic's previous coins (BRC-22): a
carrier's funding output is read from the funding tree its BEEF carries, and
a sweep's effect is read from its inputs' outpoints, which the sweep itself
names (a mined sweep's BEEF carries its proof and no parents). Previous coins
decide only which held outputs a transaction retains. Every host with the
same headers and the same version of this contract therefore admits the same
carriers and sweeps, whatever the plane's delivery order, repair or lateness
did. SPV of the BEEF (scripts, amounts, the funding parent's proof) is the
engine's and runs before the topic manager. Checks then run in the order
given; a transaction is refused for the first rule it breaks, with that
rule's reason.

**Admit or raise.** An overlay engine records every transaction its topic
manager returns admittance instructions for, empty ones included: a later
submission of the same txid is a duplicate that no topic manager is shown
again, and the engine marks spent, or deletes, every held output the
transaction spends. A recorded refusal is therefore permanent for that txid,
whatever arrives later. It would let anyone suppress a genuine carrier at a
host by submitting it first in a different BEEF (with the funding tree
unproven, say), and it would keep a host that upgrades to a later version of
this contract from ever admitting what it refused before. So a topic manager
returns admittance instructions only for a transaction it admits (at least
one output admitted, or a held output retained), and **raises for every
refusal**; the engine then counts a failed admission for that submission and
stores nothing. The cost is that a refused object offered again is checked
again, which the cheap-first order of the rules and a host's rate limits
bound.

Every script comparison below is byte for byte against the script rebuilt
from the decoded fields and the derived key, exactly as bcommon `pushdrop`
writes it: the 33-byte compressed key, `OP_CHECKSIG`, each field as one
minimal push, the signature as one minimal push when the output is signed,
and the fewest `OP_2DROP` then `OP_DROP` that clear the fields.

**Classify.** An output **claims a record** when its locking script starts
with `0x21`, 33 bytes and `0xac` (a 33-byte push and `OP_CHECKSIG`), followed
by one push (an opcode `0x01` to `0x4b`, or `OP_PUSHDATA1`, `2` or `4` with its
little-endian length, every declared byte present) whose data starts with a
CBOR definite-length map head, key 0 and a record magic: `00 44 62 62 65 01`
after the head claims an **envelope**, `00 44 62 62 72 01` a **receipt**.
Nothing else about the script is read to classify it (`script-v1.json`). A
transaction with more than one claiming output is refused
(`carrier-shape`).

**Envelope or receipt carrier.** A transaction with a claiming output. The
owner is the record's `from` or `by`:

1. `carrier-shape`: it does not have exactly one input and exactly one
   output, or the output's value differs from the value of the output the
   input spends;
2. `beef`: the BEEF does not hold exactly two transactions, the carrier and
   the transaction its input spends, and exactly one Merkle path, that
   transaction's, holding only the hashes that path needs (so a carrier's
   funding tree is mined, and nothing else rides along to be stored and
   served);
3. the record's own rules (sections 3.1 and 5), with their reasons;
4. `office`: the record's `office` is not this topic's;
5. `mineable`: `nLockTime < 4102444800`, or the input's `nSequence` is
   `0xFFFFFFFF`;
6. `unlock`: the input's unlocking script is not exactly one minimal push of
   a strict low-S DER signature followed by the sighash byte `0x41` (bcommon
   `carrier.CheckUnlocking`);
7. `lock`: the output script is not the well-formed one-field signed PushDrop
   locked to `derive(owner, "envelope")`;
8. `signature`: the field signature is not strict low-S DER, or does not
   verify over `SHA-256(record)` under that key (stricter than bcommon
   `carrier.Validate`, which accepts a high-S field signature);
9. `funding`: the output the input spends, read from the funding tree in the
   BEEF, is not a well-formed funding output `62 62 02` locked to
   `derive(owner, "envelope")`;
10. for an envelope, the content rules of section 4.6, with their reasons.

Rules 5 to 8 follow bcommon `carrier.Validate`'s order. Rules 6 and 9 make the
commitment the owner's alone: rule 9 means only the owner's key can spend the
input, and rule 6 means nobody else can re-encode that spend under another
txid. The content rules come last because they are the most work, and by then
the carrier is known to be the owner's.

**Sweep.** A transaction whose outputs claim nothing, whose output 0 is a
well-formed unsigned funding output `62 62 02` (under any compressed key: a
sweep's owner is whoever could sign its inputs), and that has a proof in its
BEEF verifying against the host's headers: output 0 is admitted and every
input's outpoint is recorded. Without the proof it is refused `unmined`.
Only output 0 is admitted, so a mined transaction cannot load a host with
thousands of held outputs, and nothing unmined is admitted, so a stranger
cannot fill a topic with free outputs. (bcommon `carrier.Sweep` writes its
tombstone at output 0; a funding tree someone publishes is admitted by the
same rule and is harmless.)

**Spend.** A transaction that claims nothing and is not a sweep, but spends
outputs the topic holds, is accepted for its inputs alone: it retains them.
Anything else is not for this topic and is refused (`not-bbox`), raising like
every refusal. This is the one verdict that reads what the topic holds; it
admits no record, and because a refusal raises, a spend refused before what
it spends arrives is decided again when offered again.

**Retention and spends.** For every transaction it accepts, the topic manager
lists every input that spends an output the topic holds as a coin to retain,
so no admitted output is deleted when something spends it. The lookup service
does not rely on the engine's spend notices, which cover only held outputs:
it records, for each carrier, the funding outpoint its input spends, and for
each admitted sweep, every outpoint its inputs spend.

**What a host does not do.** It does not evict unproven carriers: carriers
are unproven by design. It does not broadcast a carrier: a carrier is a
record, never a transaction for the chain. An engine's broadcaster is set for
the whole engine, not per topic, so an engine that carries `tm_bbox_` topics
runs with no broadcaster. It counts refusals by reason.

### 8.2 The box index

The lookup service keeps, per office, the admitted envelopes by `to`, `box`
and `from`, the admitted receipts by `by`, and one **outpoint row** per
funding outpoint any admitted carrier has spent: the commitments of the
carriers on it, which of them is the outpoint's **winner**, whether that
winner was dropped, and whether an admitted sweep spends the outpoint. The
**winner** of an outpoint is the carrier with the lowest `C` (compared as 32
bytes in hash byte order) among the envelopes on it that an answered receipt
acknowledges, or, when none is acknowledged, among all carriers on it. Each
carrier's status is a function of the admitted set, the outpoint rows and the
host's headers, never of arrival order, so a restart that rebuilds the index
in any order reaches the same statuses (the first that applies wins):

| Status | When |
| --- | --- |
| `retracted` | an admitted sweep, mined in the host's best chain, spends the carrier's funding outpoint |
| `superseded` | it is not its outpoint's winner, or its outpoint's winner has been dropped |
| `acknowledged` | (envelopes) an answered receipt, one neither retracted nor superseded, with `by` equal to the envelope's `to`, lists the envelope's `C` in `acks` |
| `held` | otherwise |

A carrier that is `held` or `acknowledged` is **answered**; a retracted or
superseded one is answered by no class, and a superseded receipt
acknowledges nothing. One funding output therefore buys at most one answered
envelope or receipt, whatever its owner does: an owner can make many carriers
from one funding output, since carriers are never mined and never conflict on
chain, but only the winner counts. Because an acknowledged envelope wins, a
sender cannot replace a message its recipient has read by grinding a lower
`C`; and because an outpoint whose winner was dropped admits no new winner, a
funding output pays for one answered carrier over the host's life, never one
per retention period. The index keeps up to 8 superseded carriers per
outpoint as evidence that the owner equivocated (all of them when the
outpoint's winner is acknowledged, up to 64), counts the rest, and MUST drop
the rest (section 8.4). A superseded carrier is still admitted, stored until
it is dropped, and delivered by the plane: what bounds that stream is the
plane's and the host's rate limits, not postage.

A receipt that arrives before the envelopes it names is kept and applies when
they arrive; a sweep that arrives before the carriers it retracts does the
same. When a block that holds a sweep or a carrier's funding tree leaves the
host's best chain, the host recomputes: the sweep retracts nothing until it is
mined again, and a carrier whose funding tree has no valid proof is not
answered until it has one.

### 8.3 What a free question answers

An answered envelope is **open** at the host's time `now` when it is `held`
(not acknowledged) and:

- `expires` is 0 or `now < expires`;
- `created >= now - 2592000` (the **answer window**: 30 days);
- `created <= now + 3600` (a created time more than an hour ahead of the
  host's clock is not answered until it is reached).

The `inbox`, `box` and `sender` classes answer open envelopes only. The
window is measured from the sender's `created`, not from when the host first
saw the envelope, so that an old envelope submitted again after a host has
dropped it is not answered again, and so that every host answers the same set
for the same `now`. Two hosts whose clocks differ can disagree about an
envelope at an edge of the window for as long as their clocks differ; a reader
comparing hosts reports that as a disagreement, never merges it silently.

The `history` classes answer answered envelopes that are not open because
they are acknowledged, expired or out of the window (not those dated ahead),
for as long as the host keeps them. The `receipt` class answers answered
receipts, and the `sweep` class admitted sweeps.

### 8.4 Retention

How long a host keeps what it no longer answers is its policy, with floors:

- an envelope that is open is kept;
- a receipt is kept at least 31 days after the host first saw it. A recipient
  acknowledges only envelopes it has read, which a host answered only while
  their `created` was within an hour of the host's time or earlier, so the
  receipt outlives the window of every envelope it can name. The floor is
  measured from first sight, never from the receipt's own `created`, which is
  a claim;
- a sweep is kept at least 31 days after the host first saw it;
- **outpoint rows, and the sweep outpoints they record, are kept for the
  host's life.** They are tens of bytes each, and they are what keeps a funding
  output from paying twice: without the row, a carrier made later on a swept
  or spent outpoint would be a fresh txid and would be answered.

To **drop** a carrier is to delete its outputs from the topic and mark its
outpoint row. The engine's record that the transaction was applied remains,
so a dropped carrier offered again is a duplicate and is neither admitted nor
answered again. A host that also deletes that record to reclaim storage
re-admits a carrier offered again, and its outpoint row keeps it superseded
or retracted.

### 8.5 Restore

The engine does not replay admissions. On start the service rebuilds its
index from the topic's stored outputs and the outpoints it persisted:
envelopes by `to`, `box` and `from`, receipts by `by`, carriers by funding
outpoint, retractions from recorded sweep inputs, and statuses from the set
(section 8.2), whatever order storage returns them in.

## 9. Delivery: plane and unicast

A publisher (a sender with its envelopes and sweeps, a recipient with its
receipts) reaches the office's hosts in one of two ways. The records, the
hosts and the readers are the same in both.

| | plane | unicast |
| --- | --- | --- |
| The publisher submits each object | once, to its facade on the plane | to every host it names, each on its own |
| Every other host receives it | from the plane, which repairs loss | only from the publisher, or from anyone who copies it across later |
| A host that was down | receives what it missed from the plane's repair | misses it until someone submits it again |
| Needs | a plane that carries the office's topic | nothing but the hosts |

**Plane.** The publisher submits the carrier once, with the office's topic
first in the object's topic list; the plane delivers it to every subscribed
host and repairs loss. Nothing in bbox disables, bypasses or asks for an
exception from repair, for any class, office or configuration; lateness is
the reader's to handle. A sweep is also submitted directly to every host the
publisher names, because a host off the plane learns of a sweep only by
receiving it.

**Unicast.** The publisher submits the same bytes to each host it names (at
most 16), retries each on its own, and counts the object published once a
quorum of hosts took it. A host **took** the object when its answer admits
outputs for the topic, or when a lookup at that host answers the object (`box`,
`receipt` or `sweep`). To a submitter, a duplicate and a refusal look the same
(an answer that admits nothing: a raised refusal is not returned as an
error), so an answer that admits nothing is confirmed by lookup before it
counts. A sweep must reach every host named.

**Filling a host that missed something.** Carriers and sweeps are
self-verifying, so anyone who holds one may submit it to a host that lacks
it: a recipient that finds an envelope at one host and not another copies it
across; a recipient resubmits its own receipts; anyone who finds a sweep at
one host (the `sweep` class) submits it to another. A host that dropped a
carrier does not take it back (section 8.4).

**Reading.** A reader asks one or more hosts the same question and verifies
every answer against its own headers (section 13). A reader that asks several
and receives different sets reports the difference as such, never merges it
silently.

## 10. Payments

Payments are bilateral: the plane carries the envelope as an opaque record,
and the payment inside it settles between the two parties when the recipient
broadcasts it. No host sees, relays or settles it as a payment.

- **Inside an envelope.** A sender that pays a recipient builds a BRC-29
  payment to keys derived from the recipient's identity key, serializes it as
  Atomic BEEF, and places it in the encrypted plaintext's `payment` member
  (section 4.5). The BRC-169 envelope's own `payment` member is `null`. The
  sender MUST NOT broadcast the payment: the recipient verifies it against
  its own headers (BRC-67) and internalizes it through its wallet (BRC-100
  `internalizeAction`), which broadcasts it (BRC-169 section 6.1).
- **Why encrypted.** A payment in the clear on a shared plane would be an
  unbroadcast transaction every host and every subscriber could broadcast
  first, which takes from the sender the choice BRC-169 section 8.3 relies
  on (reclaiming the inputs of an undelivered payment), and would publish the
  amount beside who paid whom. Encrypted, no host sees it, so BRC-169's
  delivery invariant ("If the payment attached to an envelope is internalized,
  that envelope MUST be delivered") holds trivially: only the recipient can
  internalize, and only by reading the envelope.
- **Reclaiming.** An envelope that carries a payment MUST have
  `expires >= created + 7200`; a recipient ignores a payment in an envelope
  without one. The recipient
  internalizes a payment only while `now < expires - 3600`; the sender MAY
  spend the payment's inputs elsewhere only once `now > expires + 3600` and
  only if they are unspent and no transaction spending them is known to the
  sender's chain source, mempool included. The two hours between the two rules absorb
  clock skew and propagation, so neither side acts on the other's silence:
  a missing receipt is never evidence that a payment was not internalized
  (an empty answer proves nothing), and the chain, not a lookup, settles who
  spent the inputs. A recipient that internalizes a payment acknowledges its
  envelope; a recipient that finds the inputs already spent reports the
  payment as reclaimed.
- **Tolls and scopes.** A recipient's reachability policy (BRC-169 section 8:
  `everyone`, `contacts`, `ecosystem`, `toll`) is enforced by the recipient's
  client in this version: it filters and does not internalize what its policy
  refuses. Every replica admits the same envelopes; what a policy at
  admission would change is only who stores what a recipient refuses.
- **Paying a host.** A host is paid by its readers for the priceable classes
  (section 7.3), never by the publisher and never through the plane.

## 11. Rendering and terminal safety

Everything in a record, and everything a recipient decrypts, is text someone
else wrote. A renderer MUST filter it before display, stripping control
characters, escape sequences, bidirectional and zero-width characters, and
bounding the output (bcommon `termsafe.Sanitize`; a web renderer escapes for
HTML and applies the same character rules). That includes the plaintext's
`body`, the BRC-169 `handle`, `tag` and `domain` claims, box names and office
identifiers. Records are carried and stored verbatim; filtering happens at
display, never at admission. The `envelope-note` vector's plaintext carries
an escape sequence for exactly this reason.

## 12. Size and rate bounds

| Quantity | Bound | Why |
| --- | --- | --- |
| Envelope content (key 7) | 16384 bytes (16 KiB) | a message, not a file: larger content is referenced (section 4.5). About 11 KiB of plaintext after base64 and the BRC-78 header |
| Envelope record, encoded | 20480 bytes | the content plus 4 KiB for the other fields and unknown keys |
| Receipt record, encoded | 4096 bytes | 64 acknowledgements take about 2.2 KB |
| Acknowledgements per receipt | 64 | bounds a receipt and the index work it causes |
| Entries in a record map | 64 | bounds the work of one record |
| JSON nesting in the content | 16 | bounds a parse |
| Envelopes per answer page | 64 | bounds an answer |
| Receipts per `receipt` answer, sweeps per `sweep` answer | 8 | one is the normal answer |
| Superseded carriers kept per funding outpoint | 8 | evidence of equivocation; the rest are counted |
| Answer window | 30 days before the host's time, 1 hour after | section 8.3 |
| Carrier BEEF | exactly the carrier and its funding tree (section 8.1); a host MAY refuse, raising, a submission over its own bound, which is at least 262144 bytes | a 1000-output funding tree makes a carrier of about 70 KB |
| Carrier object on the plane | the plane operator's object bound (BRC-149) | a publisher MUST check a carrier's BEEF against it before publishing |

A reader accepts every record within these bounds. A later version may raise
a record bound, which is a coordinated change of senders, hosts and readers;
it never lowers one, so a record published under a bound stays readable.

Below these bounds, what a sender sends, how fast, and how a host limits its
readers are policy: [limits.md](limits.md) sets the recommended defaults and
caps (a smaller content size on the plane, because a carrier is fragmented
into packets and a smaller object survives loss better; send rates; funding
tree sizes; host counts; retries), none of which a reader relies on. A host
MAY rate-limit lookups, answering 429 with `Retry-After`, and a client MUST
respect it.

## 13. Verification by a reader

A recipient that lists its box verifies each answered envelope against its
own block headers:

1. the carrier's BEEF: SPV through its funding parent, whose proof it
   verifies against its headers; `C = txid(K)`;
2. the carrier rules of section 8.1 for the record in it, including the
   content rules, and `to` equal to its own key;
3. the BRC-78 message: it decrypts with its own key (a failure means the
   sender encrypted something the recipient cannot read: it is shown as
   undecryptable, never as empty);
4. the plaintext against section 4.5; a `payment` is verified and
   internalized as section 10 says, and each `refs` entry only when fetched;
5. whether the envelope was retracted: a host serves nothing retracted, and a
   reader that holds an envelope it read earlier asks the `sweep` class of one
   or more hosts (or a chain source it trusts) for the carrier's funding
   outpoint; an answer that is a transaction mined in the reader's headers
   and spending that outpoint makes the envelope retracted. An empty answer
   does not prove the envelope unretracted.

A sender that wants delivery confirmation asks the `receipt` class for its
envelope's txid and verifies the receipt the same way: its carrier is derived
from `by`, and `C` is in its `acks`.

The result proves that the sender's key made the envelope and that it reached
a host. It does not prove that the recipient read it until a receipt says so,
and a receipt proves only that the recipient's key acknowledged it.

## 14. Alignment with existing patterns

| Choice | Pattern followed | Where it differs, and why |
| --- | --- | --- |
| Store and forward in named boxes; list, then acknowledge, and an acknowledged message is dropped | BRC-33 (`/sendMessage`, `/listMessages`, `/acknowledgeMessage`; "After acknowledgment, the server deletes a message") | the server is every host on an office; list is a BRC-24 lookup; acknowledgement is a signed receipt every replica applies, so no host's word is needed for it |
| Messagebox semantics and the envelope | BRC-169 section 7.1 (BRC-33 semantics; transport and internals may vary "provided those semantics and the enforcement duties of section 8 hold") and section 7.2 (metadata in the clear, content encrypted, a signature over the RFC 8785 serialization without `content` and `signature`) | departures: the signature is made with the sender's derived key for `signature`, not the identity key itself (a BRC-100 wallet cannot sign with its identity key), so a BRC-169 verifier does not verify it; the recipient carries an `identityKey`; `payment` is `null` and the payment rides encrypted; the section 8 enforcement duties (scope, toll) are the recipient client's, not the host's; no toll quote endpoint is offered, so a `quoteId` is carried but its single use (section 8.3 rule 3) is not enforced by hosts; the carried bytes must be canonical |
| Payment inside an envelope, unbroadcast, internalized by the recipient | BRC-169 section 6.1 and appendix A.7 (member names); BRC-29 payment derivation (a suffix and an output index for every output); BRC-95 Atomic BEEF; BRC-67 SPV; BRC-100 `internalizeAction` | inside the encrypted payload rather than the metadata, because every host on a shared plane would otherwise see and could broadcast it; outputs listed per output |
| Reachability policy and tolls | BRC-169 section 8 | enforced by the recipient's client in this version rather than at the messagebox; every replica admits the same set |
| Content encryption | BRC-78 (protocol `message encryption`, random key id, the serialization's header); BRC-2 for the symmetric form (a 32-byte IV and the GCM tag, as go-sdk implements it) | the version bytes are go-sdk's order, which is the BRC-78 table's and not its hex example's; the header is checked by hosts against the record |
| Sender authentication | BRC-42 derivation with BRC-43's `anyone` counterparty; BRC-48 Pay to Push Drop | lock-before, as bcommon `pushdrop` writes it; the carrier's lock and field signature under `derive(from, "envelope")` bind the record to the sender at consensus level, so BRC-33's deferred "digital signature schemes" are answered by the substrate |
| Records never mined, kept off chain | BRC-60's non-final transaction (kept open by its sequence numbers) | a far-future lock time keeps the record off the chain; nothing is updated in place |
| Canonical CBOR records with integer keys; unknown keys preserved | RFC 8949 section 4.2.1; BRC-174 section 3.2 (ignore unrecognised fields, preserve them when reconstructing) | one codec with every application on the same library |
| Record magic: prefix, type letter, version | BRC-171 section 6's header of a protocol prefix, a header version and a record type ahead of the fields | inside the CBOR record as key 0, and in the order bcommon's registry fixes (prefix, type letter, version: `"bb"`, `'e'`, `0x01`) rather than BRC-171's prefix, version, type |
| Topic and lookup names | BRC-87 naming; BRC-22 topic managers (admittance, coins to retain); BRC-24 lookup with `output-list` answers | a random suffix per office so that unrelated users do not collide on one topic |
| Host discovery | BRC-88 SHIP for which hosts carry an office; BRC-180 for what a named domain hosts | none |
| Priced questions, and the terms route | BRC-105 over BRC-103 and BRC-104; BRC-180 rule 2 (a service defines its own routes) | only the `history` classes are priceable, and the document is informative: the 402 is what a client pays against |
| Competing hosts | BRC-178 (race-settled collection markets for lookups and message box collection) | not built; every host holds the same envelopes, so BRC-178 applies with no change to a record |
| Carrier ancestry and proofs | BRC-62 and BRC-95 BEEF; BRC-74 BUMP, guarded by bcommon `guard` | a carrier's BEEF is exactly the carrier and its mined funding tree, so its ancestry is one level |
| Objects on the plane | BRC-148 and BRC-149 (the object frame; the operator bounds the size) | none |
| References to larger content | BRC-26 UHRP (content named by its SHA-256) | inside the encrypted payload, with a key when the referenced bytes are encrypted |

## 15. What this does not do

1. **Metadata is public.** `to`, `from`, `box`, `office`, `created`,
   `expires`, sizes and times are visible to every host on the office and to
   anyone who follows its topic, and so is everything in the BRC-169
   envelope outside `content`: the recipient's `handle`, `tag` and `domain`
   and the sender's `handle` and `domain` when a sender includes them, a
   `quoteId`, and any member this contract does not define. BRC-169 says of a
   messagebox that "the operator learns who contacts whom, when, how often, at
   what tags"; here that is every host and every subscriber. A sender that
   does not need them SHOULD leave the handle claims out. Content, and with
   it any payment, is not visible.
2. **No forward secrecy.** BRC-78 provides none. Compromise of a recipient's
   identity key opens every envelope ever sent to it that any host or copy
   still holds. Short `expires` and prompt acknowledgement bound how long
   honest hosts hold it; a ratchet is not part of this version.
3. **Retraction and acknowledgement are not erasure.** Honest hosts stop
   serving and may drop; a copy anyone kept stays a copy.
4. **Spam costs postage, not permission.** A host answers one envelope per
   funding output of the sender's mined tree over its life, so flooding
   inboxes costs the sender mined transactions; the carriers an owner makes
   beyond that are superseded and bounded by rate limits, not postage.
   Filtering is the recipient's. A
   recipient reads known senders by `from` and clears the rest by
   acknowledging it, 64 envelopes a receipt.
5. **Hosts can withhold.** A host can omit envelopes from an answer; it cannot
   forge or alter one. Reading from several hosts detects withholding by one.
6. **Answers depend on the host's clock** at the window's edges (section 8.3).
7. **No push.** A recipient polls, or pays a host for a watch stream.
8. **Key compromise of a sender** lets the thief send as the sender and sweep
   the sender's trees; a new key is a new sender.

## 16. Vectors

`testdata/vectors` holds the golden vectors, generated by the codec in
`internal/boxrec` with `GOWORK=off go run ./cmd/vectors`; `go test`
regenerates them and compares byte for byte, and refuses a file nothing
generates.

| File | What it holds |
| --- | --- |
| `envelope-v1.json`, `envelope-*.hex` | three envelope records: a note whose record carries an unknown key and whose plaintext carries an escape sequence; an envelope that expires, with a `quoteId`, an extra BRC-169 member and a plaintext with a reference; a record of 64 keys, the most allowed. Each with its plaintext, the BRC-78 key id, IV and message, the carried BRC-169 envelope, the serialization the signature covers, the signature key, and the hash the carrier's field signature covers |
| `receipt-v1.json`, `receipt-*.hex` | a receipt of two acknowledgements and one of 64, each ack in hash byte order and as a display txid |
| `query-v1.json` | every class with its members, price and page; questions as JSON text, each with its class or refused |
| `json-v1.json` | the JSON subset of section 4.1 case by case: accepted or refused, the canonical serialization, and whether the input already is it |
| `script-v1.json` | the classifier of section 8.1 over raw locking scripts: real envelope, receipt and funding scripts from bcommon's PushDrop, and scripts that claim nothing |
| `derivation-v1.json` | the registry values; the keys each test identity derives for `envelope` and `signature` under counterparty `anyone`; the SDK's refusal of `[1, "bbox"]`; two offices with one readable part and different suffixes, and one suffix drawn from fixed bytes |
| `refusal-v1.json` | byte strings the record and content rules refuse, each with its reason (sections 3.1, 4.6 and 5), and what admission does with an output whose first field it is: skip one that claims no record, or refuse for the reason of the record it claims (which differs where the magic names the other record). Content cases pass every earlier rule and break exactly one |

The vectors' office is `example_office_qzxkvbmwtr`; its suffix is a fixed
example so that the vectors are reproducible. The sender's test key is 32
bytes of `0x42` and the recipient's 32 bytes of `0x43`; both are public and
never for real use. BRC-78 key ids and IVs are fixed stand-ins for random
bytes, `SHA-256("bbox/vector/keyid/<label>")` and
`SHA-256("bbox/vector/iv/<label>")`; the tests decrypt every message with
go-sdk's `message.Decrypt` and through the recipient's wallet. Receipt
commitments are fixed stand-ins, `SHA-256("bbox/vector/commitment/<i>")`.
Signatures are deterministic (RFC 6979).
