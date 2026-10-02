# Limits

What a sender or a recipient publishes, how fast, and what a host answers
are bounded twice. The record bounds every reader accepts ([spec.md](spec.md)
section 12) are protocol and sit above everything here. The limits below are
policy: recommended defaults and caps for clients and hosts, none of them a
wire format or a frozen bound ([frozen.md](frozen.md)), and a later version
may change any of them without a new record version.

A **hard cap** is refused by the client before anything is sent: exit 2,
with a message naming the setting, the limit and why, for example

```text
bbox: -tree-count 1200 is over the limit of 1000: every carrier's BEEF carries its whole funding tree (docs/limits.md)
```

A **recommendation** passed is a `warning: ...` line on stderr, and the
command goes on. A **host** limit is the host operator's, set in the host's
environment ([host.md](host.md)); a client cannot change it.

## The table

### Sending

| Limit | Default | Hard cap | Why | Plane and unicast |
| --- | --- | --- | --- | --- |
| Envelope content (record key 7) | as small as the message allows | 16384 bytes (the record bound) | Every host on the office stores every envelope; larger content goes by reference inside the encrypted payload | Plane warns above 6144 bytes (8 packets with the default tree); unicast does not |
| References per message (`-ref`) | none | 32 (the plaintext bound) | A reference is a locator, a digest and a length, about 250 bytes with a key | Same |
| Funding tree outputs (`-tree-count`) | 32 | 1000 | Every carrier's BEEF carries its whole tree, about 49 bytes an output | Plane warns above 100 (3 more packets on every carrier) |
| Tree minted ahead | one, when 4 outputs are left (half the tree when it has 4 or fewer) | one at a time per home, across runs | The next tree is settled while the last outputs are spent, so a send seldom waits for a block; a second would take another coin for nothing | Same |
| Envelopes a sender sends, per office (`-rate`) | 1 a second | 20 a second | Every subscribed host receives every envelope, and a subscriber's delivery budget is shared by all its topics; each envelope costs its sender a funding output | Unicast sends rate times hosts objects a second |
| `expires` on an envelope that carries a payment (`-expires`) | 1 day | 30 days (the answer window) | The recipient internalizes only until an hour before `expires`, and the sender reclaims only an hour after it (spec section 10), so `expires` is how long the recipient has | Same |
| `object_bound` | 1048576 | 8388608 | BRC-149: 1 MiB on the open path, 8 MiB on an authenticated one; no plane admits more | Plane only |

### Publishing and reading

| Limit | Default | Hard cap | Why | Plane and unicast |
| --- | --- | --- | --- | --- |
| Hosts named (`hosts`) | none | 16 | A reader asks every host it names; a unicast publisher uploads every object to each | Unicast warns above 5: past that the plane is what removes the cost |
| Quorum (`quorum`) | `all` | the host count | A quorum no host set can meet would refuse every publish | For envelopes and receipts, in both modes: in unicast the hosts that took one, on the plane the hosts that answer it by lookup (when `hosts` and `header_url` are set). Unicast warns below a majority. A sweep always needs every host named |
| Wait for the plane to deliver an object | 3.5 s a host (0.5, 1 and 2 s between lookups) | fixed | The plane delivers in well under a second; a host still without it is offered it directly. Fewer hosts than the quorum answering it leaves it in the outbox | Plane only |
| Tries per submission | 4, waiting 1, 2 and 4 s | fixed | A host that fails four tries is reported missed; the object stays persisted and the next command resends it | Plane: the facade (a sweep also to each other host named); unicast: each host on its own |
| Request timeout (`timeout`) | 15 s | 1 s to 5 min | Bounds every request, so one host holds an object up for at most four timeouts and 7 s | Same |
| Envelopes per answer page | 64 | 64 (spec) | Bounds an answer | Same |
| Pages a client reads per list | until an answer is short | 64 pages (4096 envelopes), 128 outputs taken from one page, 256 MiB held from one host's walk | Each page is a lookup and up to 64 verifications. A page over the contract's is cut, and a host whose full page does not move past its cursor is asked no further; either is reported as truncated | Same |
| One lookup answer | as answered | 16 MiB | A full page of the largest envelopes fits several times over; a larger answer is an error, not read | Same |
| Receipts per `receipt` answer, sweeps per `sweep` answer | 8 | 8 (spec) | One is the normal answer | Same |
| Envelopes a `list -fill` copies | every one a host lacks | the listing's (4096 a host) | Each copy is one submit and one lookup at the host that lacks it | Off the plane, or a host a unicast publisher missed; on the plane a host that was down is repaired by the plane |
| Acknowledgements per receipt | every envelope named in one `ack` | 64 (the record bound) | One receipt carrier per 64 envelopes rather than one each | Same |

