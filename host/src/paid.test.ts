/**
 * The terms route over real HTTP: the terms document, free questions with
 * and without BRC-104, a priced question answered 402 and then answered once
 * paid by the SDK's own AuthFetch client, and every refusal of a payment.
 */
import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import type { AddressInfo } from 'node:net'
import { createServer, request, type IncomingHttpHeaders, type Server } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { Engine } from '@bsv/overlay'
import { defaultAcceptancePolicy, type AcceptancePolicy } from '@lightwebinc/bcommon'
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
  type CreateActionArgs,
  type CreateActionResult,
  type PeerSession,
  type WalletInterface,
} from '@bsv/sdk'
import { MemoryJournal } from './journal.js'
import { bboxModule } from './module.js'
import {
  ArcadeHttp,
  BoundedSessions,
  BroadcastRefused,
  DefaultBudget,
  DefaultResponseBudget,
  LedgerReceiver as SharedLedger,
  LookupFront,
  MemoryReceiver,
  PaymentGate,
  isFinal,
  overspends,
  spentOutpoints,
  type BudgetConfig,
  type ReceivedPayment,
  type ResponseBudgetConfig,
} from '@lightwebinc/bcommon/host'
import { Ledger, bboxRoute, parsePrices, termsDocument } from './paid.js'
import { PaymentProtocol } from './payment.js'
import { TestNetwork } from '@lightwebinc/bcommon/testing'
import { Chain, Party, PayingWallet, carrier, commitment, envelopeRecord, fromNowhere, fundingTree, receiptRecord } from './testmint.js'
import { FakeHost, MemoryStorage, quietConsole } from './testutil.js'

/** bbox's ledger: the shared receiver in the layout bbox writes. */
class LedgerReceiver extends SharedLedger {
  constructor(dir: string) {
    super(dir, Ledger)
  }
}

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
  front: LookupFront
  host: FakeHost
  net: TestNetwork
  gate: PaymentGate
  /** Signatures the payee's wallet made. */
  signatures: () => number
}

const servers: Server[] = []
after(() => {
  for (const s of servers) s.close()
})

