/**
 * What the recipient does with an envelope it has read, spec sections 4.5,
 * 10 and 13: open the BRC-78 message through its own wallet, check the
 * plaintext, and check a payment before it internalizes it. The twin of the
 * Go internal/boxrec/payment.go. A host never sees the plaintext, so none
 * of these is a host rule.
 */
import { Beef, type ChainTracker, type Transaction, type WalletInterface } from '@bsv/sdk'
import { Refusal, type Envelope } from './boxrec.js'
import { BRC78Min, parseCanonicalObject, type Content } from './content.js'
import { JObject } from './jcs.js'
import { p2pkh } from './script.js'
import { bytesEq, fromHex, isLowerHex, strictBase64, toBase64, toHex } from './util.js'

/** BRC-29's derivation protocol. */
export const PaymentProtocol: [2, string] = [2, '3241645161d8']

/** The least expires - created of an envelope whose payment a recipient takes. */
export const PaymentMinLife = 7200
/** How long before expires a recipient stops internalizing. */
export const PaymentMargin = 3600

/**
 * The most references a plaintext carries (spec section 4.5): 32 with a key
 * and a locator of a hundred bytes take about 8 KB, which fits the
 * plaintext a 16 KiB content can hold with room for a body.
 */
export const MaxRefs = 32

/** The bound on a payment's BEEF: it rides inside the content. */
const MaxPaymentBEEF = 16 << 10

export interface Ref {
  url: string
  sha256: Uint8Array
  length: number
  key?: Uint8Array
}

export interface PaymentOutput {
  outputIndex: number
  derivationSuffix: string
  satoshis: number
}

export interface Payment {
  /** The payment transaction as Atomic BEEF (BRC-95). */
  beef: Uint8Array
  derivationPrefix: string
  outputs: PaymentOutput[]
}

export interface Plaintext {
  doc: JObject
  body?: string
  payment?: Payment
  refs?: Ref[]
}

type Wallet = Pick<WalletInterface, 'decrypt' | 'getPublicKey'>

/**
 * Decrypts an envelope's BRC-78 message through the recipient's wallet:
 * protocol [2, "message encryption"], the message's key id in base64,
 * counterparty the sender. A failure is undecryptable, never empty.
 */
export async function open(w: Wallet, c: Content, from: Uint8Array): Promise<Uint8Array> {
  if (c.cipher.length < BRC78Min) throw new Refusal('undecryptable')
  try {
    const r = await w.decrypt({
      protocolID: [2, 'message encryption'],
      keyID: toBase64(c.cipher.subarray(70, 102)),
      counterparty: toHex(from),
      ciphertext: Array.from(c.cipher.subarray(102)),
    })
    return Uint8Array.from(r.plaintext)
  } catch (e) {
    throw new Refusal('undecryptable', (e as Error).message)
  }
}

const safeInt = (v: unknown): v is number => typeof v === 'number' && Number.isSafeInteger(v)

function parseRefs(v: unknown): Ref[] {
  if (!Array.isArray(v)) throw new Refusal('plaintext-shape', 'refs')
  if (v.length > MaxRefs) throw new Refusal('plaintext-shape', `${v.length} refs, at most ${MaxRefs}`)
  return v.map((e, i) => {
    if (!(e instanceof JObject)) throw new Refusal('plaintext-shape', `refs[${i}]`)
    const url = e.get('url')
    const digest = e.get('sha256')
    const length = e.get('length')
    if (typeof url !== 'string' || !(url.startsWith('https://') || url.startsWith('uhrp://')) || typeof digest !== 'string' || !isLowerHex(digest, 64) || !safeInt(length) || length < 0) {
      throw new Refusal('plaintext-shape', `refs[${i}]`)
    }
    const r: Ref = { url, sha256: fromHex(digest), length }
    if (e.has('key')) {
      const k = e.get('key')
      if (typeof k !== 'string' || !isLowerHex(k, 64)) throw new Refusal('plaintext-shape', `refs[${i}].key`)
      r.key = fromHex(k)
    }
    return r
  })
}

