/**
 * Every Go-generated vector under testdata/vectors, run by the TypeScript
 * codec: each record decodes to the fields the vector lists and re-encodes
 * to the identical bytes, each refusal is refused for the same reason, each
 * question gets the same class, each transaction is admitted or refused
 * exactly as the Go codec did, and each payment is accepted or refused by
 * the recipient exactly as it was there.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'
import { Hash, KeyDeriver, PrivateKey, ProtoWallet, type ChainTracker } from '@bsv/sdk'
import { encode, readerLockingKey } from '@lightwebinc/bcommon'
import {
  KeyEnvelope,
  KeyFund,
  KeySignature,
  LookupService,
  MagicEnvelope,
  MagicReceipt,
  MaxOfficeName,
  Protocol,
  Refusal,
  SuffixLen,
  TagFunding,
  TopicPrefix,
  claimOf,
  claimOfField,
  decodeEnvelope,
  decodeReceipt,
  encodeEnvelope,
  encodeReceipt,
  isEnvelope,
  isReceipt,
  newOffice,
  splitOffice,
  topic,
} from './boxrec.js'
import { admit, type Admission } from './admit.js'
import { checkContent, rfc3339 } from './content.js'
import { canonical, parseJSON } from './jcs.js'
import { checkPayment, open, parsePlaintext } from './payment.js'
import { Classes, parseQuery } from './query.js'
import { fundingScript } from './script.js'
import { bytesEq, displayTxid, fromHex, toHex } from './util.js'

const dir = new URL('../../testdata/vectors/', import.meta.url)
const read = (name: string): string => readFileSync(new URL(name, dir), 'utf8')
const json = <T>(name: string): T => JSON.parse(read(name)) as T
const hexFile = (name: string): string => read(name).trim()

async function reason(f: () => unknown): Promise<string> {
  try {
    await f()
  } catch (e) {
    if (e instanceof Refusal) return e.reason
    throw e
  }
  return 'accepted'
}

const senderKey = PrivateKey.fromHex('42'.repeat(32))
const recipientKey = PrivateKey.fromHex('43'.repeat(32))
const otherKey = PrivateKey.fromHex('44'.repeat(32))

// Every file the Go generator writes is read by a test below.
const covered = new Set([
  'envelope-v1.json',
  'envelope-note.hex',
  'envelope-refs.hex',
  'envelope-64-keys.hex',
  'receipt-v1.json',
  'receipt-two.hex',
  'receipt-max.hex',
  'query-v1.json',
  'json-v1.json',
  'script-v1.json',
  'derivation-v1.json',
  'refusal-v1.json',
  'transaction-v1.json',
  'payment-v1.json',
])

test('every vector file is run', () => {
  for (const f of readdirSync(dir)) assert.ok(covered.has(f), `${f} is not run by the TypeScript tests`)
})

interface EnvelopeVector {
  name: string
  office: string
  topic: string
  to: string
  box: string
  from: string
  created: number
  createdRfc3339: string
  expires: number
  plaintext: string
  brc78KeyId: string
  brc78Iv: string
  brc78: string
  content: string
  signedJson: string
  signedHash: string
  signatureKey: string
  record: string
  recordLength: number
  carrierFieldHash: string
  extra?: Array<{ key: number; valueCbor: string }>
}

test('envelope records decode, re-encode byte for byte, pass the content rules and open for the recipient', async () => {
  const v = json<{ envelopes: EnvelopeVector[] }>('envelope-v1.json')
  assert.equal(v.envelopes.length, 3)
  const w = new ProtoWallet(recipientKey)
  for (const x of v.envelopes) {
    const rec = fromHex(x.record)
    assert.equal(rec.length, x.recordLength)
    assert.equal(hexFile(`${x.name}.hex`), x.record, x.name)
    assert.ok(isEnvelope(rec) && !isReceipt(rec), x.name)
    const e = decodeEnvelope(rec)
    assert.equal(e.office, x.office)
    assert.equal(topic(e.office), x.topic)
    assert.equal(toHex(e.to), x.to)
    assert.equal(e.box, x.box)
    assert.equal(toHex(e.from), x.from)
    assert.equal(e.created, x.created)
    assert.equal(rfc3339(e.created), x.createdRfc3339)
    assert.equal(e.expires, x.expires)
    assert.equal(new TextDecoder().decode(e.content), x.content)
    assert.deepEqual(
      e.extra.map((p) => ({ key: Number(p.key), valueCbor: toHex(encode(p.val)) })),
      x.extra ?? [],
    )
    assert.equal(toHex(encodeEnvelope(e)), x.record, `${x.name} re-encodes`)
    assert.equal(toHex(Hash.sha256(Array.from(rec))), x.carrierFieldHash)
    const c = checkContent(e)
    assert.equal(new TextDecoder().decode(c.signed), x.signedJson)
    assert.equal(toHex(Hash.sha256(Array.from(c.signed))), x.signedHash)
    assert.equal(toHex(c.cipher), x.brc78)
    assert.equal(toHex(c.cipher.subarray(70, 102)), x.brc78KeyId)
    assert.equal(toHex(c.cipher.subarray(102, 134)), x.brc78Iv)
    assert.equal(readerLockingKey(Protocol, KeySignature, x.from).toString(), x.signatureKey)
    const plain = await open(w, c, e.from)
    assert.equal(new TextDecoder().decode(plain), x.plaintext, x.name)
    assert.equal(parsePlaintext(plain).doc.members.length > 0, true)
  }
})

test('receipt records decode and re-encode byte for byte', () => {
  const v = json<{ receipts: Array<{ name: string; office: string; by: string; acks: string[]; acksDisplayTxids: string[]; created: number; record: string; recordLength: number }> }>(
    'receipt-v1.json',
  )
  for (const x of v.receipts) {
    const rec = fromHex(x.record)
    assert.equal(rec.length, x.recordLength)
    assert.equal(hexFile(`${x.name}.hex`), x.record)
    assert.ok(isReceipt(rec) && !isEnvelope(rec))
    const r = decodeReceipt(rec)
    assert.equal(r.office, x.office)
    assert.equal(toHex(r.by), x.by)
    assert.equal(r.created, x.created)
    assert.deepEqual(r.acks.map(toHex), x.acks)
    assert.deepEqual(r.acks.map(displayTxid), x.acksDisplayTxids)
    assert.equal(toHex(encodeReceipt(r)), x.record)
  }
})

test('every question gets the class or the refusal the Go codec gave it', async () => {
  const v = json<{
    classes: Array<{ class: string; members: string[]; free: boolean; page: number }>
    questions: Array<{ name: string; question: string; class?: string; free?: boolean; page?: number; refused?: string }>
  }>('query-v1.json')
  assert.deepEqual(
    v.classes,
    Classes.map((c) => ({ class: c.name, members: c.members, free: c.free, page: c.page })),
  )
  for (const q of v.questions) {
    const obj = JSON.parse(q.question) as Record<string, unknown>
    if (q.refused !== undefined) {
      assert.equal(await reason(() => parseQuery(obj)), q.refused, q.name)
      continue
    }
    const got = parseQuery(obj)
    assert.equal(got.class.name, q.class, q.name)
    assert.equal(got.class.free, q.free)
    assert.equal(got.class.page, q.page)
  }
})

test('the JSON subset, case by case', () => {
  const v = json<{ cases: Array<{ name: string; inputHex: string; accepted: boolean; canonical?: string; isCanonical: boolean }> }>('json-v1.json')
  for (const c of v.cases) {
    const input = fromHex(c.inputHex)
    let out: Uint8Array | undefined
    try {
      out = canonical(parseJSON(input))
    } catch {
      out = undefined
    }
    assert.equal(out !== undefined, c.accepted, c.name)
    if (out === undefined) continue
    assert.equal(new TextDecoder().decode(out), c.canonical, c.name)
    assert.equal(bytesEq(out, input), c.isCanonical, c.name)
  }
})

test('the classifier over raw locking scripts', () => {
  const v = json<{ scripts: Array<{ name: string; script: string; claims: string }> }>('script-v1.json')
  for (const c of v.scripts) assert.equal(claimOf(fromHex(c.script)), c.claims, c.name)
})

test('derivations, tags, magics and office identifiers', () => {
  const v = json<{
    protocol: [number, string]
    fundKeyId: string
    refusedProtocol: { protocol: string }
    identities: { sender: string; recipient: string }
    keys: Array<{ identity: 'sender' | 'recipient'; keyId: string; invoice: string; lockingKey: string }>
    tagFunding: string
    magicEnvelope: string
    magicReceipt: string
    classifierPrefix: { envelope: string; receipt: string }
    offices: Array<{ office: string; name: string; suffix: string; topic: string }>
    drawnOffice: { randomBytes: string; office: string }
    maxOfficeName: number
    suffixLength: number
    lookupService: string
    topicPrefix: string
  }>('derivation-v1.json')
  assert.deepEqual(v.protocol, Protocol)
  assert.equal(v.fundKeyId, KeyFund)
  assert.equal(v.identities.sender, senderKey.toPublicKey().toString())
  assert.equal(v.identities.recipient, recipientKey.toPublicKey().toString())
  for (const k of v.keys) {
    assert.ok(k.keyId === KeyEnvelope || k.keyId === KeySignature)
    assert.equal(k.invoice, `1-${Protocol[1]}-${k.keyId}`)
    assert.equal(readerLockingKey(Protocol, k.keyId, v.identities[k.identity]).toString(), k.lockingKey)
  }
  // The TypeScript SDK refuses the application name as a protocol too.
  assert.throws(() => new KeyDeriver(senderKey).derivePublicKey([1, v.refusedProtocol.protocol], KeyEnvelope, 'anyone', true))
  assert.equal(v.tagFunding, toHex(TagFunding))
  assert.equal(v.magicEnvelope, toHex(MagicEnvelope))
  assert.equal(v.magicReceipt, toHex(MagicReceipt))
  assert.equal(v.classifierPrefix.envelope, '0044' + toHex(MagicEnvelope))
  assert.equal(v.classifierPrefix.receipt, '0044' + toHex(MagicReceipt))
  for (const o of v.offices) {
    assert.deepEqual(splitOffice(o.office), { name: o.name, suffix: o.suffix })
    assert.equal(topic(o.office), o.topic)
  }
  const random = fromHex(v.drawnOffice.randomBytes)
  let at = 0
  assert.equal(
    newOffice('example_office', (n) => random.subarray(at, (at += n))),
    v.drawnOffice.office,
  )
  assert.equal(v.maxOfficeName, MaxOfficeName)
  assert.equal(v.suffixLength, SuffixLen)
  assert.equal(v.lookupService, LookupService)
  assert.equal(v.topicPrefix, TopicPrefix)
})

test('every refusal vector is refused for its reason, and classified as the Go codec does', async () => {
  const v = json<{
    refusals: Array<{ name: string; kind: string; record: string; reason: string; atAdmission: string; admissionClaims: string; admissionReason?: string }>
  }>('refusal-v1.json')
  assert.ok(v.refusals.length >= 60)
  for (const r of v.refusals) {
    const b = fromHex(r.record)
    const got = await reason(() => (r.kind === 'envelope' ? checkContent(decodeEnvelope(b)) : decodeReceipt(b)))
    assert.equal(got, r.reason, r.name)
    const k = claimOfField(b)
    assert.equal(k, r.admissionClaims, r.name)
    assert.equal(k === 'none' ? 'skip' : 'refuse', r.atAdmission, r.name)
    if (k !== 'none') {
      assert.equal(await reason(() => (k === 'envelope' ? checkContent(decodeEnvelope(b)) : decodeReceipt(b))), r.admissionReason, r.name)
    }
  }
})

function chainTracker(headers: Array<{ height: number; merkleRoot: string }>): ChainTracker {
  const roots = new Map(headers.map((h) => [h.height, h.merkleRoot]))
  return {
    isValidRootForHeight: async (root, height) => roots.get(height) === root,
    currentHeight: async () => Math.max(...roots.keys()),
  }
}

interface AdmitsJSON {
  kind: string
  outputsToAdmit: number[]
  coinsToRetain: number[]
  commitment?: string
  record?: string
  owner?: string
  fundingTxid?: string
  fundingVout?: number
  spent?: string[]
}

/** An admission as the Go vectors write it. */
export function admitsJSON(a: Admission): AdmitsJSON {
  const j: AdmitsJSON = { kind: a.kind, outputsToAdmit: a.outputs, coinsToRetain: a.retain }
  if (a.carrier !== undefined) {
    j.commitment = toHex(a.carrier.commitment)
    j.record = toHex(a.carrier.record)
    j.owner = toHex(a.carrier.owner)
    j.fundingTxid = a.carrier.funding.txid
    j.fundingVout = a.carrier.funding.vout
  }
  if (a.spent !== undefined) j.spent = a.spent.map((o) => `${o.txid}.${o.vout}`)
  return j
}