/** A module with one envelope, engine and storage, and its terms route on a free port. */
async function rig(prices: string, sessions?: { max: number; ttlSeconds: number }, budget?: BudgetConfig, responses?: ResponseBudgetConfig, policy: Partial<AcceptancePolicy> = {}, networked = true): Promise<Rig> {
  const host = new FakeHost()
  const chain = new Chain(11000)
  const m = bboxModule(host, { offices: [office], stateDir: '/nonexistent', retentionDays: 31, maxBEEF: 262144, prices: parsePrices(prices), sessions: { max: 100, ttlSeconds: 600 }, handshakes: DefaultBudget, responses: DefaultResponseBudget }, [topic], new MemoryJournal())
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
  let signatures = 0
  const wallet = new ProtoWallet(payee)
  const sign = wallet.createSignature.bind(wallet)
  wallet.createSignature = async (args) => {
    signatures++
    return await sign(args)
  }
  const net = new TestNetwork(chain)
  const gate = new PaymentGate({ app: 'bbox', host, headers: chain.tracker, policy: { ...defaultAcceptancePolicy(), ...policy }, arcade: networked ? net : undefined, node: networked ? net : undefined, waitMs: 200, pollMs: 10 })
  const front = new LookupFront({ ...bboxRoute(m.ls), host, prices: parsePrices(prices), wallet, receiver, headers: chain.tracker, sessions, budget, responses, gate })
  const server = await front.listen(0, '127.0.0.1')
  servers.push(server)
  return { url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`, server, receiver, wallet: new PayingWallet(client.key, chain), client, chain, env: read.id('hex'), front, host, net, gate, signatures: () => signatures }
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
  // A class and its -after form are priced together: the walk cannot be had page by page for nothing.
  assert.equal((await fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, history: r.client.hex, after: `1:${'00'.repeat(32)}` } }))).status, 401)
  assert.equal((await fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, to: r.client.hex } }, { 'x-bsv-auth-nonce': 'x' }))).status, 400)

  const none = await rig('')
  assert.equal((await fetch(`${none.url}/ls_bbox/terms`)).status, 404, 'a host that prices nothing serves no terms')
  const unpriced = await fetch(`${none.url}/lookup`, post({ service: 'ls_bbox', query: { office, history: none.client.hex } }))
  assert.equal(unpriced.status, 200)
  assert.deepEqual(txids((await unpriced.json()) as never), [none.env])
})

test('over BRC-104: a free question is answered and signed; a priced one is answered 402, paid, and answered', async () => {
  const r = await rig('history=5,history-after=5')
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
  const r = await rig('history=5,history-after=5')
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

test('the payment ledger keeps every accepted payment across a restart and refuses its txids and its coins again', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bbox-ledger-'))
  try {
    const p: ReceivedPayment = { txid: 'ab'.repeat(32), beef: [1, 1, 1, 1], outputIndex: 0, satoshis: 5, derivationPrefix: 'AA==', derivationSuffix: 'AQ==', senderIdentityKey: payeeKey, class: 'history', inputs: [`${'dd'.repeat(32)}.0`] }
    const a = new LedgerReceiver(dir)
    assert.equal(a.claim(p), 'accepted')
    assert.equal(a.claim(p), 'replayed')
    a.close()
    const b = new LedgerReceiver(dir)
    assert.equal(b.claim(p), 'replayed', 'claimed before the restart')
    assert.equal(b.claim({ ...p, txid: 'cd'.repeat(32) }), 'conflict', 'another transaction spending the coin the first one spent')
    assert.equal(b.claim({ ...p, txid: 'cd'.repeat(32), inputs: [`${'ee'.repeat(32)}.1`] }), 'accepted')
    assert.equal(b.claim({ ...p, txid: 'ef'.repeat(32), inputs: [] }), 'conflict', 'a payment that names no coin is not one')
    b.close()
    const c = new LedgerReceiver(dir)
    assert.equal(c.claim({ ...p, txid: '12'.repeat(32), inputs: [`${'ee'.repeat(32)}.1`, `${'ff'.repeat(32)}.0`] }), 'conflict', 'the coins are remembered across a restart')
    c.close()
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

const session = (nonce: string, key: string, lastUpdate = 0): PeerSession => ({ isAuthenticated: true, sessionNonce: nonce, peerIdentityKey: key, lastUpdate })

test('the BRC-104 session store is bounded: a cap that forgets the least recently used, and an idle life', () => {
  let now = 1_000
  const evicted: string[] = []
  const s = new BoundedSessions(2, 60_000, () => now, (why) => evicted.push(why))
  s.addSession(session('n1', 'k1'))
  s.addSession(session('n2', 'k2'))
  assert.equal(s.getSession('n1')?.sessionNonce, 'n1', 'a lookup touches n1, so n2 is now the least recently used')
  s.addSession(session('n3', 'k3'))
  assert.equal(s.size, 2)
  assert.equal(s.hasSession('n2'), false, 'the least recently used went at the cap')
  assert.deepEqual(evicted, ['cap'])
  assert.equal(s.getSession('k3')?.sessionNonce, 'n3', 'looked up by identity key too')
  s.updateSession(session('n3', 'k3', 5))
  assert.equal(s.size, 2)
  now += 60_000
  assert.equal(s.getSession('n1'), undefined, 'idle for the whole life: forgotten')
  assert.equal(s.getSession('k3'), undefined)
  assert.equal(s.size, 0)
  assert.deepEqual(evicted, ['cap', 'idle', 'idle'])
  s.addSession(session('n4', 'k4'))
  s.removeSession(session('n4', 'k4'))
  assert.equal(s.hasSession('k4'), false)
  assert.throws(() => new BoundedSessions(0, 1), /max/)
  assert.throws(() => s.addSession({ isAuthenticated: false, lastUpdate: 0 }), /sessionNonce/)
})

test('the terms route keeps at most its bound of sessions; a client whose session went shakes hands again', async () => {
  const r = await rig('history=5,history-after=5', { max: 2, ttlSeconds: 600 }, { perSec: 100, burst: 100, perAddressPerSec: 100, addressBurst: 100 })
  const ask = { service: 'ls_bbox', query: { office, to: r.client.hex } }
  const clients = [0, 1, 2].map(() => new AuthFetch(new ProtoWallet(PrivateKey.fromRandom()) as unknown as WalletInterface))
  for (const c of clients) assert.equal((await c.fetch(`${r.url}/lookup`, post(ask))).status, 200)
  assert.equal(r.front.sessions.size, 2, 'three handshakes, two sessions kept')
  assert.equal(r.host.count('bbox_sessions_evicted_total', { why: 'cap' }), 1)
  assert.equal(r.host.gauges.get('bbox_sessions')?.(), 2)
  // The first client's session went: its client finds its request refused
  // and shakes hands again, which the bound admits in place of the oldest.
  const res = await quietly(async () => await clients[0]!.fetch(`${r.url}/lookup`, post(ask)))
  assert.equal(res.status, 200)
  assert.equal(r.host.count('bbox_sessions_evicted_total', { why: 'cap' }), 2, 'its new session took the place of the oldest')
  const again = new AuthFetch(new ProtoWallet(PrivateKey.fromRandom()) as unknown as WalletInterface)
  assert.equal((await again.fetch(`${r.url}/lookup`, post(ask))).status, 200, 'a new handshake is answered')
  assert.equal(r.front.sessions.size, 2)
})

/** One BRC-104 initial request, raw, from a fresh identity, sent from localAddress. */
async function shake(url: string, localAddress: string): Promise<{ status: number; retryAfter?: string }> {
  const body = JSON.stringify({
    version: '0.1',
    messageType: 'initialRequest',
    identityKey: PrivateKey.fromRandom().toPublicKey().toString(),
    initialNonce: Utils.toBase64(Random(32)),
    requestedCertificates: { certifiers: [], types: {} },
  })
  return await new Promise((resolve, reject) => {
    const req = request(`${url}/.well-known/auth`, { method: 'POST', localAddress, headers: { 'content-type': 'application/json', 'content-length': Buffer.byteLength(body) } }, (res) => {
      res.resume()
      res.on('end', () => resolve({ status: res.statusCode ?? 0, retryAfter: res.headers['retry-after'] as string | undefined }))
    })
    req.on('error', reject)
    req.end(body)
  })
}

test('a handshake flood is refused 429 before any signature; a real client still pays its 402 within the budget', async () => {
  const r = await rig('history=5,history-after=5', undefined, { perSec: 0.2, burst: 5, perAddressPerSec: 0.1, addressBurst: 2 })
  // One address floods: its own burst is answered, the rest refused at once.
  const started = Date.now()
  const flood = await Promise.all(Array.from({ length: 50 }, () => shake(r.url, '127.0.0.2')))
  const ok = flood.filter((x) => x.status === 200).length
  const limited = flood.filter((x) => x.status === 429)
  assert.equal(ok, 2, 'the address burst')
  assert.equal(limited.length, 48)
  assert.ok(limited.every((x) => Number(x.retryAfter) >= 1), 'each refusal says when to come back')
  assert.equal(r.signatures(), 2, 'one signature per answered handshake, none for a refused one')
  assert.ok(Date.now() - started < 5000, 'refused cheaply')
  assert.equal(r.host.count('bbox_handshakes_total', { result: 'accepted' }), 2)
  assert.equal(r.host.count('bbox_handshakes_total', { result: 'limited_address' }), 48)
  // Other addresses take the rest of the route's burst, then the route refuses.
  const others = await Promise.all(['127.0.0.3', '127.0.0.3', '127.0.0.4', '127.0.0.5'].map((a) => shake(r.url, a)))
  assert.deepEqual(others.map((x) => x.status).sort(), [200, 200, 200, 429])
  assert.equal(r.host.count('bbox_handshakes_total', { result: 'limited_global' }), 1)
  // The route refills; a real client from its own address shakes hands
  // once, is answered 402, pays, and is answered.
  const wait = Number(others.find((x) => x.status === 429)!.retryAfter)
  assert.ok(wait >= 1 && wait <= 5)
  await new Promise((resolve) => setTimeout(resolve, wait * 1000 + 100))
  const af = new AuthFetch(r.wallet as unknown as WalletInterface)
  const paid = await quietly(async () => await af.fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, history: r.client.hex } })))
  assert.equal(paid.status, 200)
  assert.equal(paid.headers.get('x-bsv-payment-satoshis-paid'), '5')
  assert.deepEqual(txids((await paid.json()) as never), [r.env])
  assert.equal(r.receiver.payments.length, 1)
  assert.equal(r.host.count('bbox_handshakes_total', { result: 'accepted' }), 6, 'the client shook hands once')
  // The route spent that token too: the next handshake waits.
  assert.equal((await shake(r.url, '127.0.0.6')).status, 429)
})

test('a malformed handshake inside the budget is counted failed and costs no signature', async () => {
  const r = await rig('history=5,history-after=5')
  const bad = await fetch(`${r.url}/.well-known/auth`, post({ messageType: 'general' }))
  assert.equal(bad.status, 400)
  assert.equal(r.host.count('bbox_handshakes_total', { result: 'failed' }), 1)
  assert.equal(r.signatures(), 0)
})

test('one coin pays once: conflicting unbroadcast payments that spend it buy one answer, not one each', async () => {
  const r = await rig('history=5,history-after=5')
  // A wallet that pays every question from the same coin: each payment is a
  // different transaction, each verifies, and at most one can ever be mined.
  class OneCoin extends PayingWallet {
    readonly same = this.coin()
    override async createAction(args: CreateActionArgs): Promise<CreateActionResult> {
      const tx = await this.pay((args.outputs ?? []).map((o) => ({ satoshis: o.satoshis, lockingScript: o.lockingScript })), this.same)
      return { tx: tx.toAtomicBEEF(), txid: tx.id('hex') }
    }
  }
  const af = new AuthFetch(new OneCoin(r.client.key, r.chain) as unknown as WalletInterface)
  const statuses: number[] = []
  const codes: Array<string | undefined> = []
  for (let i = 0; i < 4; i++) {
    const res = await quietly(async () => await af.fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, history: r.client.hex } })))
    statuses.push(res.status)
    codes.push(res.status === 200 ? undefined : ((await res.json()) as { code?: string }).code)
  }
  assert.deepEqual(statuses, [200, 409, 409, 409])
  assert.deepEqual(codes, [undefined, 'ERR_PAYMENT_CONFLICT', 'ERR_PAYMENT_CONFLICT', 'ERR_PAYMENT_CONFLICT'])
  assert.equal(r.receiver.payments.length, 1)
  assert.equal(new Set(r.receiver.payments.flatMap((p) => p.inputs)).size, 1)
  assert.equal(r.host.count('bbox_payments_total', { decision: 'refuse', reason: 'conflict' }), 3)
})

test('the ledger reads the coins of a line written before lines carried them, from its transaction', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'bbox-ledger-'))
  try {
    const chain = new Chain(96000)
    const wallet = new PayingWallet(PrivateKey.fromRandom(), chain)
    const coin = wallet.coin()
    const lock = new P2PKH().lock(payee.toAddress()).toHex()
    const first = await wallet.pay([{ satoshis: 5, lockingScript: lock }], coin)
    const second = await wallet.pay([{ satoshis: 6, lockingScript: lock }], coin)
    const line = { txid: first.id('hex'), beef: Utils.toBase64(first.toAtomicBEEF()), outputIndex: 0, satoshis: 5, derivationPrefix: 'AA==', derivationSuffix: 'AQ==', senderIdentityKey: payeeKey, class: 'history', at: 1 }
    writeFileSync(join(dir, 'payments.jsonl'), JSON.stringify(line) + '\n')
    const ledger = new LedgerReceiver(dir)
    const p: ReceivedPayment = { txid: second.id('hex'), beef: second.toAtomicBEEF(), outputIndex: 0, satoshis: 6, derivationPrefix: 'AA==', derivationSuffix: 'AQ==', senderIdentityKey: payeeKey, class: 'history', inputs: spentOutpoints(second) }
    assert.deepEqual(spentOutpoints(second), [`${coin.id('hex')}.0`])
    assert.equal(ledger.claim(p), 'conflict')
    ledger.close()
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

/** A server in front of the route that keeps each request as it arrived, to send it again. */
async function recording(r: Rig): Promise<{ url: string; port: number; requests: Array<{ method: string; url: string; headers: IncomingHttpHeaders; body: Buffer[] }> }> {
  const requests: Array<{ method: string; url: string; headers: IncomingHttpHeaders; body: Buffer[] }> = []
  const server = createServer((req, res) => {
    const body: Buffer[] = []
    requests.push({ method: req.method ?? 'GET', url: req.url ?? '/', headers: { ...req.headers }, body })
    req.on('data', (c: Buffer) => body.push(c))
    r.front.handler(req, res)
  })
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  servers.push(server)
  const port = (server.address() as AddressInfo).port
  return { url: `http://127.0.0.1:${port}`, port, requests }
}

test('a signed request sent again byte for byte is refused before any signature', async () => {
  const r = await rig('history=5,history-after=5')
  const rec = await recording(r)
  const af = new AuthFetch(r.wallet as unknown as WalletInterface)
  assert.equal((await af.fetch(`${rec.url}/lookup`, post({ service: 'ls_bbox', query: { office, to: r.client.hex } }))).status, 200)
  const sent = rec.requests.find((x) => x.url === '/lookup' && x.headers['x-bsv-auth-request-id'] !== undefined)!
  const before = r.signatures()
  const replay = async (): Promise<number> =>
    await new Promise((resolve, reject) => {
      const q = request({ host: '127.0.0.1', port: rec.port, path: sent.url, method: sent.method, headers: sent.headers }, (res) => {
        res.resume()
        res.on('end', () => resolve(res.statusCode ?? 0))
      })
      q.on('error', reject)
      q.end(Buffer.concat(sent.body))
    })
  const statuses = new Set<number>()
  for (let i = 0; i < 50; i++) statuses.add(await replay())
  // And at once, while none of them is yet answered.
  for (const s of await Promise.all(Array.from({ length: 20 }, replay))) statuses.add(s)
  assert.deepEqual([...statuses], [401])
  assert.equal(r.signatures(), before, 'no signature for a replay')
  assert.equal(r.host.count('bbox_requests_total', { result: 'replayed' }), 70)
  // The session itself goes on.
  assert.equal((await af.fetch(`${rec.url}/lookup`, post({ service: 'ls_bbox', query: { office, to: r.client.hex } }))).status, 200)
})

test('a session has a budget of signed responses: over it a request is refused 429 and nothing is signed', async () => {
  const r = await rig('history=5,history-after=5', undefined, undefined, { perSec: 0.5, burst: 3 })
  const af = new AuthFetch(r.wallet as unknown as WalletInterface)
  const ask = async (): Promise<number> => {
    try {
      return (await quietly(async () => await af.fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, to: r.client.hex } })))).status
    } catch {
      // The SDK's client raises on a response the server did not sign.
      return 429
    }
  }
  const statuses: number[] = []
  for (let i = 0; i < 6; i++) statuses.push(await ask())
  assert.deepEqual(statuses, [200, 200, 200, 429, 429, 429])
  assert.equal(r.signatures(), 4, 'the handshake and three responses')
  assert.equal(r.host.count('bbox_requests_total', { result: 'limited' }), 3)
  // Another session has its own budget.
  const other = new AuthFetch(new ProtoWallet(PrivateKey.fromRandom()) as unknown as WalletInterface)
  assert.equal((await other.fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, to: r.client.hex } }))).status, 200)
  // It refills.
  await new Promise((resolve) => setTimeout(resolve, 2100))
  assert.equal(await ask(), 200)
})