### Retracting

| Limit | Default | Hard cap | Why | Plane and unicast |
| --- | --- | --- | --- | --- |
| Sweeps in flight per home | one | one | A sweep is persisted, settled and waited for before the next is built, so no output is swept twice. One the leg did not take, or refused while its inputs are still its own, stays in flight and the next `drop` finishes it first; one an input of which the node shows spent by another transaction is marked failed and frees the next | Same |
| Outputs the chain already shows spent | left out of the sweep; the spender, once mined, is taken as their sweep | none | A sweep naming a spent output is refused whole, for good. One node question an output | Same |
| Tries to build a sweep an input of which another transaction took | 3 | fixed | The fee coin, or an output swept since the node was asked | Same |
| Wait for a sweep to mine | 10 min | fixed | A drop waits for one block, about 30 s on a test chain and 10 min on a busy main chain; past the wait the sweep stays in flight | Same |
| Wait for a fee coin | up to the same 10 min, only when every coin is change not yet mined | fixed | A tree minted ahead can take the last proven coin; its change becomes spendable when it mines, and the drop waits for a block anyway | Same |

### Coins

| Limit | Default | Hard cap | Why | Plane and unicast |
| --- | --- | --- | --- | --- |
| Coin taken by a run that stopped before it recorded the spend | found by the next command from the pool the state recorded, and put back when the node shows it unspent | none | The pool and the state are two files; the state records the pool before a coin leaves it | Same |
| Coin taken while a funding tree was minted, when the run stopped after the tree was sent and before it was recorded | reported as spent by a transaction the home does not record; the tree's outputs are not recovered by the tooling | open | The tree is built and sent inside bcommon's `producer.Trees`, which gives the caller no hook between signing and sending. The coin is spent on the chain, by a tree this home holds no record of | Same |

### Paying and settling

| Limit | Default | Hard cap | Why | Plane and unicast |
| --- | --- | --- | --- | --- |
| Payments one coin can make to one host | one | none | Two unbroadcast payments spending one coin both verify and only one can mine: the host refuses the second (409) | Not on the plane |
| `bbox history` refused for its session's budget (429) | waits as the host says, 1 to 30 s, and asks once more, while it has paid nothing | one retry | A second refusal ends the command, exit 3. A 429 on the request that carried the payment is not asked again (that would take a second payment): the payment is kept as made, exit 3 | Not on the plane |
| Payments for one priced question | one | one | The SDK's client pays every 402 again, up to three times; the command refuses a second payment, and a host that asks for one is refused (exit 1) | Not on the plane |
| A payment that was sent and not answered | kept as made: recorded before it is sent, broadcast from here, its coin never returned to the pool | fixed | The host holds a valid transaction that spends the coin; a coin given back would be spent twice | Not on the plane |
| What one command pays in all (`-budget`) | 16000 sat | the flag | `history -all` pays one question a page; the pages and prices are the host's, their sum the command's | Not on the plane |
| Price the client pays for one priced question (`-max-sats`) | 1000 sat | the flag | A price is per question (one page); a host that asks more is asked by the user, never paid by default | Not on the plane: the terms route is HTTP |
| Payments one `payee settle` has broadcast and not yet mined (`-in-flight`) | 16 | 1 to 64 | All are broadcast before any is waited for, so a run takes about one block; each in flight is a proof poll against the node until its block | Not on the plane |
| Wait for a payment to mine | 10 min | fixed | Past it the payment is `NOT SETTLED` and tried again next run. A payment whose input is spent elsewhere is refused at once, recorded, and never tried again | Not on the plane |

