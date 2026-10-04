/**
 * An envelope record's content: a BRC-169 section 7.2 envelope, checked
 * against the record in the order of spec section 4.6. The twin of the Go
 * boxrec/content.go.
 */
import { readerLockingKey, verifyFieldSignature } from '@lightwebinc/bcommon'
import { KeySignature, Protocol, Refusal, type Envelope } from './boxrec.js'
import { JObject, JSONSubsetError, canonical, parseJSON, type JValue } from './jcs.js'
import { strictSignature } from './script.js'
import { bytesEq, strictBase64, toHex } from './util.js'

export const MetanetHandles = '1.0'

/** The version bytes a BRC-78 message starts with, as go-sdk writes them. */
export const BRC78Version = Uint8Array.of(0x42, 0x42, 0x10, 0x33)

/** The shortest BRC-78 message: header, key id, IV and tag around nothing. */
export const BRC78Min = 4 + 33 + 33 + 32 + 32 + 16

export interface Content {
  doc: JObject
  /** The RFC 8785 serialization of doc without content and signature. */
  signed: Uint8Array
  /** The BRC-78 message the content member carries. */
  cipher: Uint8Array
  /** The DER signature the signature member carries; empty when it is not lowercase hex. */
  signature: Uint8Array
}

/** created as the one form this contract admits: YYYY-MM-DDTHH:MM:SSZ. */
export function rfc3339(t: number): string {
  return new Date(t * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z')
}

function str(o: JObject, name: string): string | undefined {
  const v = o.get(name)
  return typeof v === 'string' ? v : undefined
}

function optionalString(o: JObject, name: string): boolean {
  return !o.has(name) || typeof o.get(name) === 'string'
}

function checkParty(doc: JObject, name: string, key: Uint8Array, optional: string[]): void {
  const p = doc.get(name)
  if (!(p instanceof JObject)) throw new Refusal('content-shape', name)
  if (str(p, 'identityKey') !== toHex(key)) throw new Refusal('content-shape', `${name}.identityKey`)
  for (const o of optional) if (!optionalString(p, o)) throw new Refusal('content-shape', `${name}.${o}`)
}

/** Parses JSON in the subset and requires its bytes to be the canonical serialization of an object. */
export function parseCanonicalObject(b: Uint8Array): JObject | undefined {
  let v: JValue
  try {
    v = parseJSON(b)
  } catch (e) {
    if (e instanceof JSONSubsetError) return undefined
    throw e
  }
  if (!(v instanceof JObject)) return undefined
  let c: Uint8Array
  try {
    c = canonical(v)
  } catch (e) {
    if (e instanceof JSONSubsetError) return undefined
    throw e
  }
  return bytesEq(c, b) ? v : undefined
}

/**
 * The content rules short of the signature, in order: content-json,
 * content-shape, content-cipher. The record has passed decodeEnvelope.
 */
export function parseContent(e: Envelope): Content {
  const doc = parseCanonicalObject(e.content)
  if (doc === undefined) throw new Refusal('content-json')
  if (str(doc, 'metanetHandles') !== MetanetHandles) throw new Refusal('content-shape', 'metanetHandles')
  checkParty(doc, 'recipient', e.to, ['handle', 'tag', 'domain'])
  checkParty(doc, 'sender', e.from, ['handle', 'domain'])
  if (str(doc, 'created') !== rfc3339(e.created)) throw new Refusal('content-shape', 'created')
  if (!optionalString(doc, 'quoteId')) throw new Refusal('content-shape', 'quoteId')
  if (!doc.has('payment') || doc.get('payment') !== null) throw new Refusal('content-shape', 'payment must be null')
  const b64 = str(doc, 'content')
  if (b64 === undefined) throw new Refusal('content-shape', 'content')
  const sigHex = str(doc, 'signature')
  if (sigHex === undefined) throw new Refusal('content-shape', 'signature')
  const cipher = strictBase64(b64)
  if (cipher === undefined || cipher.length < BRC78Min) throw new Refusal('content-cipher', 'not a BRC-78 message in standard base64')
  if (!bytesEq(cipher.subarray(0, 4), BRC78Version) || !bytesEq(cipher.subarray(4, 37), e.from) || !bytesEq(cipher.subarray(37, 70), e.to)) {
    throw new Refusal('content-cipher', "header does not name the record's sender and recipient")
  }
  const signed = canonical(doc.without('content', 'signature'))
  const signature = /^(?:[0-9a-f]{2})*$/.test(sigHex) ? Uint8Array.from(sigHex.match(/../g) ?? [], (x) => parseInt(x, 16)) : new Uint8Array(0)
  return { doc, signed, cipher, signature }
}

/**
 * The content-signature rule: a strict low-S DER signature over
 * SHA-256(signed) under the sender's key for [1, "bbox message"], key id
 * signature, counterparty anyone.
 */
export function verifyContentSignature(c: Content, from: Uint8Array): void {
  if (!strictSignature(c.signature)) throw new Refusal('content-signature', 'not lowercase hex of a strict low-S DER signature')
  const key = readerLockingKey(Protocol, KeySignature, toHex(from))
  if (!verifyFieldSignature(key, Array.from(c.signed), Array.from(c.signature))) throw new Refusal('content-signature')
}

/** Every content rule in order. */
export function checkContent(e: Envelope): Content {
  const c = parseContent(e)
  verifyContentSignature(c, e.from)
  return c
}