test('every transaction is admitted or refused as the Go codec does', async () => {
  const v = json<{
    office: string
    maxBeef: number
    senderEnvelopeKey: string
    senderFundingScript: string
    headers: Array<{ height: number; merkleRoot: string }>
    transactions: Array<{
      name: string
      txid: string
      rawTx: string
      beef: string
      previousCoins: Array<{ inputIndex: number }>
      verdict: 'admit' | 'refuse'
      reason?: string
      admits?: AdmitsJSON
    }>
  }>('transaction-v1.json')
  assert.equal(toHex(fundingScript(fromHex(v.senderEnvelopeKey))), v.senderFundingScript)
  const headers = chainTracker(v.headers)
  let admitted = 0
  let refused = 0
  for (const t of v.transactions) {
    const held = t.previousCoins.map((c) => c.inputIndex)
    let got: Admission
    try {
      got = await admit(fromHex(t.beef), held, { office: v.office, headers, maxBEEF: v.maxBeef })
    } catch (e) {
      if (!(e instanceof Refusal)) throw e
      assert.equal(t.verdict, 'refuse', `${t.name}: refused ${e.reason}: ${e.message}`)
      assert.equal(e.reason, t.reason, t.name)
      refused++
      continue
    }
    assert.equal(t.verdict, 'admit', `${t.name}: admitted, want ${t.reason}`)
    assert.equal(displayTxid(got.txid), t.txid, t.name)
    assert.deepEqual(admitsJSON(got), t.admits, t.name)
    admitted++
  }
  assert.equal(admitted + refused, v.transactions.length)
  assert.ok(admitted > 0 && refused > 0)
})