test('a class and its -after form are priced together or not at all', () => {
  for (const bad of ['history=5', 'history-after=5', 'history=5,history-after=0', 'history=0,history-after=5']) {
    assert.throws(() => parsePrices(bad), /priced together/, bad)
  }
  for (const good of ['history=5,history-after=6', 'history=0', 'history-after=0', 'history=0,history-after=0']) parsePrices(good)
})

/** Asks one priced question with wallet, paying as AuthFetch does; a held answer (402, no BRC-105 headers) throws in the SDK and is status 0. */
async function asker(r: Rig, who: Party = r.client, wallet: PayingWallet = r.wallet): Promise<() => Promise<number>> {
  const af = new AuthFetch(wallet as unknown as WalletInterface)
  return async () => {
    try {
      return (await quietly(async () => await af.fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, history: who.hex } })))).status
    } catch {
      return 0
    }
  }
}

const last = (r: Rig): ReceivedPayment => r.receiver.payments[r.receiver.payments.length - 1]!

test('acceptance: a small paid lookup is broadcast by the host, then answered; the watch releases it once mined', async () => {
  const r = await rig('history=5,history-after=5')
  const ask = await asker(r)
  assert.equal(await ask(), 200)
  const p = last(r)
  assert.deepEqual(r.net.sent, [p.txid], 'broadcast before the answer')
  assert.equal(p.decision, 'fast')
  assert.equal(r.host.count('bbox_payments_total', { decision: 'fast', reason: 'at-or-below-threshold' }), 1)
  assert.deepEqual(r.gate.watching, [p.txid])
  assert.equal(r.gate.exposure.unmined(r.client.hex).payer, 5)
  assert.deepEqual(await r.gate.sweep(), [], 'nothing to report while unmined')
  r.net.mine(p.txid)
  assert.deepEqual((await r.gate.sweep()).map((e) => e.kind), ['confirmed'])
  assert.deepEqual(r.gate.watching, [])
  assert.equal(r.gate.exposure.unmined(r.client.hex).total, 0)
  assert.equal(r.host.count('bbox_payment_events_total', { kind: 'confirmed' }), 1)
})

