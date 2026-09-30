/**
 * The module inside the real overlay engine (the upstream release the
 * reference host's engine is a one-patch fork of, at the same version),
 * over an in-memory store: what the engine records for a refusal, the
 * suppression and out-of-order attacks spec section 8.1 guards against, every
 * golden transaction, answers hydrated by the engine, and restore from the
 * engine's own storage in any order.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Engine } from '@bsv/overlay'
import { Transaction } from '@bsv/sdk'
import type { Module, TopicManager } from '@lightwebinc/bcommon'
import { LockTime } from '@lightwebinc/bcommon'
import { LookupService, topic as topicOf } from './boxrec.js'
import { MemoryJournal } from './journal.js'
import { BboxLookupService } from './ls_bbox.js'
import { bboxModule, type Config } from './module.js'
import { Chain, Party, carrier, commitment, envelopeRecord, fundingTree, receiptRecord, sweep } from './testmint.js'
import { FakeHost, MemoryStorage, beefOf, byName, quietConsole, txVectors } from './testutil.js'
import { Refused } from './tm_bbox.js'
import { toHex } from './util.js'

const v = txVectors()
const topic = v.topic
const office = v.office

function engine(mod: Module, storage: MemoryStorage, chain: Chain): Engine {
  return new Engine(
    mod.topics as never,
    mod.lookups as never,
    storage as never,
    chain.tracker,
    undefined,
    [],
    [],
    undefined,
    undefined,
    { [topic]: false },
    false,
    undefined,
    undefined,
    undefined,
    quietConsole(),
  )
}

const config: Config = { offices: [office], stateDir: '/nonexistent', retentionDays: 31, maxBEEF: 262144, prices: new Map(), sessions: { max: 100, ttlSeconds: 600 } }

/** A module and its engine, with storage handed over as the host does. */
async function setup(opts: { naive?: boolean; journal?: MemoryJournal; chain?: Chain } = {}) {
  const host = new FakeHost()
  const chain = opts.chain ?? new Chain(9000, v.headers)
  const journal = opts.journal ?? new MemoryJournal()
  const m = bboxModule(host, config, [topic], journal)
  const storage = new MemoryStorage()
  await m.ls.restore([], storage)
  let calls = 0
  const tm = m.module.topics![topic]!
  const counting: TopicManager = {
    identifyAdmissibleOutputs: async (beef, previousCoins) => {
      calls++
      try {
        return await tm.identifyAdmissibleOutputs(beef, previousCoins)
      } catch (e) {
        // A manager that returned every refusal as empty instructions, as a
        // manager that does not raise would.
        if (opts.naive === true && e instanceof Refused) return { outputsToAdmit: [], coinsToRetain: [...previousCoins] }
        throw e
      }
    },
    getDocumentation: tm.getDocumentation.bind(tm),
    getMetaData: tm.getMetaData.bind(tm),
  }
  m.module.topics![topic] = counting
  return { host, ls: m.ls, storage, journal, chain, engine: engine(m.module, storage, chain), calls: () => calls }
}

async function submit(e: Engine, beef: number[]): Promise<number[] | undefined> {
  const steak = await e.submit({ beef, topics: [topic] })
  return steak[topic]?.outputsToAdmit
}

const vec = (name: string): number[] => beefOf(byName(v, name))
const applied = (s: MemoryStorage, txid: string): Promise<boolean> => s.doesAppliedTransactionExist({ txid, topic })

async function answer(e: Engine, query: Record<string, unknown>): Promise<string[]> {
  const a = await e.lookup({ service: LookupService, query })
  if (a.type !== 'output-list') throw new Error('not an output list')
  return a.outputs.map((o) => Transaction.fromBEEF(o.beef).id('hex'))
}

test('suppression: a manager that records refusals loses a genuine carrier to a copy in a worse BEEF; tm_bbox does not', async () => {
  const note = byName(v, 'carrier-envelope-note')
  // The same txid, its funding tree unproven: SPV passes, the beef rule refuses.
  const worse = byName(v, 'carrier-beef-tree-unproven')
  assert.equal(worse.txid, note.txid)

  const naive = await setup({ naive: true })
  assert.deepEqual(await submit(naive.engine, beefOf(worse)), [])
  assert.ok(await applied(naive.storage, note.txid), 'the refusal is recorded as applied')
  const before = naive.calls()
  assert.deepEqual(await submit(naive.engine, beefOf(note)), [])
  assert.equal(naive.calls(), before, 'the genuine carrier is a duplicate the manager never sees')

  const real = await setup()
  assert.deepEqual(await submit(real.engine, beefOf(worse)), [], 'raised: nothing admitted')
  assert.equal(await applied(real.storage, note.txid), false, 'nothing recorded')
  assert.equal(real.host.count('bbox_refused_total', { reason: 'beef' }), 1)
  assert.deepEqual(await submit(real.engine, beefOf(note)), [0], 'the genuine carrier is admitted')
})

