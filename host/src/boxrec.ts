/**
 * The byte-level contract of bbox, the twin of the Go package
 * internal/boxrec: the envelope record an envelope carrier holds, the
 * receipt record a receipt carrier holds, the office and box name grammars,
 * and the classifier that says which record an output claims. docs/spec.md
 * is the normative text; the vectors under testdata/vectors hold both codecs
 * to the same bytes.
 *
 * Every record is canonical CBOR through @lightwebinc/bcommon, so one value
 * has one encoding a reader accepts. Every decoder bounds its input before
 * the CBOR decoder runs. Decoding follows the refusal order of spec section
 * 3.1 step for step, so both codecs refuse a record for the same reason.
 */
import { CborError, CborMap, bytesEqual, decodeValue, encode, strictPublicKey, type Pair, type Value } from '@lightwebinc/bcommon'

/** The BRC-43 protocol, security level 1, and the key ids under it. */
export const ProtocolName = 'bbox message'
export const Protocol: [1, string] = [1, ProtocolName]
export const KeyEnvelope = 'envelope'
export const KeySignature = 'signature'
export const KeyFund = 'fund'

/** The overlay names (BRC-87). */
export const TopicPrefix = 'tm_bbox_'
export const LookupService = 'ls_bbox'

/** Tags and magics. The tag prefix "bb" belongs to bbox. */
export const TagFunding = Uint8Array.of(0x62, 0x62, 0x02)
export const MagicEnvelope = Uint8Array.of(0x62, 0x62, 0x65, 0x01)
export const MagicReceipt = Uint8Array.of(0x62, 0x62, 0x72, 0x01)

/** Bounds. A reader accepts every record within them. */
export const MaxSafe = Number.MAX_SAFE_INTEGER
export const MaxTime = 253402300799
export const SuffixLen = 10
export const MaxOffice = 50 - TopicPrefix.length
export const MaxOfficeName = MaxOffice - 1 - SuffixLen
export const MinOffice = 1 + 1 + SuffixLen
export const MaxBox = 50
export const MaxContent = 16 << 10
export const MaxEnvelopeRecord = MaxContent + 4096
export const MaxAcks = 64
export const MaxReceiptRecord = 4096
export const MaxKeys = 64

/** Envelope record keys. */
export const EK = { magic: 0, office: 1, to: 2, box: 3, from: 4, created: 5, expires: 6, content: 7 } as const
const ekLast = 7n
/** Receipt record keys. */
export const RK = { magic: 0, office: 1, by: 2, acks: 3, created: 4 } as const
const rkLast = 4n

/**
 * The fixed labels a host counts refusals by: the record reasons of spec
 * section 3.1, the content reasons of section 4.6, the transaction reasons
 * of section 8.1, and the recipient's reasons for what it decrypts.
 */
export type Reason =
  | 'too-large'
  | 'cbor'
  | 'key-type'
  | 'magic'
  | 'missing'
  | 'type'
  | 'range'
  | 'identity'
  | 'office'
  | 'box'
  | 'expires'
  | 'acks'
  | 'content-json'
  | 'content-shape'
  | 'content-cipher'
  | 'content-signature'
  | 'query'
  | 'carrier-shape'
  | 'beef'
  | 'mineable'
  | 'unlock'
  | 'lock'
  | 'signature'
  | 'funding'
  | 'unmined'
  | 'not-bbox'
  | 'undecryptable'
  | 'plaintext-json'
  | 'plaintext-shape'
  | 'payment-shape'
  | 'payment-expires'
  | 'payment-late'
  | 'payment-beef'
  | 'payment-output'
  | 'payment-spv'

/** A refusal: the reason label, and a detail for people. */
export class Refusal extends Error {
  constructor(
    readonly reason: Reason,
    detail?: string,
  ) {
    super(detail === undefined ? `boxrec: ${reason}` : `boxrec: ${reason}: ${detail}`)
    this.name = 'Refusal'
  }
}

/**
 * Refuses anything but a canonical compressed secp256k1 key (bcommon
 * strictPublicKey): 33 bytes, prefix 0x02 or 0x03, an x-coordinate below
 * the field prime, and a point on the curve.
 */
export function checkIdentity(k: Uint8Array): void {
  if (strictPublicKey(k) === undefined) throw new Refusal('identity')
}

/**
 * The readable part's grammar: 1 to max bytes of lowercase ASCII letters
 * and single underscores, starting and ending with a letter; digits also
 * admits 0 to 9 after the first byte.
 */