test('acceptance: a payment above the threshold is broadcast and held 402 until it mines, then the same payment is answered', async () => {
  const r = await rig('history=5,history-after=5', undefined, undefined, undefined, { thresholdSats: 4 })
  const ask = await asker(r)
  assert.equal(await ask(), 0, 'held: 402 without BRC-105 headers, so the client pays nothing more')
  const p = last(r)
  assert.equal(p.decision, 'hold')
  assert.equal(p.reason, 'above-threshold')
  // AuthFetch sends the request again after a 402 it cannot pay; a held payment offered again is held again, once in the ledger.
  assert.deepEqual([...new Set(r.net.sent)], [p.txid], 'a held payment is broadcast too')
  assert.equal(new Set(r.receiver.payments.map((x) => x.txid)).size, 1)
  assert.ok(r.host.count('bbox_payments_total', { decision: 'hold', reason: 'above-threshold' }) >= 1)
  const header = JSON.stringify({ derivationPrefix: p.derivationPrefix, derivationSuffix: p.derivationSuffix, transaction: Utils.toBase64(Uint8Array.from(p.beef)) })
  const af = new AuthFetch(r.wallet as unknown as WalletInterface)
  const again = async (): Promise<number> => {
    try {
      return (await af.fetch(`${r.url}/lookup`, post({ service: 'ls_bbox', query: { office, history: r.client.hex } }, { 'x-bsv-payment': header }))).status
    } catch {
      return 0
    }
  }
  assert.equal(await again(), 0, 'still held before it mines')
  r.net.mine(p.txid)
  assert.equal(await again(), 200)
  assert.equal(last(r).decision, 'mined')
  assert.equal(await again(), 409, 'and answered once')
})

