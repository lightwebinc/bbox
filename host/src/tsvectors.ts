/**
 * The vectors the TypeScript codec writes, for the Go codec to read: the
 * other direction of the cross-language check. Every input is fixed, so a
 * run writes the same bytes every time; tsvectors.test.ts regenerates the
 * file and compares it byte for byte, and the Go tests decode, check and
 * rebuild each value.
 *
 *   node dist/tsvectors.js          # write testdata/ts/ts-v1.json
 *   node dist/tsvectors.js -check   # compare, write nothing
 */
import { createCipheriv } from 'node:crypto'
import { readFileSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import {
  Hash,
  LockingScript,
  MerklePath,
  P2PKH,
  PrivateKey,
  CompletedProtoWallet,
  ProtoWallet,
  PushDrop,
  Transaction,
  UnlockingScript,
  type PublicKey,
} from '@bsv/sdk'
import { CborMap, LockTime, readerLockingKey } from '@lightwebinc/bcommon'
import { KeyEnvelope, KeySignature, Protocol, encodeEnvelope, encodeReceipt, newOffice, type Envelope } from './boxrec.js'
import { BRC78Version, rfc3339 } from './content.js'
import { JObject, canonical, type JValue } from './jcs.js'
import { PaymentProtocol } from './payment.js'
import { fundingScript, pushDropScript } from './script.js'
import { concat, fromHex, toBase64, toHex } from './util.js'

export const file = new URL('../../testdata/ts/ts-v1.json', import.meta.url)

const text = new TextEncoder()
const sha = (s: string): Uint8Array => Uint8Array.from(Hash.sha256(Array.from(text.encode(s))))

/** The public test keys: 32 bytes of 0x42 (sender) and of 0x43 (recipient). */
const senderKey = PrivateKey.fromHex('42'.repeat(32))
const recipientKey = PrivateKey.fromHex('43'.repeat(32))
const sender = senderKey.toPublicKey()
const recipient = recipientKey.toPublicKey()
const senderWallet = new ProtoWallet(senderKey)

const Office = 'example_office_qzxkvbmwtr'
const t0 = 1767225600

const compressed = (k: PublicKey): Uint8Array => Uint8Array.from(k.encode(true) as number[])

/**
 * A BRC-78 message from the sender to the recipient, with the key id and IV
 * given rather than drawn, so the vector is reproducible: the invoice
 * "2-message encryption-<base64 key id>", the child keys each side derives,
 * their shared secret's x-coordinate as the AES-256-GCM key, and the 32-byte
 * IV and 16-byte tag around the ciphertext.
 */
function brc78(plaintext: Uint8Array, keyID: Uint8Array, iv: Uint8Array): Uint8Array {
  const invoice = `2-message encryption-${toBase64(keyID)}`
  const mine = senderKey.deriveChild(recipient, invoice)
  const theirs = recipient.deriveChild(senderKey, invoice)
  const key = Uint8Array.from(mine.deriveSharedSecret(theirs).encode(true) as number[]).subarray(1)
  const c = createCipheriv('aes-256-gcm', key, iv)
  const body = concat([c.update(plaintext), c.final(), c.getAuthTag()])
  return concat([BRC78Version, compressed(sender), compressed(recipient), keyID, iv, body])
}

/** The sender's wallet signature under [1, "bbox message"] key id, counterparty anyone. */
async function sign(keyID: string, data: Uint8Array): Promise<Uint8Array> {
  const r = await senderWallet.createSignature({ data: Array.from(data), protocolID: Protocol, keyID, counterparty: 'anyone' })
  return Uint8Array.from(r.signature)
}

/**
 * Seals plaintext into an envelope record in box, created at created and
 * expiring at expires: the BRC-169 envelope with its claims and extra
 * members, the payload encrypted to the recipient, the signature over the
 * serialization without content and signature, and the record with extra
 * keys.
 */
async function seal(label: string, box: string, created: number, expires: number, plaintext: string, extraDoc: Array<[string, JValue]>, extra: Envelope['extra']) {
  const keyID = sha(`bbox/ts-vector/keyid/${label}`)
  const iv = sha(`bbox/ts-vector/iv/${label}`)
  const message = brc78(text.encode(plaintext), keyID, iv)
  const doc = new JObject()
    .set('metanetHandles', '1.0')
    .set('recipient', new JObject().set('identityKey', recipient.toString()).set('handle', 'rôbin'))
    .set('sender', new JObject().set('identityKey', sender.toString()).set('domain', 'example.org'))
    .set('created', rfc3339(created))
    .set('payment', null)
    .set('content', toBase64(message))
  for (const [k, v] of extraDoc) doc.set(k, v)
  const signed = canonical(doc.without('content', 'signature'))
  doc.set('signature', toHex(await sign(KeySignature, signed)))
  const e: Envelope = {
    office: Office,
    to: compressed(recipient),
    box,
    from: compressed(sender),
    created,
    expires,
    content: canonical(doc),
    extra,
  }
  return { label, plaintext, keyID: toHex(keyID), iv: toHex(iv), signedJson: new TextDecoder().decode(signed), record: encodeEnvelope(e) }
}

/** A block of two at height: the transaction at offset 1 beside a fixed sibling. */
function mine(tx: Transaction, height: number): { height: number; merkleRoot: string } {
  tx.merklePath = new MerklePath(height, [
    [
      { offset: 0, hash: '55'.repeat(32) },
      { offset: 1, hash: tx.id('hex'), txid: true },
    ],
  ])
  return { height, merkleRoot: tx.merklePath.computeRoot(tx.id('hex')) }
}

/** A transaction from nowhere: one unsigned input of a fixed outpoint. */
function fromNowhere(tag: string, outs: Array<{ satoshis: number; lockingScript: LockingScript }>): Transaction {
  return new Transaction(
    1,
    [{ sourceTXID: toHex(sha(`bbox/ts-vector/nowhere/${tag}`)), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff }],
    outs,
    0,
  )
}

export async function generate(): Promise<string> {
  // An office drawn from fixed bytes: the first sixteen hold values the
  // draw skips (234 and above), so the suffix needs a second read.
  const random = Uint8Array.from([
    0xff, 0x00, 0xea, 0x19, 0xfe, 0x1a, 0xeb, 0x33, 0xf0, 0x4e, 0xec, 0x67, 0xfa, 0x80, 0x9c, 0xb5, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09,
    0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
  ])
  let at = 0
  const drawn = newOffice('ts_written', (n) => random.subarray(at, (at += n)))

  // A note whose plaintext and claims carry what JCS escapes and what it
  // writes raw: a quote, a backslash, controls, U+2028, a character outside
  // the BMP, and a name that sorts differently in UTF-16 and UTF-8.
  const notePlain = new TextDecoder().decode(
    canonical(new JObject().set('body', 'Line one\nquote " back \\ tab \t bell \u0007 sep   emoji \u{1F600} end').set('｡', 1).set('\u{1F600}', -2)),
  )
  const note = await seal(
    'note',
    'inbox',
    t0 + 900,
    0,
    notePlain,
    [
      ['threadId', 't-ts'],
      ['nested', [1, -9007199254740991, true, null, new JObject().set('z', 'last').set('a', 'first')]],
    ],
    [
      { key: 9n, val: 'a key this version does not define' },
      { key: 40n, val: [1n, new CborMap([{ key: 'x', val: null }])] },
    ],
  )

  // A receipt of three commitments, ascending, with an unknown key.
  const acks = [1, 2, 3].map((i) => sha(`bbox/ts-vector/commitment/${i}`)).sort((a, b) => Buffer.compare(a, b))
  const receipt = encodeReceipt({ office: Office, by: compressed(recipient), acks, created: t0 + 1800, extra: [{ key: 5n, val: -1n }] })

  // The canonical scripts: a signed record output and a funding output.
  const envKey = Uint8Array.from(readerLockingKey(Protocol, KeyEnvelope, sender.toString()).encode(true) as number[])
  const fieldSig = await sign(KeyEnvelope, note.record)
  const recordScript = pushDropScript(envKey, [note.record], fieldSig)

  // A carrier the TypeScript side builds end to end: a mined funding tree
  // of four outputs from nowhere, and a carrier of the note on output 1,
  // its input signed through the SDK's PushDrop unlocker.
  const headers: Array<{ height: number; merkleRoot: string }> = []
  const tree = fromNowhere('tree', [0, 1, 2, 3].map(() => ({ satoshis: 1000, lockingScript: LockingScript.fromBinary(Array.from(fundingScript(envKey))) })))
  headers.push(mine(tree, 700))
  const unlock = new PushDrop(new CompletedProtoWallet(senderKey)).unlock(Protocol, KeyEnvelope, 'anyone', 'all', false)
  const carrier = new Transaction(
    1,
    [{ sourceTransaction: tree, sourceOutputIndex: 1, unlockingScriptTemplate: unlock, sequence: 0 }],
    [{ satoshis: 1000, lockingScript: LockingScript.fromBinary(Array.from(recordScript)) }],
    LockTime,
  )
  await carrier.sign()

  // A payment in an envelope, built by the TypeScript side: a coin from
  // nowhere paying the sender, spent to two outputs the sender derives for
  // the recipient under BRC-29, and the change.
  const coinKey = PrivateKey.fromHex(toHex(sha('bbox/ts-vector/coin-key')))
  const coin = fromNowhere('coin', [{ satoshis: 20000, lockingScript: new P2PKH().lock(coinKey.toAddress()) }])
  headers.push(mine(coin, 701))
  const prefix = toBase64(sha('bbox/ts-vector/payment/prefix').subarray(0, 16))
  const suffixes = [0, 1].map((i) => toBase64(sha(`bbox/ts-vector/payment/suffix/${i}`).subarray(0, 16)))
  const sats = [700, 1300]
  const outputs = []
  for (const [i, suffix] of suffixes.entries()) {
    const { publicKey } = await senderWallet.getPublicKey({ protocolID: PaymentProtocol, keyID: `${prefix} ${suffix}`, counterparty: recipient.toString() })
    outputs.push({ satoshis: sats[i]!, lockingScript: new P2PKH().lock(Hash.hash160(Array.from(fromHex(publicKey)))) })
  }
  outputs.push({ satoshis: 20000 - 700 - 1300 - 150, lockingScript: new P2PKH().lock(coinKey.toAddress()) })
  const pay = new Transaction(1, [{ sourceTransaction: coin, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(coinKey), sequence: 0xffffffff }], outputs, 0)
  await pay.sign()
  const payPlain = new TextDecoder().decode(
    canonical(
      new JObject().set('body', 'Paid from TypeScript.').set(
        'payment',
        new JObject()
          .set('beef', toBase64(Uint8Array.from(pay.toAtomicBEEF())))
          .set('derivationPrefix', prefix)
          .set(
            'outputs',
            suffixes.map((s, i) => new JObject().set('outputIndex', i).set('derivationSuffix', s).set('satoshis', sats[i]!)),
          ),
      ),
    ),
  )
  const payment = await seal('payment', 'payment_inbox', t0 + 1200, t0 + 1200 + 86400, payPlain, [], [])

  const v = {
    description:
      'Written by the TypeScript codec (host/src/tsvectors.ts) and read by the Go codec: the office draw reproduces from the same random bytes; each envelope and receipt record decodes and re-encodes to the same bytes; each envelope passes the content rules, its signature (made by the TypeScript SDK) verifies and its BRC-78 message (encrypted here with a fixed key id and IV) opens for the recipient; the scripts rebuild identically; the carrier the TypeScript side built is admitted; and the payment the TypeScript side built is accepted by the recipient at now.',
    office: Office,
    officeDraw: { name: 'ts_written', random: toHex(random), office: drawn },
    envelopes: [note, payment].map((s) => ({ ...s, record: toHex(s.record) })),
    receipt: { record: toHex(receipt), acks: acks.map(toHex), extraKeys: [5] },
    recordScript: { key: toHex(envKey), record: toHex(note.record), signature: toHex(fieldSig), script: toHex(recordScript) },
    fundingScript: { key: toHex(envKey), script: toHex(fundingScript(envKey)) },
    headers,
    carrier: { txid: carrier.id('hex'), commitment: toHex(Uint8Array.from(carrier.hash() as number[])), beef: toHex(Uint8Array.from(carrier.toAtomicBEEF())) },
    payment: { now: t0 + 1260, paymentTxid: pay.id('hex'), outputs: suffixes.map((s, i) => ({ outputIndex: i, derivationSuffix: s, satoshis: sats[i] })) },
  }
  return JSON.stringify(v, null, 2) + '\n'
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const out = await generate()
  if (process.argv.includes('-check')) {
    let have = ''
    try {
      have = readFileSync(file, 'utf8')
    } catch {}
    if (have !== out) {
      console.error(`differs: ${fileURLToPath(file)}`)
      process.exit(1)
    }
  } else {
    writeFileSync(file, out)
  }
}
