# Frozen at first publish

Every item below is hashed into a key, written into a record, or named by a
host. Once one record using it is on a public host, readers verify against it
for as long as anyone reads that record, so changing it afterwards is a new
identifier or a new record version, never an edit.

**Status: signed.** Every row below is signed. The rows exercised only by
transaction and payment vectors (carrier shape, script canonicality,
signatures, sweep and the plaintext `payment` shape) were signed once
`transaction-v1.json` and `payment-v1.json` pinned them. The registry
rows in section 1 belong in bcommon's registry (`docs/registry.md`), where
they are checked against every registered row; this repository does not edit
that registry.

## 1. Registry entries

| Kind | Value | Use | Status |
| --- | --- | --- | --- |
| Protocol | `[1, "bbox message"]` | every bbox derivation. BRC-43 refuses a protocol name under five characters, so `[1, "bbox"]` cannot derive (`derivation-v1.json` records the SDK's refusal) | signed |
| Key id | `envelope` | the record output of every carrier (envelopes and receipts), its field signature, and funding-tree outputs | signed |
| Key id | `signature` | the BRC-169 envelope signature (spec section 4.4) | signed |
| Key id | `fund` | the embedded wallet's funding key | signed |
| Tag prefix | `bb` (`62 62`) | owned by bbox. Registered prefixes are `bf`, `gw`, `bl` and `vx` (tests); none planned is `bb` | signed |
| Tag | `bb` `0x02` | funding-tree output, its one field (the `0x02` every registered application uses for funding) | signed |
| Record magic | `bbe` `0x01` | envelope record, version 1 (`"bb"`, `'e'`, version) | signed |
| Record magic | `bbr` `0x01` | receipt record, version 1 (`"bb"`, `'r'`, version) | signed |
| Topics | `tm_bbox_<name>_<suffix>` | one topic per office: a readable `<name>` of at most 31 characters and a 10-letter random `<suffix>` fixed at the office's creation (spec section 7.1); the `tm_bbox_` namespace belongs to bbox | signed |
| Lookup service | `ls_bbox` | one per host, all offices; every question names its office | signed |
| Host route | `<base>/ls_bbox/terms` | the host terms document (spec section 7.4) | signed |
| Baskets | `bbox fund`, `bbox envelope funding`, `bbox kill tombstone` | wallet baskets; never on a public host, registered so no two applications share one in a wallet | signed |

Tag type bytes `0x01`, `0x03` to `0x64`, `0x66` to `0x71` and `0x73` to
`0xff` under `bb`, and record letters other than `e` and `r`, are unassigned
and stay bbox's. `0x65` and `0x72` are never assigned as tag type bytes,
because `bb 0x65` and `bb 0x72` are the first three bytes of the record
magics `bbe` and `bbr`.

## 2. Mechanism shared with the substrate

| Item | Value | Status |
| --- | --- | --- |
| Derivation setting | BRC-42, counterparty `anyone`, `forSelf = true` on the owner's side | signed |
| PushDrop layout | lock-before, field signature last, over `SHA-256` of the one field | signed |
| Carrier non-final device | writers use `nLockTime = 4102444800` and input `nSequence = 0`; hosts accept any `nLockTime >= 4102444800` and any `nSequence` below `0xFFFFFFFF` | signed |
| Carrier shape | exactly one input spending an owner funding output of a mined tree, unlocked by exactly one minimal push of a strict low-S DER signature and sighash `0x41`; exactly one output carrying the whole input value; the owner is `from` or `by`; the BEEF is exactly the carrier and its funding tree with one minimal Merkle path (the counts as declared on the wire, a txid-only entry counting as a transaction; BEEF V1, V2 and Atomic accepted; minimal as spec section 8.1 rule 2 spells it, so a funding tree alone in its block is refused); a BEEF that does not carry the spent output is refused `beef`; refused in the order `carrier-shape`, `beef`, record rules, `office`, `mineable`, `unlock`, `lock`, `signature`, `funding`, content rules (spec section 8.1) | signed |
| Script canonicality | every checked script equals, byte for byte, the one rebuilt from its fields and the derived key | signed |
| Signatures | field, input and BRC-169 signatures strict DER, low S; input signatures sighash `0x41` only | signed |
| Classifier | an output claims a record when its script is `0x21`, 33 bytes, `0xac`, then a push (`0x01` to `0x4b`, or `OP_PUSHDATA1`, `2`, `4`, every byte present) whose data starts with a CBOR definite map head, `00 44` and `bbe 0x01` or `bbr 0x01`; two claiming outputs refuse the transaction (`script-v1.json`) | signed |
| Funding output | `<derive(owner, "envelope")> OP_CHECKSIG <62 62 02> OP_DROP`, unsigned; a carrier's funding parent carries its proof in the carrier's BEEF | signed |
| Sweep | a mined transaction whose outputs claim nothing and whose output 0 is a funding-shaped output under any compressed key (bcommon's tombstone); only output 0 is admitted; only a mined spend of a carrier's funding outpoint by another transaction retracts; a retracted receipt acknowledges nothing; an unmined transaction whose output 0 is funding-shaped is judged as a sweep even when it spends a held output, refused `unmined` and decided again when it arrives mined; a published funding tree, once mined, is admitted as a sweep (output 0 held, its fee input's outpoint recorded as swept) | signed |
| Publication | funding trees are not published to an office; sweeps are published once, after they mine, and also directly to every host named | signed |
| Commitment | `C = txid(K)`, hash byte order in records, display order in JSON | signed |
| CBOR | RFC 8949 section 4.2.1 core deterministic encoding, bcommon's subset, enforced on decode | signed |
| Admission | a verdict depends only on the transaction, the transactions its inputs name, the BEEF it arrived in and the host's headers; no verdict reads the topic's previous coins, which decide only retention; **a topic manager returns instructions only for what it admits and raises for every refusal**, so no refusal is recorded; transaction-level reasons `carrier-shape`, `beef`, `office`, `mineable`, `unlock`, `lock`, `signature`, `funding`, `unmined`, `not-bbox`; `beef` labels every refusal of a BEEF (over the host's bound, unparseable, and so on), and a host that names no bound uses 262144 bytes | signed |
| One answered carrier per funding outpoint, over the host's life | the winner is the lowest `C` (hash byte order) among acknowledged envelopes on the outpoint, else among all carriers on it; every other carrier is `superseded`, and so is every carrier once the winner is dropped; up to 8 kept as evidence (64 when the winner is acknowledged), the rest dropped | signed |
| Status precedence | `retracted`, then `superseded`, then `acknowledged` (by an answered receipt whose `by` is the envelope's `to`), then `held`; a function of the admitted set, the outpoint rows and the host's best chain | signed |
| Outpoint rows | one per funding outpoint any admitted carrier spent (commitments, winner, dropped, swept), kept for the host's life | signed |
| Hosts do not broadcast carriers | an engine that carries `tm_bbox_` topics runs with no broadcaster | signed |
| Payment envelopes | `expires >= created + 7200`; the sender's "unspent" includes the mempool | signed |
| Coin retention | every accepted transaction retains every held output it spends; the lookup service records carrier funding outpoints and sweep inputs itself | signed |

## 3. The envelope record (magic `bbe` `0x01`)

| Item | Status |
| --- | --- |
| Key numbers and meanings: 0 `magic`, 1 `office`, 2 `to`, 3 `box`, 4 `from`, 5 `created`, 6 `expires`, 7 `content`; all required | signed |
| Types: `office` and `box` text; `to` and `from` bytes(33), canonical compressed keys; `created` and `expires` unsigned integers; `content` bytes | signed |
| `created` 1 to 253402300799 and `expires` 0 to 253402300799, Unix seconds; `expires` is 0 or after `created` | signed |
| Box name grammar: 1 to 50 bytes of `[a-z0-9_]`, starting with a letter, ending with a letter or a digit, no doubled underscore | signed |
| Unknown integer keys above 7 preserved and ignored; any non-integer key refuses the record; at most 64 keys | signed |
| The refusal order (spec section 3.1) and the reason labels the vectors carry | signed |
| The classifier: an output claims an envelope when its first PushDrop field starts with a CBOR map head, key 0 and `bbe 0x01`, a receipt with `bbr 0x01`; two claiming outputs refuse the transaction | signed |
| No salt and no plaintext reference list in the record | signed |

## 4. The content (a BRC-169 section 7.2 envelope)

| Item | Status |
| --- | --- |
| The JSON subset, decided on the parsed value in the order of spec section 4.1: valid UTF-8 without BOM, one object, unique names, no lone surrogate in any string or name, integers within 2^53 - 1 in magnitude, depth at most 16 counting the top level as 1, and the carried bytes exactly the RFC 8785 serialization (`json-v1.json`) | signed |
| Members: `metanetHandles` `"1.0"`; `recipient.identityKey` equal to `to`; `sender.identityKey` equal to `from`; `created` as `YYYY-MM-DDTHH:MM:SSZ` equal to the record's; `quoteId` optional string; `payment` exactly `null`; `content` a BRC-78 message in standard padded base64, one unbroken RFC 4648 string with zero pad bits; `signature` lowercase hex; other members allowed and signed | signed |
| **The payment rides inside the encrypted plaintext; the envelope's `payment` is `null`** | signed |
| BRC-78 header checked by hosts: version `42 42 10 33` (go-sdk's order), sender and recipient equal to `from` and `to`, at least 150 bytes | signed |
| **The BRC-169 signature is made under `derive(from, "signature")`, not by the identity key itself**, over `SHA-256(JCS(envelope without content and signature))` | signed |
| Content rule order and labels: `content-json`, `content-shape`, `content-cipher`, `content-signature` | signed |
| Plaintext members (client contract): `body` a string; `payment` (`beef` Atomic BEEF in standard padded base64; `derivationPrefix` and each `derivationSuffix` non-empty standard padded base64; `outputs` non-empty, of `{outputIndex, derivationSuffix, satoshis}` with distinct `outputIndex` and `satoshis` 1 to 2^53 - 1; unlisted outputs ignored, a listed output that does not pay the recipient refused); `refs` at most 32 (`url` `https://` or `uhrp://`, `sha256` and `key` 64 lowercase hex, `length` a non-negative safe integer, the key random per reference) | signed |
| The recipient's check order and labels (client contract, spec section 4.7): `undecryptable`, `plaintext-json`, `plaintext-shape`, `payment-shape`, `payment-expires`, `payment-late`, `payment-beef`, `payment-output`, `payment-spv`; "verifies against its headers" is full SPV (scripts and ancestor proofs) with no fee check | signed |
| Payment timing: the recipient internalizes only while `now < expires - 3600`; the sender reclaims only when `now > expires + 3600` and the inputs are unspent; never on a lookup's silence | signed |

## 5. The receipt record (magic `bbr` `0x01`)

| Item | Status |
| --- | --- |
| Key numbers and meanings: 0 `magic`, 1 `office`, 2 `by`, 3 `acks`, 4 `created`; all required | signed |
| `acks`: 1 to 64 commitments of 32 bytes, hash byte order, strictly ascending | signed |
| A receipt acknowledges an envelope only when `by` equals the envelope's `to` | signed |
| Unknown integer keys above 4 preserved and ignored; at most 64 keys | signed |

## 6. Names, questions and terms

| Item | Status |
| --- | --- |
| Office identifier `<name>_<suffix>`, 12 to 42 bytes: `<name>` 1 to 31 bytes of `[a-z]` and single underscores, starting and ending with a letter; `<suffix>` exactly 10 letters `a` to `z`, drawn uniformly at random once at the office's creation and fixed for its life | signed |
| The full identifier is what records, questions and topics carry; the readable part alone never identifies an office | signed |
| Class names and members: free `inbox` `{office, to}`, `inbox-after` `{office, to, after}`, `box` `{office, to, box}`, `box-after` `{office, to, box, after}`, `sender` `{office, to, from}`, `sender-after` `{office, to, from, after}`, `receipt` `{office, by, receiptFor}`, `sweep` `{office, spent}`; priceable `history` `{office, history}`, `history-after` `{office, history, after}` | signed |
| Query values validated as parsed; every member a string; keys 66 lowercase hex meeting the key rule; `receiptFor` 64 lowercase hex, display order; `spent` is BRC-100's `OutpointString`, `<txid>.<vout>`; `receiptFor` answers receipts whose `acks` list it, whatever the host still holds; `after` is `<created>:<txid>`; a question naming an office the host does not carry is refused | signed |
| Answer order ascending by `created` then txid (display hex); `-after` strictly after the cursor | signed |
| An unknown or missing query member is refused, never ignored; no priced class is a superset of a free class; free classes need no authentication | signed |
| Open envelope: held, not expired, `created` within 30 days before and 1 hour after the host's time (the answer window) | signed |
| Retention floors: open envelopes kept; receipts and sweeps kept 31 days after first sight; outpoint rows and sweep outpoints kept for the host's life; dropping deletes outputs, marks the outpoint row and keeps the applied record | signed (floors: a later version may lengthen them, never shorten) |
| Terms document at `<base>/ls_bbox/terms`: members `service`, `terms` (`1`), `classes` of `{class, satoshis}`; a price is per question (one page); unknown members ignored; absent means no charge; the 402 is authoritative and alone carries the payee and the derivation prefix; priced questions use the same `/lookup` route over BRC-104 | signed |

## 7. Bounds that are a floor

Readers accept every record within these, and a later version may raise
them but never lower them: content 16384 bytes, envelope record 20480 bytes,
receipt record 4096 bytes, 64 acknowledgements per receipt, 64 keys per
record, JSON nesting 16, 32 references per plaintext, a host's BEEF bound of at least 262144 bytes.
Signed as floors.

## 8. Not frozen

- The funding tree's size and output value.
- Host policy: how long envelopes and receipts are kept beyond the floors of
  spec section 8.4, lookup rate limits, prices, metric names.
- The page sizes (64 envelopes, 8 receipts, 8 sweeps) and the 8 superseded
  carriers kept per outpoint, which a later version may raise.
- Whether a host offers a watch stream, and its route and format.
- Anything the spec marks as a host's, a sender's or a recipient's choice.
- The limits of [limits.md](limits.md).