test('acceptance: arcade answers DOUBLE_SPEND_ATTEMPTED: held, and nothing charged', async () => {
  const r = await rig('history=5,history-after=5')
  r.net.mode = 'double-spend'
  assert.equal(await (await asker(r))(), 0)
  assert.equal(last(r).reason, 'double-spend-attempted')
  assert.ok(r.host.count('bbox_payments_total', { decision: 'hold', reason: 'double-spend-attempted' }) >= 1)
  assert.equal(r.gate.exposure.unmined(r.client.hex).total, 0)
})

test('acceptance: a refusal, an unknown spend view, a silent network and a broadcast that fails', async () => {
  const r = await rig('history=5,history-after=5')
  const ask = await asker(r)
  r.net.mode = 'reject'
  assert.equal(await ask(), 400)
  assert.equal(r.host.count('bbox_payments_total', { decision: 'refuse', reason: 'network-refused' }), 1)
  r.net.mode = 'refuse-http'
  assert.equal(await ask(), 400)
  assert.equal(r.host.count('bbox_payments_total', { decision: 'refuse', reason: 'network-refused' }), 2)
  r.net.mode = 'silent'
  assert.equal(await ask(), 0)
  assert.equal(last(r).reason, 'no-network-verdict')
  r.net.mode = 'down'
  assert.equal(await ask(), 0)
  assert.equal(last(r).reason, 'broadcast-unconfirmed')
  r.net.mode = 'accept'
  r.net.spendUnknown = true
  assert.equal(await ask(), 0)
  assert.equal(last(r).reason, 'spend-view-unknown')
  assert.equal(r.gate.exposure.unmined(r.client.hex).total, 0, 'every demotion released its charge')
})