function checkName(s: string, max: number, digits: boolean, fail: Reason): void {
  if (s.length === 0 || s.length > max) throw new Refusal(fail)
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i)
    if (c >= 0x61 && c <= 0x7a) continue
    if (digits && c >= 0x30 && c <= 0x39 && i > 0) continue
    if (c === 0x5f && i !== 0 && i !== s.length - 1 && s.charCodeAt(i - 1) !== 0x5f) continue
    throw new Refusal(fail)
  }
}

/** The grammar of an office's readable part. */
export function checkOfficeName(name: string): void {
  checkName(name, MaxOfficeName, false, 'office')
}

/**
 * The office identifier grammar: <name>_<suffix>, where name meets
 * checkOfficeName and suffix is exactly SuffixLen lowercase ASCII letters.
 * Every character the grammar allows is one byte, so a string of other
 * characters fails whatever its length is counted in.
 */
export function checkOffice(s: string): void {
  if (s.length < MinOffice || s.length > MaxOffice) throw new Refusal('office')
  const cut = s.length - SuffixLen - 1
  if (s.charCodeAt(cut) !== 0x5f) throw new Refusal('office')
  for (let i = cut + 1; i < s.length; i++) {
    const c = s.charCodeAt(i)
    if (c < 0x61 || c > 0x7a) throw new Refusal('office')
  }
  checkOfficeName(s.slice(0, cut))
}

/** An office identifier's readable part and suffix. */
export function splitOffice(s: string): { name: string; suffix: string } {
  checkOffice(s)
  const cut = s.length - SuffixLen - 1
  return { name: s.slice(0, cut), suffix: s.slice(cut + 1) }
}

/**
 * Creates an office identifier for name: name, an underscore, and SuffixLen
 * letters drawn uniformly at random from a to z. random(n) returns n random
 * bytes (crypto.getRandomValues when omitted). The draw is the Go codec's:
 * bytes are read 16 at a time and a byte of 234 or more (234 = 9 * 26) is
 * skipped, so every letter is equally likely and the same bytes give the
 * same suffix in both languages.
 */
export function newOffice(name: string, random?: (n: number) => Uint8Array): string {
  checkOfficeName(name)
  const draw = random ?? ((n: number) => crypto.getRandomValues(new Uint8Array(n)))
  let suffix = ''
  while (suffix.length < SuffixLen) {
    const buf = draw(16)
    if (buf.length !== 16) throw new Error('boxrec: short random read')
    for (const c of buf) {
      if (c < 234 && suffix.length < SuffixLen) suffix += String.fromCharCode(0x61 + (c % 26))
    }
  }
  return `${name}_${suffix}`
}

/** The topic manager name of an office identifier. */
export function topic(office: string): string {
  checkOffice(office)
  return TopicPrefix + office
}

/** The box name grammar. */
export function checkBox(s: string): void {
  checkName(s, MaxBox, true, 'box')
}

/** Keys a record carries by number, and the ones above the last defined. */
interface Fields {
  known: Map<number, Value>
  extra: Pair[]
}

/** Refusal steps 1 to 3: the bound, one canonical CBOR map, at most MaxKeys entries. */
function decodeMap(b: Uint8Array, max: number): CborMap {
  if (b.length > max) throw new Refusal('too-large')
  let v: Value
  try {
    v = decodeValue(b)
  } catch (e) {
    if (e instanceof CborError) throw new Refusal('cbor', e.message)
    throw e
  }
  if (!(v instanceof CborMap)) throw new Refusal('cbor', 'not a map')
  if (v.entries.length > MaxKeys) throw new Refusal('too-large', `more than ${MaxKeys} keys`)
  return v
}

/** Step 4: every key an unsigned integer; those above last are preserved. */
function intKeys(m: CborMap, last: bigint): Fields {
  const known = new Map<number, Value>()
  const extra: Pair[] = []
  for (const p of m.entries) {
    if (typeof p.key !== 'bigint' || p.key < 0n) throw new Refusal('key-type')
    if (p.key > last) {
      extra.push(p)
      continue
    }
    known.set(Number(p.key), p.val)
  }
  return { known, extra }
}

/** Step 5. */
function checkMagic(f: Fields, magic: Uint8Array): void {
  const b = f.known.get(0)
  if (!(b instanceof Uint8Array) || !bytesEqual(b, magic)) throw new Refusal('magic')
}

function present(f: Fields, k: number): Value {
  const v = f.known.get(k)
  if (v === undefined) throw new Refusal('missing', `key ${k}`)
  return v
}

function needBytes(f: Fields, k: number): Uint8Array {
  const v = present(f, k)
  if (!(v instanceof Uint8Array)) throw new Refusal('type', `key ${k}`)
  return v
}

function needText(f: Fields, k: number): string {
  const v = present(f, k)
  if (typeof v !== 'string') throw new Refusal('type', `key ${k}`)
  return v
}