test('every payment is accepted or refused by the recipient as the Go codec does', async () => {
  const v = json<{
    headers: Array<{ height: number; merkleRoot: string }>
    payments: Array<{
      name: string
      now: number
      record: string
      plaintext: string
      verdict: 'accept' | 'refuse'
      reason?: string
      paymentTxid?: string
      outputs?: Array<{ outputIndex: number; derivationSuffix: string; satoshis: number; lockingScript: string }>
    }>
  }>('payment-v1.json')
  const headers = chainTracker(v.headers)
  const w = new ProtoWallet(recipientKey)
  let accepted = 0
  for (const p of v.payments) {
    const e = decodeEnvelope(fromHex(p.record))
    const c = checkContent(e)
    const plain = await open(w, c, e.from)
    assert.equal(new TextDecoder().decode(plain), p.plaintext, p.name)
    let got
    try {
      const pt = parsePlaintext(plain)
      const pay = pt.payment
      if (pay === undefined) throw new Error(`${p.name}: no payment`)
      got = { pay, tx: await checkPayment(w, e, pay, headers, p.now) }
    } catch (err) {
      if (!(err instanceof Refusal)) throw err
      assert.equal(p.verdict, 'refuse', `${p.name}: refused ${err.reason}: ${err.message}`)
      assert.equal(err.reason, p.reason, p.name)
      continue
    }
    assert.equal(p.verdict, 'accept', `${p.name}: accepted, want ${p.reason}`)
    assert.equal(got.tx.id('hex'), p.paymentTxid)
    assert.deepEqual(
      got.pay.outputs.map((o) => ({ ...o, lockingScript: got.tx.outputs[o.outputIndex]!.lockingScript.toHex() })),
      p.outputs,
    )
    accepted++
  }
  assert.ok(accepted > 0)
  // Only the recipient opens the payment envelope.
  const first = decodeEnvelope(fromHex(v.payments[0]!.record))
  assert.equal(await reason(() => open(new ProtoWallet(otherKey), checkContent(first), first.from)), 'undecryptable')
})