test('acceptance: a host with no arcade or no node holds every payment', async () => {
  const r = await rig('history=5,history-after=5', undefined, undefined, undefined, {}, false)
  assert.equal(await (await asker(r))(), 0)
  assert.equal(last(r).reason, 'no-broadcast-leg')
  assert.deepEqual(r.net.sent, [])
})

test('acceptance: a payer whose fast payment was double-spent is flagged; its later payments are held, others are not', async () => {
  const r = await rig('history=5,history-after=5')
  const ask = await asker(r)
  assert.equal(await ask(), 200)
  const p = last(r)
  r.net.spendElsewhere(p.inputs[0]!)
  const events = await r.gate.sweep()
  assert.deepEqual(events.map((e) => [e.kind, e.payer]), [['double-spent', r.client.hex]])
  assert.ok(r.host.lines.some((l) => l.msg.includes('fast payment lost')))
  assert.equal(r.host.count('bbox_payment_events_total', { kind: 'double-spent' }), 1)
  assert.equal(await ask(), 0)
  assert.equal(last(r).reason, 'payer-flagged')
  const bob = new Party('bob')
  assert.equal(await (await asker(r, bob, new PayingWallet(bob.key, r.chain)))(), 200, 'another payer is still fast')
})

test('acceptance: fast payments are bounded per payer and in total until they mine', async () => {
  const r = await rig('history=5,history-after=5', undefined, undefined, undefined, { payerLimit: 10, totalLimit: 15 })
  const ask = await asker(r)
  assert.equal(await ask(), 200)
  assert.equal(await ask(), 200)
  assert.equal(await ask(), 0)
  assert.equal(last(r).reason, 'payer-limit')
  const bob = new Party('bob')
  const bobAsk = await asker(r, bob, new PayingWallet(bob.key, r.chain))
  assert.equal(await bobAsk(), 200)
  assert.equal(await bobAsk(), 0)
  assert.equal(last(r).reason, 'total-limit')
  r.net.mine(r.receiver.payments[0]!.txid)
  await r.gate.sweep()
  assert.equal(await bobAsk(), 200, 'a mined payment leaves the sum')
})

