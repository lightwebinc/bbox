/**
 * The terms route over real HTTP: the terms document, free questions with
 * and without BRC-104, a priced question answered 402 and then answered once
 * paid by the SDK's own AuthFetch client, and every refusal of a payment.
 */
import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync } from 'node:fs'
import type { AddressInfo } from 'node:net'
import type { Server } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { Engine } from '@bsv/overlay'
import {
  AuthFetch,
  P2PKH,
  PrivateKey,
  ProtoWallet,
  PublicKey,
  Random,
  Transaction,
  Utils,
  createNonce,
  type WalletInterface,
} from '@bsv/sdk'
import { MemoryJournal } from './journal.js'
import { bboxModule } from './module.js'
import { LedgerReceiver, LookupFront, MemoryReceiver, parsePrices, termsDocument, type ReceivedPayment } from './paid.js'
import { PaymentProtocol } from './payment.js'
import { Chain, Party, PayingWallet, carrier, commitment, envelopeRecord, fromNowhere, fundingTree, receiptRecord } from './testmint.js'
import { FakeHost, MemoryStorage, quietConsole } from './testutil.js'

const office = 'example_office_qzxkvbmwtr'
const topic = `tm_bbox_${office}`
const payee = PrivateKey.fromHex('11'.repeat(32))
const payeeWallet = new ProtoWallet(payee)
const payeeKey = payee.toPublicKey().toString()

interface Rig {
  url: string
  server: Server
  receiver: MemoryReceiver
  wallet: PayingWallet
  client: Party
  chain: Chain
  env: string
}

const servers: Server[] = []
after(() => {
  for (const s of servers) s.close()
})