test('out of order: an unmined sweep of a held output and a spend of an unheld tombstone are refused without a trace, and decided again later', async () => {
  const { engine: e, storage } = await setup()
  const tree = byName(v, 'funding-tree')
  assert.deepEqual(await submit(e, vec('funding-tree')), [0])
  // The unmined sweep spends the held output 0 of the tree: judged as a sweep, refused unmined.
  assert.deepEqual(await submit(e, vec('sweep-unmined')), [])
  assert.equal(await applied(storage, byName(v, 'sweep-unmined').txid), false)
  assert.equal((await storage.findOutput(tree.txid, 0, topic))?.spent, false, 'the held output is not marked spent')
  assert.equal(await applied(storage, byName(v, 'sweep').txid), false)
  // A spend of the tombstone before the sweep arrives: nothing held, refused not-bbox.
  assert.deepEqual(await submit(e, vec('spend-tombstone')), [])
  assert.equal(await applied(storage, byName(v, 'spend-tombstone').txid), false)
  assert.deepEqual(await submit(e, vec('sweep')), [0], 'the sweep, mined, is admitted')
  assert.equal((await storage.findOutput(tree.txid, 0, topic))?.spent, true)
  assert.deepEqual(await submit(e, vec('spend-tombstone')), [], 'offered again, the spend is accepted for what it retains')
  assert.ok(await applied(storage, byName(v, 'spend-tombstone').txid))
  assert.equal((await storage.findOutput(byName(v, 'sweep').txid, 0, topic))?.spent, true)
})

test('every transaction vector through the engine: admitted as the vectors say, and nothing refused is recorded', async () => {
  const { engine: e, storage, ls } = await setup()
  const admitted = new Set(v.transactions.filter((t) => t.verdict === 'admit').map((t) => t.txid))
  let checked = 0
  const engineRefused: string[] = []
  for (const t of v.transactions) {
    let got: number[] | undefined
    try {
      got = await submit(e, beefOf(t))
    } catch {
      // The engine's own SPV refuses before any manager runs: a proof at a
      // height its headers do not hold, a proof with a leaf it cannot place.
      assert.equal(t.verdict, 'refuse', t.name)
      engineRefused.push(t.name)
    }
    if (t.verdict === 'admit') {
      assert.ok(await applied(storage, t.txid), t.name)
    } else if (!admitted.has(t.txid)) {
      assert.deepEqual(got ?? [], [], t.name)
      assert.equal(await applied(storage, t.txid), false, t.name)
    }
    checked++
  }
  assert.equal(checked, v.transactions.length)
  // The SDK's interpreter is stricter than the rules need (it enforces low S
  // and minimal pushes itself), so the engine refuses some carriers before
  // tm_bbox sees them; tm_bbox.test.ts runs every one through the manager.
  assert.ok(engineRefused.every((n) => byName(v, n).verdict === 'refuse'))
  // The sweep retracted the note and the refs envelopes; the receipt acknowledges them, and the payment envelope is held.
  const cm = (name: string): string => byName(v, name).admits!.commitment!
  assert.equal(ls.status(cm('carrier-envelope-note')), 'retracted')
  assert.equal(ls.status(cm('carrier-locktime-max')), 'retracted')
  assert.equal(ls.status(cm('carrier-envelope-payment')), 'held')
  assert.equal(ls.status(cm('carrier-receipt')), 'held')
  assert.deepEqual(await answer(e, { office, by: v.recipientIdentityKey, receiptFor: byName(v, 'carrier-envelope-refs').txid }), [byName(v, 'carrier-receipt').txid])
  assert.deepEqual(await answer(e, { office, spent: v.sweepRetractsOutpoints[1]! }), [byName(v, 'sweep').txid])
  // The vectors were created long before now: out of the window, so history answers the payment envelope.
  assert.deepEqual(await answer(e, { office, history: v.recipientIdentityKey }), [byName(v, 'carrier-envelope-payment').txid])
})