function needUint(f: Fields, k: number, lo: number, hi: number): number {
  const v = present(f, k)
  if (typeof v !== 'bigint' || v < 0n) throw new Refusal('type', `key ${k}`)
  if (v < BigInt(lo) || v > BigInt(hi)) throw new Refusal('range', `key ${k}`)
  return Number(v)
}

function inRange(n: number, lo: number, hi: number, what: string): void {
  if (!Number.isSafeInteger(n) || n < lo || n > hi) throw new Refusal('range', what)
}

/** Preserved keys sit above the known ones, so re-encoding cannot shadow one. */
function checkExtra(extra: Pair[], last: bigint): void {
  for (const p of extra) {
    if (typeof p.key !== 'bigint' || p.key <= last) throw new Refusal('key-type')
  }
}

/** One envelope record: the payload of one envelope carrier. */
export interface Envelope {
  office: string
  /** The recipient's identity key. */
  to: Uint8Array
  box: string
  /** The sender's identity key. */
  from: Uint8Array
  created: number
  /** 0 means no expiry of the sender's own. */
  expires: number
  /** The BRC-169 envelope, JSON, as carried. */
  content: Uint8Array
  /** Integer keys above 7, preserved verbatim and ignored. */
  extra: Pair[]
}

/** Step 7. */
function envelopeCross(e: Envelope): void {
  if (e.expires !== 0 && e.expires <= e.created) throw new Refusal('expires')
}

/** Every rule that needs only the record, in the refusal order (steps 6 and 7). */
export function validateEnvelope(e: Envelope): void {
  checkExtra(e.extra, ekLast)
  if (e.extra.length + 8 > MaxKeys) throw new Refusal('too-large', `more than ${MaxKeys} keys`)
  checkOffice(e.office)
  checkIdentity(e.to)
  checkBox(e.box)
  checkIdentity(e.from)
  inRange(e.created, 1, MaxTime, 'created')
  inRange(e.expires, 0, MaxTime, 'expires')
  inRange(e.content.length, 1, MaxContent, 'content')
  envelopeCross(e)
}

export function encodeEnvelope(e: Envelope): Uint8Array {
  validateEnvelope(e)
  const out = encode(
    new CborMap([
      { key: BigInt(EK.magic), val: MagicEnvelope },
      { key: BigInt(EK.office), val: e.office },
      { key: BigInt(EK.to), val: e.to },
      { key: BigInt(EK.box), val: e.box },
      { key: BigInt(EK.from), val: e.from },
      { key: BigInt(EK.created), val: BigInt(e.created) },
      { key: BigInt(EK.expires), val: BigInt(e.expires) },
      { key: BigInt(EK.content), val: e.content },
      ...e.extra,
    ]),
  )
  if (out.length > MaxEnvelopeRecord) throw new Refusal('too-large')
  return out
}

/** Parses an envelope record in the refusal order of spec section 3.1. */
export function decodeEnvelope(b: Uint8Array): Envelope {
  const f = intKeys(decodeMap(b, MaxEnvelopeRecord), ekLast)
  checkMagic(f, MagicEnvelope)
  const office = needText(f, EK.office)
  checkOffice(office)
  const to = needBytes(f, EK.to)
  checkIdentity(to)
  const box = needText(f, EK.box)
  checkBox(box)
  const from = needBytes(f, EK.from)
  checkIdentity(from)
  const created = needUint(f, EK.created, 1, MaxTime)
  const expires = needUint(f, EK.expires, 0, MaxTime)
  const content = needBytes(f, EK.content)
  inRange(content.length, 1, MaxContent, 'content')
  const e: Envelope = { office, to, box, from, created, expires, content, extra: f.extra }
  envelopeCross(e)
  return e
}

/** One receipt record: the payload of one receipt carrier. */
export interface Receipt {
  office: string
  /** The acknowledging recipient's identity key. */
  by: Uint8Array
  /** Envelope commitments, hash byte order, strictly ascending. */
  acks: Uint8Array[]
  created: number
  /** Integer keys above 4, preserved verbatim and ignored. */
  extra: Pair[]
}

function compare(a: Uint8Array, b: Uint8Array): number {
  for (let i = 0; i < Math.min(a.length, b.length); i++) if (a[i] !== b[i]) return a[i]! - b[i]!
  return a.length - b.length
}

/** 1 to MaxAcks commitments of 32 bytes in strictly ascending byte order. */
function checkAcks(acks: Uint8Array[]): void {
  if (acks.length < 1 || acks.length > MaxAcks) throw new Refusal('acks')
  for (const [i, a] of acks.entries()) {
    if (a.length !== 32) throw new Refusal('acks')
    if (i > 0 && compare(acks[i - 1]!, a) >= 0) throw new Refusal('acks')
  }
}