/** A module with one envelope, engine and storage, and its terms route on a free port. */
async function rig(prices: string): Promise<Rig> {
  const host = new FakeHost()
  const chain = new Chain(11000)
  const m = bboxModule(host, { offices: [office], stateDir: '/nonexistent', retentionDays: 31, maxBEEF: 262144, prices: parsePrices(prices) }, [topic], new MemoryJournal())
  const storage = new MemoryStorage()
  await m.ls.restore([], storage)
  const engine = new Engine(m.module.topics as never, m.module.lookups as never, storage as never, chain.tracker, undefined, [], [], undefined, undefined, { [topic]: false }, false, undefined, undefined, undefined, quietConsole())
  const alice = new Party('alice')
  const client = new Party('client')
  const tree = fundingTree(alice, 2, chain)
  const now = Math.floor(Date.now() / 1000)
  // One open envelope, and one the client has acknowledged: history answers it.
  const open = await carrier(tree, 0, alice, await envelopeRecord({ office, from: alice, to: client, box: 'inbox', created: now - 60 }))
  const read = await carrier(tree, 1, alice, await envelopeRecord({ office, from: alice, to: client, box: 'inbox', created: now - 120 }))
  const rc = await carrier(fundingTree(client, 1, chain), 0, client, receiptRecord(office, client, [commitment(read)], now))
  for (const x of [open, read, rc]) await engine.submit({ beef: x.toAtomicBEEF(), topics: [topic] })
  const receiver = new MemoryReceiver()
  const front = new LookupFront({ ls: m.ls, host, prices: parsePrices(prices), wallet: payeeWallet, receiver, headers: chain.tracker })
  const server = await front.listen(0, '127.0.0.1')
  servers.push(server)
  return { url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`, server, receiver, wallet: new PayingWallet(client.key, chain), client, chain, env: read.id('hex') }
}

const post = (body: unknown, headers: Record<string, string> = {}): { method: string; headers: Record<string, string>; body: string } => ({
  method: 'POST',
  headers: { 'content-type': 'application/json', ...headers },
  body: JSON.stringify(body),
})

const txids = (a: { outputs: Array<{ beef: number[] }> }): string[] => a.outputs.map((o) => Transaction.fromBEEF(o.beef).id('hex'))

/** AuthFetch prints each payment attempt; the tests read the responses instead. */
async function quietly<T>(f: () => Promise<T>): Promise<T> {
  const saved = { warn: console.warn, info: console.info, error: console.error }
  console.warn = () => {}
  console.info = () => {}
  console.error = () => {}
  try {
    return await f()
  } finally {
    Object.assign(console, saved)
  }
}

test('prices: priceable classes only, once each, 0 to 2^53 - 1; the terms document lists them in class order', () => {
  assert.deepEqual([...parsePrices('history-after=7, history=5')], [['history-after', 7], ['history', 5]])
  assert.deepEqual(parsePrices(undefined).size, 0)
  for (const bad of ['inbox=1', 'history', 'history=-1', 'history=01', 'history=1.5', 'history=1,history=2', 'nosuch=1', 'history=9007199254740992']) {
    assert.throws(() => parsePrices(bad), bad)
  }
  assert.deepEqual(termsDocument(parsePrices('history-after=7,history=5')), {
    service: 'ls_bbox',
    terms: 1,
    classes: [
      { class: 'history', satoshis: 5 },
      { class: 'history-after', satoshis: 7 },
    ],
  })
})

test('the terms document, free questions without authentication, and refusals as errors', async () => {
  const r = await rig('history=5,history-after=5')
  const terms = await fetch(`${r.url}/ls_bbox/terms`)
  assert.equal(terms.status, 200)
  assert.deepEqual(await terms.json(), termsDocument(parsePrices('history=5,history-after=5')))
  const free = await fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, to: r.client.hex } }))
  assert.equal(free.status, 200)
  assert.equal(txids((await free.json()) as never).length, 1)
  for (const [q, status] of [
    [{ office: 'other_office_abcdefghij', to: r.client.hex }, 400],
    [{ office, to: r.client.hex, extra: 'x' }, 400],
  ] as const) {
    assert.equal((await fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: q }))).status, status)
  }
  assert.equal((await fetch(`${r.url}/lookup`, { method: 'POST', body: 'nope' })).status, 400)
  assert.equal((await fetch(`${r.url}/nosuch`)).status, 404)
  const priced = await fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, history: r.client.hex } }))
  assert.equal(priced.status, 401, 'a priced class is asked over BRC-104')
  assert.equal((await fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, to: r.client.hex } }, { 'x-bsv-auth-nonce': 'x' }))).status, 400)

  const none = await rig('')
  assert.equal((await fetch(`${none.url}/ls_bbox/terms`)).status, 404, 'a host that prices nothing serves no terms')
  const unpriced = await fetch(`${none.url}/lookup`, post({ service: 'ls_bbox', query: { office, history: none.client.hex } }))
  assert.equal(unpriced.status, 200)
  assert.deepEqual(txids((await unpriced.json()) as never), [none.env])
})

test('over BRC-104: a free question is answered and signed; a priced one is answered 402, paid, and answered', async () => {
  const r = await rig('history=5')
  const af = new AuthFetch(r.wallet as unknown as WalletInterface)
  const free = await af.fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, to: r.client.hex } }))
  assert.equal(free.status, 200)
  assert.equal(free.headers.get('x-bsv-auth-identity-key'), payeeKey, 'signed by the payee')
  const paid = await quietly(async () => await af.fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, history: r.client.hex } })))
  assert.equal(paid.status, 200)
  assert.equal(paid.headers.get('x-bsv-payment-satoshis-paid'), '5')
  assert.deepEqual(txids((await paid.json()) as never), [r.env])
  assert.equal(r.receiver.payments.length, 1)
  const p = r.receiver.payments[0]!
  assert.equal(p.senderIdentityKey, r.client.hex)
  assert.equal(p.class, 'history')
  assert.equal(p.satoshis, 5)
  // What the payee's wallet internalizes: output 0 pays the key it derives for the remittance.
  const { publicKey } = await payeeWallet.getPublicKey({ protocolID: PaymentProtocol, keyID: `${p.derivationPrefix} ${p.derivationSuffix}`, counterparty: p.senderIdentityKey, forSelf: true })
  const tx = Transaction.fromAtomicBEEF(p.beef)
  assert.equal(tx.outputs[0]!.lockingScript.toHex(), new P2PKH().lock(PublicKey.fromString(publicKey).toAddress()).toHex())
  // A second question is a second payment.
  const again = await quietly(async () => await af.fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, history: r.client.hex } })))
  assert.equal(again.status, 200)
  assert.equal(r.receiver.payments.length, 2)
})

test('a payment is refused unless the prefix is the server\'s, output 0 pays the price to the derived key, and it verifies; it buys one question', async () => {
  const r = await rig('history=5')
  const af = new AuthFetch(r.wallet as unknown as WalletInterface)
  const ask = async (payment: string): Promise<{ status: number; code?: string }> => {
    const res = await af.fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, history: r.client.hex } }, { 'x-bsv-payment': payment }))
    const body = (await res.json()) as { code?: string }
    return { status: res.status, code: body.code }
  }
  const prefix = await createNonce(payeeWallet as unknown as WalletInterface)
  const suffix = Utils.toBase64(Random(32))
  const derived = async (counterparty: string): Promise<string> => {
    const { publicKey } = await r.wallet.getPublicKey({ protocolID: PaymentProtocol, keyID: `${prefix} ${suffix}`, counterparty })
    return new P2PKH().lock(PublicKey.fromString(publicKey).toAddress()).toHex()
  }
  const header = (tx: Transaction, pre = prefix): string => JSON.stringify({ derivationPrefix: pre, derivationSuffix: suffix, transaction: Utils.toBase64(tx.toAtomicBEEF()) })
  const good = await r.wallet.pay([{ satoshis: 5, lockingScript: await derived(payeeKey) }])

  assert.deepEqual(await ask('not json'), { status: 400, code: 'ERR_MALFORMED_PAYMENT' })
  assert.deepEqual(await ask(JSON.stringify({ derivationPrefix: prefix, derivationSuffix: suffix })), { status: 400, code: 'ERR_MALFORMED_PAYMENT' })
  assert.deepEqual(await ask(header(good, Utils.toBase64(Random(32)))), { status: 400, code: 'ERR_INVALID_DERIVATION_PREFIX' })
  assert.deepEqual(await ask(header(await r.wallet.pay([{ satoshis: 4, lockingScript: await derived(payeeKey) }]))), { status: 400, code: 'ERR_INVALID_PAYMENT' })
  const other = PrivateKey.fromRandom().toPublicKey().toString()
  assert.deepEqual(await ask(header(await r.wallet.pay([{ satoshis: 5, lockingScript: await derived(other) }]))), { status: 400, code: 'ERR_INVALID_PAYMENT' })
  // Paid from a coin whose block the host's headers do not hold.
  const stray = fromNowhere([{ satoshis: 100000, lockingScript: new P2PKH().lock(r.client.key.toAddress()) }])
  new Chain(99000).mine(stray)
  assert.deepEqual(await ask(header(await r.wallet.pay([{ satoshis: 5, lockingScript: await derived(payeeKey) }], stray))), { status: 400, code: 'ERR_PAYMENT_SPV' })
  assert.equal(r.receiver.payments.length, 0, 'nothing refused was taken')
  assert.equal((await ask(header(good))).status, 200)
  assert.deepEqual(await ask(header(good)), { status: 409, code: 'ERR_PAYMENT_REPLAYED' })
  assert.equal(r.receiver.payments.length, 1)
})

test('the payment ledger keeps every accepted payment across a restart and refuses its txids again', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bbox-ledger-'))
  try {
    const p: ReceivedPayment = { txid: 'ab'.repeat(32), beef: [1, 1, 1, 1], outputIndex: 0, satoshis: 5, derivationPrefix: 'AA==', derivationSuffix: 'AQ==', senderIdentityKey: payeeKey, class: 'history' }
    const a = new LedgerReceiver(dir)
    assert.equal(a.claim(p), true)
    assert.equal(a.claim(p), false)
    a.close()
    const b = new LedgerReceiver(dir)
    assert.equal(b.claim(p), false, 'claimed before the restart')
    assert.equal(b.claim({ ...p, txid: 'cd'.repeat(32) }), true)
    b.close()
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})