test('restore from the engine storage reaches the same answers and statuses whatever order the rows come in, with or without their BEEF', async () => {
  const now = Math.floor(Date.now() / 1000)
  const s = await setup()
  const alice = new Party('alice')
  const bob = new Party('bob')
  const aTree = fundingTree(alice, 4, s.chain)
  const bTree = fundingTree(bob, 1, s.chain)
  const seal = async (vout: number, box: string, created: number, lock = LockTime): Promise<Transaction> =>
    await carrier(aTree, vout, alice, await envelopeRecord({ office, from: alice, to: bob, box, created }), lock)
  const e1 = await seal(0, 'inbox', now - 300)
  const e1b = await seal(0, 'inbox', now - 290, LockTime + 1)
  const e2 = await seal(1, 'files', now - 200)
  const e3 = await seal(2, 'inbox', now - 100)
  const rc = await carrier(bTree, 0, bob, receiptRecord(office, bob, [commitment(e2)], now))
  const sw = await sweep(aTree, [2], alice, s.chain)
  const spendTomb = byName(v, 'spend-tombstone')
  for (const x of [rc, sw, e3, e2, e1b, e1]) assert.deepEqual(await submit(s.engine, x.toAtomicBEEF()), [0])
  assert.deepEqual(await submit(s.engine, vec('funding-tree')), [0])
  assert.deepEqual(await submit(s.engine, vec('sweep')), [0])
  assert.deepEqual(await submit(s.engine, beefOf(spendTomb)), [])
  const questions: Array<Record<string, unknown>> = [
    { office, to: bob.hex },
    { office, to: bob.hex, box: 'inbox' },
    { office, to: bob.hex, from: alice.hex },
    { office, history: bob.hex },
    { office, by: bob.hex, receiptFor: e2.id('hex') },
    { office, spent: `${aTree.id('hex')}.2` },
    { office, spent: v.sweepRetractsOutpoints[0]! },
  ]
  const live = await Promise.all(questions.map((q) => s.ls.lookup({ service: LookupService, query: q })))
  const lowest = [e1, e1b].sort((a, b) => (toHex(commitment(a)) < toHex(commitment(b)) ? -1 : 1))[0]!
  assert.deepEqual(live[0]!.map((f) => f.txid), [lowest.id('hex')])
  assert.deepEqual(live[3]!.map((f) => f.txid), [e2.id('hex')])
  assert.deepEqual(live[6]!.map((f) => f.txid), [byName(v, 'sweep').txid], 'the tombstone, spent by the spend, still answers')
  const statuses = (ls: BboxLookupService): Array<string | undefined> => [e1, e1b, e2, e3, rc].map((x) => ls.status(toHex(commitment(x))))
  const want = statuses(s.ls)
  assert.deepEqual(want.slice(2), ['acknowledged', 'retracted', 'held'])

  const rows = await s.storage.findUTXOsForTopic(topic, undefined, undefined, true)
  assert.ok(!rows.some((o) => o.txid === byName(v, 'sweep').txid), 'the spent tombstone is not among the unspent rows')
  const bare = rows.map((o) => ({ ...o, beef: undefined }))
  for (const order of [rows, [...rows].reverse(), bare, []]) {
    const host = new FakeHost()
    const again = bboxModule(host, config, [topic], s.journal).ls
    assert.ok((await again.restore(order, s.storage)) > 0)
    assert.deepEqual(statuses(again), want)
    for (const [i, q] of questions.entries()) assert.deepEqual(await again.lookup({ service: LookupService, query: q }), live[i], JSON.stringify(q))
  }
})

test('engine answers are hydrated from storage, and a dropped carrier is deleted from storage and never restored', async () => {
  const now = Math.floor(Date.now() / 1000)
  const s = await setup()
  const alice = new Party('alice')
  const bob = new Party('bob')
  const tree = fundingTree(alice, 1, s.chain)
  const many: Transaction[] = []
  for (let i = 0; i < 10; i++) many.push(await carrier(tree, 0, alice, await envelopeRecord({ office, from: alice, to: bob, box: 'inbox', created: now - 60 }), LockTime + i))
  for (const x of many) assert.deepEqual(await submit(s.engine, x.toAtomicBEEF()), [0])
  const sorted = [...many].sort((a, b) => (toHex(commitment(a)) < toHex(commitment(b)) ? -1 : 1))
  assert.deepEqual(await answer(s.engine, { office, to: bob.hex }), [sorted[0]!.id('hex')], 'the engine hydrates the one answered carrier')
  // The ninth and tenth superseded carriers were dropped: gone from storage once flushed.
  await s.ls.prune()
  for (const x of sorted.slice(9)) assert.equal(await s.storage.findOutput(x.id('hex'), 0, topicOf(office)), null)
  for (const x of sorted.slice(0, 9)) assert.notEqual(await s.storage.findOutput(x.id('hex'), 0, topicOf(office)), null)
  // Offered again, a dropped carrier is a duplicate the engine never shows the manager.
  assert.deepEqual(await submit(s.engine, sorted[9]!.toAtomicBEEF()), [])
  const again = bboxModule(new FakeHost(), config, [topic], s.journal).ls
  await again.restore(await s.storage.findUTXOsForTopic(topic, undefined, undefined, true), s.storage)
  assert.equal(again.envelopeCount, 9)
  assert.ok(again.dropped(toHex(commitment(sorted[9]!))))
})