test('acceptance: finality and conservation', async () => {
  const chain = new Chain(97000)
  const w = new PayingWallet(PrivateKey.fromRandom(), chain)
  const tx = await w.pay([{ satoshis: 5, lockingScript: new P2PKH().lock(payee.toAddress()).toHex() }])
  assert.equal(isFinal(tx), true)
  assert.equal(overspends(tx), undefined)
  tx.lockTime = 500
  assert.equal(isFinal(tx), true, 'every input final')
  tx.inputs[0]!.sequence = 1
  assert.equal(isFinal(tx), false)
  tx.outputs[0]!.satoshis = 200000
  assert.match(overspends(tx)!, /outputs .* inputs/)
  tx.inputs[0]!.sourceTransaction = undefined
  assert.match(overspends(tx)!, /no source/)
})

test('acceptance: held and fast lines survive a restart; a lost payment is written to unsettleable.jsonl', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'bbox-accept-'))
  try {
    const chain = new Chain(98000)
    const w = new PayingWallet(PrivateKey.fromRandom(), chain)
    const tx = await w.pay([{ satoshis: 5, lockingScript: new P2PKH().lock(payee.toAddress()).toHex() }])
    const base: ReceivedPayment = { txid: tx.id('hex'), beef: tx.toAtomicBEEF(), outputIndex: 0, satoshis: 5, derivationPrefix: 'AA==', derivationSuffix: 'AQ==', senderIdentityKey: payeeKey, class: 'history', inputs: spentOutpoints(tx) }
    const a = new LedgerReceiver(dir)
    assert.equal(a.claim({ ...base, decision: 'hold' }), 'accepted')
    assert.equal(a.claim({ ...base, decision: 'hold' }), 'accepted', 'held again, not written twice')
    a.close()
    const b = new LedgerReceiver(dir)
    assert.equal(b.refusal(base), undefined, 'a held payment can still be answered')
    assert.equal(b.claim({ ...base, decision: 'mined' }), 'accepted')
    assert.equal(b.claim({ ...base, decision: 'mined' }), 'replayed')
    const fast = await w.pay([{ satoshis: 5, lockingScript: new P2PKH().lock(payee.toAddress()).toHex() }])
    assert.equal(b.claim({ ...base, txid: fast.id('hex'), beef: fast.toAtomicBEEF(), inputs: spentOutpoints(fast), decision: 'fast' }), 'accepted')
    b.close()
    const c = new LedgerReceiver(dir)
    assert.equal(c.fast.length, 1)
    c.close()
    const host = new FakeHost()
    const net = new TestNetwork(chain)
    const gate = new PaymentGate({ app: 'bbox', host, headers: chain.tracker, policy: defaultAcceptancePolicy(), arcade: net, node: net, stateDir: dir })
    const f = c.fast[0]!
    gate.restore(Transaction.fromAtomicBEEF(Utils.toArray(f.beef, 'base64')), f.senderIdentityKey, f.satoshis, f.at)
    assert.deepEqual(gate.watching, [fast.id('hex')])
    assert.equal(gate.exposure.unmined(payeeKey).payer, 5)
    net.spendElsewhere(spentOutpoints(fast)[0]!)
    await gate.sweep()
    const lines = readFileSync(join(dir, 'unsettleable.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l) as { kind: string; txid: string })
    assert.deepEqual(lines.map((l) => [l.kind, l.txid]), [['double-spent', fast.id('hex')]])
    assert.equal(gate.exposure.isFlagged(payeeKey), true)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('acceptance: flags outlive a restart, restored lines are watched a day and charged an hour, a partial network still broadcasts, the watch is bounded', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'bbox-accept-'))
  try {
    const chain = new Chain(99500)
    const w = new PayingWallet(PrivateKey.fromRandom(), chain)
    const lock = new P2PKH().lock(payee.toAddress()).toHex()
    writeFileSync(join(dir, 'unsettleable.jsonl'), JSON.stringify({ kind: 'double-spent', txid: 'ab'.repeat(32), payer: '02cd', sats: 5 }) + '\n')
    const host = new FakeHost()
    const net = new TestNetwork(chain)
    const gate = new PaymentGate({ app: 'bbox', host, headers: chain.tracker, policy: defaultAcceptancePolicy(), arcade: net, node: net, stateDir: dir, maxWatched: 2, waitMs: 200, pollMs: 10 })
    assert.equal(gate.exposure.isFlagged('02cd'), true, 'flagged again from unsettleable.jsonl')
    const old = await w.pay([{ satoshis: 5, lockingScript: lock }])
    gate.restore(old, '02ef', 5, Date.now() - 2 * 3_600_000)
    assert.deepEqual(gate.watching, [old.id('hex')], 'watched: under a day old')
    assert.equal(gate.exposure.unmined('02ef').payer, 0, 'not charged: past the window')
    const ancient = await w.pay([{ satoshis: 5, lockingScript: lock }])
    gate.restore(ancient, '02ef', 5, Date.now() - 2 * 86_400_000)
    assert.equal(gate.watching.length, 1, 'over a day old: not watched')
    assert.equal((await gate.accept(await w.pay([{ satoshis: 5, lockingScript: lock }]), '02aa', 5)).decision, 'fast')
    assert.deepEqual(await gate.accept(await w.pay([{ satoshis: 5, lockingScript: lock }]), '02aa', 5), { decision: 'hold', reason: 'watch-limit', detail: 'too many fast payments are waiting to mine' })
    // arcade alone: broadcast, and held, since no node can show it mined.
    const half = new PaymentGate({ app: 'bbox', host: new FakeHost(), headers: chain.tracker, policy: defaultAcceptancePolicy(), arcade: net })
    const p = await w.pay([{ satoshis: 5, lockingScript: lock }])
    assert.equal((await half.accept(p, '02aa', 5)).reason, 'no-broadcast-leg')
    assert.ok(net.sent.includes(p.id('hex')), 'broadcast even so')
    net.mode = 'reject'
    assert.equal((await half.accept(await w.pay([{ satoshis: 5, lockingScript: lock }]), '02aa', 5)).decision, 'refuse')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('acceptance: arcade over HTTP: a refusal code refuses, "already known" is no verdict, "already spent" is never accepted', async () => {
  const replies: Array<[number, string]> = [
    [465, '{"title":"input already spent"}'],
    [409, '{"title":"txn-already-known"}'],
    [400, '{"title":"missing inputs or already spent"}'],
    [200, '{"txid":"x","txStatus":"SEEN_ON_NETWORK"}'],
  ]
  const server = createServer((req, res) => {
    req.resume()
    const [status, body] = replies.shift()!
    res.writeHead(status, { 'content-type': 'application/json' })
    res.end(body)
  })
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r))
  servers.push(server)
  const a = new ArcadeHttp(`http://127.0.0.1:${(server.address() as AddressInfo).port}`)
  await assert.rejects(a.submit(Uint8Array.of(1)), BroadcastRefused)
  assert.deepEqual(await a.submit(Uint8Array.of(1)), { txStatus: 'RECEIVED', extraInfo: 'already known' })
  await assert.rejects(a.submit(Uint8Array.of(1)), (e: unknown) => !(e instanceof BroadcastRefused))
  assert.equal((await a.submit(Uint8Array.of(1))).txStatus, 'SEEN_ON_NETWORK')
})