### Hosts

| Limit | Default | Hard cap | Why | Plane and unicast |
| --- | --- | --- | --- | --- |
| BEEF bound (`BBOX_MAX_BEEF`) | 262144 bytes | at least 262144 (a floor, never lowered) | A carrier at the content bound with a 1000-output tree fits | Same |
| Retention of what a host no longer answers (`BBOX_RETENTION_DAYS`) | 31 days from first sight | at least 31 (spec section 8.4) | A receipt must outlive every envelope it names; open envelopes are always kept until they leave the 30-day answer window. The `history` classes are what longer retention sells | Same |
| Superseded carriers per funding output | 8 (64 once its winner is acknowledged) | spec | Bounds what one funding output can make a host keep | Same |
| Outpoint rows (`outpoints.jsonl`) | kept for the host's life | none | It is what keeps a funding output from paying for a second answered carrier (spec section 8.2); back it up ([host.md](host.md)) | Same |
| Carrier lines held in memory | every carrier ever admitted, about 1 KB of memory and 400 bytes of journal an envelope (a receipt of 8 acknowledgements about 2 KB) | none: not bounded. 1,000,000 carriers are about 1 to 2 GB of memory | The winner of a funding output is decided over every carrier ever admitted on it, so every one is kept. `bbox_carrier_lines` counts them; size Node's heap for it | Same |
| BRC-104 sessions the terms route keeps (`BBOX_SESSIONS`, `BBOX_SESSION_TTL`) | 10000, each forgotten after 600 s idle | host policy | A handshake is unauthenticated: the store must not grow without end. Past the cap the least recently used goes (`bbox_sessions_evicted_total{why="cap"}`) and its client shakes hands again | Not on the plane |
| BRC-104 handshakes the terms route answers (`BBOX_HANDSHAKES_PER_SEC`, `BBOX_HANDSHAKE_BURST`, `BBOX_HANDSHAKES_PER_ADDR_PER_SEC`, `BBOX_HANDSHAKE_ADDR_BURST`) | 4 a second, 8 at once; per remote address (IPv6 per /64) 1 a second, 4 at once | host policy; over it answered 429 with `Retry-After`, before any signature | The session cap bounds memory, not CPU: each handshake is a signature on the host's own process, which signs about 10 a second on a test host. The defaults leave most of it to the host's own work (`bbox_handshakes_total{result}`) | Not on the plane |
| Signed responses one BRC-104 session is given (`BBOX_RESPONSES_PER_SEC`, `BBOX_RESPONSE_BURST`) | 5 a second, 20 at once | host policy; over it answered 429 with `Retry-After` and no signature | Every authenticated answer is a signature on the host's own process: one handshake must not buy them without end. A reader walking pages asks far fewer (`bbox_requests_total{result}`) | Not on the plane |
| Authenticated requests remembered, to refuse one sent twice | the last 65536 | fixed | A replay is refused 401 before any signature; the session budget bounds an older one | Not on the plane |
| Lookup rate a host accepts | host policy | host policy; answers 429 with `Retry-After` | Resolution and lookup are an unauthenticated surface | Same |

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

**Settling.** A payment is worth nothing until it mines, so settle waits
for blocks, not for requests. Broadcasting every payment first and then
waiting for all of them makes a run one block long whatever its size; the
bound of 16 in flight (64 at most) keeps the number of proof polls against
the node small. On a test chain with a block about every 30 s, two
payments waited for one after the other took about 58 s; together, about
one block.

**The terms route.** A BRC-104 handshake costs the host a signature, on the
host's own process. On a test host a flood of priced questions from fresh
identities was answered at about 10 a second, and a few of the flooding
clients timed out waiting; the plane feed into the same host recorded no retry or error meanwhile. The session cap
holds memory at 10000 sessions whatever the flood; it does not bound the
work. The handshake budget does: a flood past it is answered 429 before
any signature, at the cost of reading a request line, and an honest client,
which shakes hands once per session, still pays its 402 and is answered.
The default of 4 handshakes a second is under half of what the test host
signed.

