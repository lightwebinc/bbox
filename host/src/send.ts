/**
 * The sender's builders, the TypeScript twins of the Go send path
 * (boxrec.Seal, SealEnvelope, EncodePlaintext, the BRC-29 payment member and
 * destination of send.Engine.Payment, and the envelope carrier of
 * send.Engine.carrierFor through bcommon's carrier.Mint twin). Every key
 * operation goes through a BRC-100 wallet: createSignature for the envelope
 * signature and the carrier, encrypt for BRC-78, getPublicKey for the
 * payment destination. A page writes exactly what the Go sender writes; the
 * bytes are held to testdata/vectors/envelope-v1.json, payment-v1.json and
 * transaction-v1.json.
 *
 * What the Go engine adds around these (its coin pool, funding trees, the
 * outbox, receipts' bookkeeping) is the caller's: a page asks its wallet to
 * fund trees and payments (createAction).
 */
import { P2PKH, PublicKey, Transaction, Utils, type LockingScript, type WalletInterface } from '@bsv/sdk'
import { fundingLock, mintCarrier, type Derivation } from '@lightwebinc/bcommon'
import { KeyEnvelope, KeySignature, Protocol, TagFunding, encodeEnvelope, validateEnvelope, type Envelope } from './boxrec.js'
import { BRC78Version, MetanetHandles, rfc3339 } from './content.js'
import { JObject, canonical, type JValue } from './jcs.js'
import { PaymentProtocol, parsePlaintext } from './payment.js'
import { concat, toHex } from './util.js'

/** The carrier and funding derivation (boxrec.EnvelopeDerivation) and the signature's (SignatureDerivation). */
export const EnvelopeDerivation: Derivation = { protocol: Protocol, keyID: KeyEnvelope }
export const SignatureDerivation: Derivation = { protocol: Protocol, keyID: KeySignature }
export { TagFunding }

/** BRC-78's protocol: [2, "message encryption"]. */
export const MessageEncryption: [2, string] = [2, 'message encryption']

export type SealWallet = Pick<WalletInterface, 'createSignature' | 'encrypt'>

const b64 = (b: Uint8Array): string => Utils.toBase64(Array.from(b))

/**
 * Completes a BRC-169 envelope (boxrec.Seal): payment null, content the
 * BRC-78 message in standard base64, and the signature under the sender's
 * key for [1, "bbox message"] key id "signature", counterparty anyone, over
 * the RFC 8785 serialization without content and signature. Returns the
 * canonical serialization of the whole envelope.
 */
export async function seal(w: Pick<WalletInterface, 'createSignature'>, originator: string, doc: JObject, brc78: Uint8Array): Promise<Uint8Array> {
  doc.set('payment', null)
  doc.set('content', b64(brc78))
  const signed = canonical(doc.without('content', 'signature'))
  const r = await w.createSignature(
    { data: Array.from(signed), protocolID: SignatureDerivation.protocol, keyID: SignatureDerivation.keyID, counterparty: 'anyone' },
    originator,
  )
  doc.set('signature', toHex(Uint8Array.from(r.signature)))
  return canonical(doc)
}

/**
 * Encrypts plaintext to the recipient under BRC-78 through the sender's
 * wallet ([2, "message encryption"], a random key id) and seals the
 * envelope (boxrec.SealEnvelope). keyID is drawn at random unless given (a
 * vector's fixed stand-in). Returns the content an envelope record carries.
 */
export async function sealEnvelope(
  w: SealWallet,
  originator: string,
  from: Uint8Array,
  to: Uint8Array,
  plaintext: Uint8Array,
  created: number,
  keyID: Uint8Array = crypto.getRandomValues(new Uint8Array(32)),
): Promise<Uint8Array> {
  if (keyID.length !== 32) throw new Error('send: a key id is 32 bytes')
  const recipient = PublicKey.fromString(toHex(to))
  const res = await w.encrypt(
    { plaintext: Array.from(plaintext), protocolID: MessageEncryption, keyID: b64(keyID), counterparty: recipient.toString() },
    originator,
  )
  const brc78 = concat([BRC78Version, from, to, keyID, Uint8Array.from(res.ciphertext)])
  const party = (k: Uint8Array): JObject => new JObject().set('identityKey', toHex(k))
  const doc = new JObject().set('metanetHandles', MetanetHandles).set('recipient', party(to)).set('sender', party(from)).set('created', rfc3339(created))
  return await seal(w, originator, doc, brc78)
}

/** A plaintext (boxrec.EncodePlaintext): its canonical serialization, held to the recipient's rules before it is returned. */
export function encodePlaintext(doc: JObject): Uint8Array {
  const b = canonical(doc)
  parsePlaintext(b)
  return b
}

/** An envelope record (key 7 the sealed content), validated as a host validates it. */
export function envelopeRecord(e: Envelope): Uint8Array {
  validateEnvelope(e)
  return encodeEnvelope(e)
}

/** A funding-tree output's script for the sender's bbox trees (carrier.FundingLock under the envelope derivation). */
export async function envelopeFundingLock(w: Pick<WalletInterface, 'getPublicKey' | 'createSignature'>, originator: string): Promise<LockingScript> {
  return await fundingLock(w, originator, EnvelopeDerivation, TagFunding)
}

/** The carrier of an envelope or receipt record on output vout of a bbox funding tree (send.Engine.carrierFor's carrier.Mint). */
export async function envelopeCarrier(w: Pick<WalletInterface, 'getPublicKey' | 'createSignature'>, originator: string, record: Uint8Array, tree: Transaction, vout: number): Promise<Transaction> {
  return await mintCarrier(w, originator, EnvelopeDerivation, record, tree, vout)
}

/** BRC-29's key id for a prefix and a suffix (bwallet.PaymentKeyID). */
export const paymentKeyID = (prefix: string, suffix: string): string => `${prefix} ${suffix}`

/**
 * The payment's destination (bwallet.Signer.PaymentDestination): P2PKH of
 * the key the sender's wallet derives for the recipient under BRC-29
 * ([2, "3241645161d8"], key id "<prefix> <suffix>", counterparty the
 * recipient, not for self).
 */
export async function paymentDestination(w: Pick<WalletInterface, 'getPublicKey'>, originator: string, recipient: Uint8Array, prefix: string, suffix: string, mainnet = false): Promise<LockingScript> {
  const r = await w.getPublicKey({ protocolID: PaymentProtocol, keyID: paymentKeyID(prefix, suffix), counterparty: toHex(recipient), forSelf: false }, originator)
  return new P2PKH().lock(PublicKey.fromString(r.publicKey).toAddress(mainnet ? 'mainnet' : 'testnet'))
}

/** A BRC-29 derivation token: 16 random bytes, standard padded base64. */
export const paymentToken = (): string => b64(crypto.getRandomValues(new Uint8Array(16)))

/**
 * The plaintext's payment member (send.Engine.Payment): the payment's
 * Atomic BEEF in base64, the derivation prefix, and its one output at
 * index outputIndex with its suffix and satoshis.
 */
export function paymentMember(atomicBEEF: Uint8Array, prefix: string, suffix: string, sats: number, outputIndex = 0): JObject {
  if (!Number.isSafeInteger(sats) || sats <= 0) throw new Error('send: a payment pays at least one satoshi')
  const out = new JObject().set('outputIndex', outputIndex).set('derivationSuffix', suffix).set('satoshis', sats)
  return new JObject().set('beef', b64(atomicBEEF)).set('derivationPrefix', prefix).set('outputs', [out] as JValue[])
}