export function validateReceipt(r: Receipt): void {
  checkExtra(r.extra, rkLast)
  if (r.extra.length + 5 > MaxKeys) throw new Refusal('too-large', `more than ${MaxKeys} keys`)
  checkOffice(r.office)
  checkIdentity(r.by)
  checkAcks(r.acks)
  inRange(r.created, 1, MaxTime, 'created')
}

export function encodeReceipt(r: Receipt): Uint8Array {
  validateReceipt(r)
  const out = encode(
    new CborMap([
      { key: BigInt(RK.magic), val: MagicReceipt },
      { key: BigInt(RK.office), val: r.office },
      { key: BigInt(RK.by), val: r.by },
      { key: BigInt(RK.acks), val: r.acks.map((a) => Uint8Array.from(a)) },
      { key: BigInt(RK.created), val: BigInt(r.created) },
      ...r.extra,
    ]),
  )
  if (out.length > MaxReceiptRecord) throw new Refusal('too-large')
  return out
}

/** Parses a receipt record in the refusal order. */
export function decodeReceipt(b: Uint8Array): Receipt {
  const f = intKeys(decodeMap(b, MaxReceiptRecord), rkLast)
  checkMagic(f, MagicReceipt)
  const office = needText(f, RK.office)
  checkOffice(office)
  const by = needBytes(f, RK.by)
  checkIdentity(by)
  const v = present(f, RK.acks)
  if (!Array.isArray(v)) throw new Refusal('type', `key ${RK.acks}`)
  if (v.length < 1 || v.length > MaxAcks) throw new Refusal('acks')
  const acks: Uint8Array[] = []
  for (const a of v) {
    if (!(a instanceof Uint8Array) || a.length !== 32) throw new Refusal('acks')
    acks.push(a)
  }
  checkAcks(acks)
  const created = needUint(f, RK.created, 1, MaxTime)
  return { office, by, acks, created, extra: f.extra }
}

/** What an output claims to carry. */
export type Kind = 'none' | 'envelope' | 'receipt'

/**
 * A definite-length map head (0xa0 to 0xbb), key 0, then a four-byte
 * string equal to magic. Key 0 always sorts first in a canonical map with
 * unsigned-integer keys.
 */
function claims(p: Uint8Array, magic: Uint8Array): boolean {
  if (p.length < 1 || p[0]! >> 5 !== 5) return false
  const ai = p[0]! & 0x1f
  let i: number
  if (ai < 24) i = 1
  else if (ai <= 27) i = 1 + (1 << (ai - 24))
  else return false
  if (p.length < i + 2 + magic.length || p[i] !== 0x00 || p[i + 1] !== 0x44) return false
  return bytesEqual(p.subarray(i + 2, i + 2 + magic.length), magic)
}

export const isEnvelope = (p: Uint8Array): boolean => claims(p, MagicEnvelope)
export const isReceipt = (p: Uint8Array): boolean => claims(p, MagicReceipt)

/** What a PushDrop's first field claims to be. */
export function claimOfField(p: Uint8Array): Kind {
  if (isEnvelope(p)) return 'envelope'
  if (isReceipt(p)) return 'receipt'
  return 'none'
}

/**
 * The push at s[i]: an opcode 0x01 to 0x4b pushing that many bytes, or
 * OP_PUSHDATA1, 2 or 4 with a little-endian length, every byte present.
 */
export function firstPush(s: Uint8Array, i: number): Uint8Array | undefined {
  if (i >= s.length) return undefined
  const op = s[i++]!
  let n: number
  if (op >= 0x01 && op <= 0x4b) n = op
  else if (op === 0x4c && i + 1 <= s.length) n = s[i++]!
  else if (op === 0x4d && i + 2 <= s.length) {
    n = s[i]! | (s[i + 1]! << 8)
    i += 2
  } else if (op === 0x4e && i + 4 <= s.length) {
    n = (s[i]! | (s[i + 1]! << 8) | (s[i + 2]! << 16)) + s[i + 3]! * 0x1000000
    if (n > s.length) return undefined
    i += 4
  } else return undefined
  if (n > s.length - i) return undefined
  return s.subarray(i, i + n)
}

/**
 * The admission classifier over raw locking-script bytes (spec section
 * 8.1): a 33-byte push, OP_CHECKSIG, then one push whose data a record's
 * claim prefix begins. Nothing else about the script is read here.
 */
export function claimOf(s: Uint8Array): Kind {
  if (s.length < 35 || s[0] !== 0x21 || s[34] !== 0xac) return 'none'
  const f = firstPush(s, 35)
  return f === undefined ? 'none' : claimOfField(f)
}