function parsePayment(v: unknown): Payment {
  if (!(v instanceof JObject)) throw new Refusal('payment-shape', 'not an object')
  const b64 = v.get('beef')
  const prefix = v.get('derivationPrefix')
  const outs = v.get('outputs')
  const beef = typeof b64 === 'string' ? strictBase64(b64) : undefined
  if (beef === undefined || typeof prefix !== 'string' || strictBase64(prefix) === undefined || !Array.isArray(outs) || outs.length === 0) {
    throw new Refusal('payment-shape', 'beef, derivationPrefix or outputs')
  }
  const seen = new Set<number>()
  const outputs = outs.map((e, i) => {
    if (!(e instanceof JObject)) throw new Refusal('payment-shape', `outputs[${i}]`)
    const idx = e.get('outputIndex')
    const suffix = e.get('derivationSuffix')
    const sats = e.get('satoshis')
    if (!safeInt(idx) || idx < 0 || idx > 0xffffffff || typeof suffix !== 'string' || strictBase64(suffix) === undefined || !safeInt(sats) || sats < 1) {
      throw new Refusal('payment-shape', `outputs[${i}]`)
    }
    if (seen.has(idx)) throw new Refusal('payment-shape', `output ${idx} listed twice`)
    seen.add(idx)
    return { outputIndex: idx, derivationSuffix: suffix, satoshis: sats }
  })
  return { beef, derivationPrefix: prefix, outputs }
}

/**
 * The plaintext's rules in order: plaintext-json, plaintext-shape (body,
 * refs), payment-shape. Unknown members are kept in doc and ignored.
 */
export function parsePlaintext(b: Uint8Array): Plaintext {
  const doc = parseCanonicalObject(b)
  if (doc === undefined) throw new Refusal('plaintext-json')
  const p: Plaintext = { doc }
  if (doc.has('body')) {
    const body = doc.get('body')
    if (typeof body !== 'string') throw new Refusal('plaintext-shape', 'body')
    p.body = body
  }
  if (doc.has('refs')) p.refs = parseRefs(doc.get('refs'))
  if (doc.has('payment')) p.payment = parsePayment(doc.get('payment'))
  return p
}

/**
 * What a recipient checks before it internalizes a payment from envelope e
 * at its time now, in order: payment-expires, payment-late, payment-beef,
 * payment-output (each listed output exists, holds the satoshis listed and
 * is P2PKH to the key the recipient's wallet derives under BRC-29 for the
 * prefix, the suffix and the sender), payment-spv. Returns the payment
 * transaction, for the wallet's internalizeAction.
 */
export async function checkPayment(w: Wallet, e: Envelope, p: Payment, headers: ChainTracker, now: number): Promise<Transaction> {
  if (e.expires === 0 || e.expires < e.created + PaymentMinLife) throw new Refusal('payment-expires')
  if (now + PaymentMargin >= e.expires) throw new Refusal('payment-late')
  const b = p.beef
  if (b.length < 4 || b[0] !== 0x01 || b[1] !== 0x01 || b[2] !== 0x01 || b[3] !== 0x01) throw new Refusal('payment-beef', 'not Atomic BEEF')
  let tx: Transaction | undefined
  try {
    if (b.length > MaxPaymentBEEF) throw new Error('over the bound')
    const beef = Beef.fromBinaryView(Uint8Array.from(b))
    if (beef.txs.some((t) => t.tx !== undefined && t.tx.inputs.length === 0)) throw new Error('a transaction of no inputs')
    tx = beef.findAtomicTransaction(beef.atomicTxid!)
  } catch (err) {
    throw new Refusal('payment-beef', (err as Error).message)
  }
  if (tx === undefined) throw new Refusal('payment-beef', 'no subject')
  for (const o of p.outputs) {
    const out = tx.outputs[o.outputIndex]
    if (out === undefined) throw new Refusal('payment-output', `output ${o.outputIndex} of ${tx.outputs.length}`)
    if ((out.satoshis ?? 0) !== o.satoshis) throw new Refusal('payment-output', `output ${o.outputIndex} holds ${out.satoshis}`)
    const { publicKey } = await w.getPublicKey({
      protocolID: PaymentProtocol,
      keyID: `${p.derivationPrefix} ${o.derivationSuffix}`,
      counterparty: toHex(e.from),
      forSelf: true,
    })
    if (!bytesEq(Uint8Array.from(out.lockingScript.toBinary()), p2pkh(fromHex(publicKey)))) {
      throw new Refusal('payment-output', `output ${o.outputIndex} does not pay the recipient's key`)
    }
  }
  let ok = false
  try {
    ok = await tx.verify(headers)
  } catch {
    ok = false
  }
  if (!ok) throw new Refusal('payment-spv')
  return tx
}
