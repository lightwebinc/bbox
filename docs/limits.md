# Limits

What a sender or a recipient publishes, how fast, and what a host answers
are bounded twice. The record bounds every reader accepts ([spec.md](spec.md)
section 12) are protocol and sit above everything here. The limits below are
policy: recommended defaults and caps for clients and hosts, none of them a
wire format or a frozen bound ([frozen.md](frozen.md)), and a later version
may change any of them without a new record version.

A **cap** is refused by a client before anything is sent, with a message
naming the setting and the limit. A **warning** is printed and the client
goes on.

## The table

| Limit | Default | Cap | Why | Plane and unicast |
| --- | --- | --- | --- | --- |
| Envelope content (key 7) | as small as the message allows | 16384 bytes (the record bound) | Every host on the office stores and is billed for every envelope; larger content goes by reference inside the encrypted payload | Plane: warn above 6144 bytes (8 packets with the default tree). Unicast: no warning |
| Funding tree outputs | 32 | 1000 | Every carrier's BEEF carries its whole tree, about 49 bytes an output | Plane: warn above 100 (3 more packets on every carrier) |
| Envelopes a sender sends, per office | 1 a second | 20 a second | Every subscribed host receives every envelope, and a subscriber's delivery budget is shared by all its topics; each envelope costs its sender a funding output | Unicast sends rate times hosts objects a second |
| Acknowledgements per receipt | batch for up to 10 s, then one receipt | 64 (the record bound) | One receipt carrier per batch rather than per envelope | Same |
| `expires` on an envelope that carries a payment | 1 day | the answer window | The recipient internalizes only until an hour before `expires`, and the sender reclaims only an hour after it (spec section 10), so `expires` is how long the recipient has | Same |
| Hosts named | none | 16 | A reader asks every host it names; a unicast publisher uploads every object to each | Unicast warns above 5 |
| Quorum | all | the host count | A quorum no host set can meet would refuse every publish | Unicast only; warns below a majority. A sweep always needs every host named |
| Tries per submission | 4, waiting 1, 2 and 4 s | fixed | A host that fails four tries is reported missed; the object stays persisted and is resent | Plane: the facade; unicast: each host on its own |
| Request timeout | 15 s | 1 s to 5 min | Bounds every request | Same |
| Pages a client reads per list | until an answer is short | 64 pages (4096 envelopes) | Each page is a lookup and 64 verifications | Same |
| Envelopes per answer page | 64 | 64 (spec) | Bounds an answer | Same |
| Receipts per `receipt` answer, sweeps per `sweep` answer | 8 | 8 (spec) | One is the normal answer | Same |
| Host retention of envelopes | until they leave the answer window | host policy | The window is 30 days (spec section 8.3); the `history` classes are what longer retention sells | Same |
| Host retention of receipts | 31 days after the later of created and first sight | at least that (spec section 8.4) | A receipt must outlive every envelope it names | Same |
| Lookup rate a host accepts | host policy | host policy; answers 429 with `Retry-After` | Resolution and lookup are an unauthenticated surface | Same |
| References per message (`-ref`) | none | 32 (the plaintext bound) | A reference is a locator, a digest and a length, about 250 bytes with a key | Same |
| Price the client pays for one priced question | none | 1000 sat unless `-max-sats` says more | A price is per question (one page), and a host that asks more than this is asked by the user, never paid by default | Same |
| BRC-104 sessions a host's terms route keeps | 10000, each forgotten after 600 s idle | host policy (`BBOX_SESSIONS`, `BBOX_SESSION_TTL`) | A handshake is unauthenticated: the store must not grow without end. Past the cap the least recently used goes and its client shakes hands again | Same |

## Where the numbers come from

**Carrier sizes, measured.** `internal/limits` mints real carriers with
bcommon from a mined funding tree whose proof has 16 levels (a block of
about 65,000 transactions) and measures the Atomic BEEF a sender publishes.
A packet carries 1128 bytes of object (the IPv6 minimum MTU of 1280 bytes
less the IPv6, UDP and object fragment headers).

| Content | Tree outputs | Carrier BEEF | Packets |
| --- | --- | --- | --- |
| 1024 | 32 | 3,799 bytes | 4 |
| 4096 | 32 | 6,872 bytes | 7 |
| 6144 | 32 | 8,919 bytes | 8 |
| 6144 | 100 | 12,252 bytes | 11 |
| 12288 | 32 | 15,063 bytes | 14 |
| 16384 | 32 | 19,158 bytes | 17 |
| 16384 | 100 | 22,491 bytes | 20 |
| 16384 | 250 | 29,840 bytes | 27 |

About 2.8 KB of every carrier is the tree, its proof and the carrier's own
fields; each tree output adds about 49 bytes. A receipt of 64
acknowledgements is a 2,256-byte record, so its carrier is about 5 KB, 5
packets.

**Why smaller on the plane.** An object larger than one packet is carried as
fragments. Every lost fragment is repaired, but the whole object must be
reassembled within a fixed window, and a fragment that needs a second or third
repair round at a long round trip can land after the object was given up.
In measurements at 1 % packet loss, no object of 1 or 8 packets was lost and
about 1 % of 16-packet objects were; over a round trip of about 140 ms, 12 %
of 64-packet objects were lost. An envelope at the full bound is 17
packets, so the plane warning sits at 8 packets, where no loss was seen. A
typical message (a few hundred bytes of text, or a payment of one or two
kilobytes of BEEF, which is base64 twice inside the content) is 4 to 7
packets.

**Unicast.** No fragments: a host takes each object in one HTTP request, so a
unicast publisher can use the full bound. What grows instead is its upload
and its retries, multiplied by the host count.

**Rates.** A plane bounds what one source submits and what one subscriber
receives, and the subscriber's budget is shared by every topic it takes. A
sender at the cap of 20 envelopes a second is a small part of a subscriber
budget of hundreds of objects a second; the default of 1 leaves room for many
senders on one office.

**Cost.** At a fee of 1 satoshi per byte a 32-output funding tree costs about
1,700 satoshis, about 53 an envelope; an envelope's carrier itself is never
mined. A host's delivered bytes are what an office costs its hosts, which is
why the content default is small.
