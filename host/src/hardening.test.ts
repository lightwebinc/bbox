/**
 * Defects a review of a sibling host's index proved against it, each pinned
 * here with the sequence that shows it in this one: what one topic must not
 * take from another, what must not wait in a queue, and what a restore in
 * flight must not be handed.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Engine } from '@bsv/overlay'
import type { Transaction } from '@bsv/sdk'
import type { Output } from '@lightwebinc/bcommon'
import { LockTime } from '@lightwebinc/bcommon'
import { LookupService, Refusal, topic as topicOf } from './boxrec.js'
import { DefaultBudget, DefaultResponseBudget } from '@lightwebinc/bcommon/host'
import { MemoryJournal } from './journal.js'
import { BboxLookupService } from './ls_bbox.js'
import { bboxModule, type Config } from './module.js'
import { parseQuery } from './query.js'
import { Chain, Party, carrier, commitment, envelopeRecord, fundingTree, sweep } from './testmint.js'
import { FakeHost, MemoryStorage, quietConsole } from './testutil.js'
import { toHex } from './util.js'

const office = 'example_office_qzxkvbmwtr'
const office2 = 'other_office_abcdefghij'
const topic = topicOf(office)
const topic2 = topicOf(office2)
const alice = new Party('alice')
const bob = new Party('bob')
const id = (t: Transaction): string => t.id('hex')
const c = (t: Transaction): string => toHex(commitment(t))
const now = (): number => Math.floor(Date.now() / 1000)

const config: Config = { offices: [office, office2], stateDir: '/nonexistent', retentionDays: 31, maxBEEF: 262144, prices: new Map(), sessions: { max: 100, ttlSeconds: 600 }, handshakes: DefaultBudget, responses: DefaultResponseBudget }

/** The module and the real engine over an in-memory store. */
async function setup(chain: Chain, storage = new MemoryStorage()) {
  const host = new FakeHost()
  const journal = new MemoryJournal()
  const m = bboxModule(host, config, [topic, topic2], journal)
  await m.ls.restore([], storage)
  const engine = new Engine(m.module.topics as never, m.module.lookups as never, storage as never, chain.tracker, undefined, [], [], undefined, undefined, { [topic]: false, [topic2]: false }, false, undefined, undefined, undefined, quietConsole())
  const submit = async (tx: Transaction, t = topic): Promise<number[] | undefined> => (await engine.submit({ beef: tx.toAtomicBEEF(), topics: [t] }))[t]?.outputsToAdmit
  return { host, journal, ls: m.ls, storage, engine, submit }
}

/** n envelopes from alice to bob on one funding output, lowest commitment first. */
async function equivocations(chain: Chain, n: number): Promise<Transaction[]> {
  const tree = fundingTree(alice, 1, chain)
  const many: Transaction[] = []
  for (let i = 0; i < n; i++) many.push(await carrier(tree, 0, alice, await envelopeRecord({ office, from: alice, to: bob, box: 'inbox', created: now() - 60 }), LockTime + i))
  return many.sort((a, b) => (c(a) < c(b) ? -1 : 1))
}

test('a sweep admitted in two offices answers in both, and after a restart that finds its tombstone spent', async () => {
  const chain = new Chain(12000)
  const s = await setup(chain)
  const tree = fundingTree(alice, 2, chain)
  const a = await carrier(tree, 0, alice, await envelopeRecord({ office, from: alice, to: bob, box: 'inbox', created: now() - 60 }))
  // The second office's carrier on the same tree: the funding key does not name an office.
  const b = await carrier(tree, 1, alice, await envelopeRecord({ office: office2, from: alice, to: bob, box: 'inbox', created: now() - 60 }))
  assert.deepEqual(await s.submit(a, topic), [0])
  assert.deepEqual(await s.submit(b, topic2), [0])
  const sw = await sweep(tree, [0, 1], alice, chain)
  assert.deepEqual(await s.submit(sw, topic), [0])
  assert.deepEqual(await s.submit(sw, topic2), [0])
  const asked = async (ls: typeof s.ls, o: string, vout: number): Promise<string[]> => (await ls.lookup({ service: LookupService, query: { office: o, spent: `${id(tree)}.${vout}` } })).map((f) => f.txid)
  assert.deepEqual([s.ls.status(c(a)), s.ls.status(c(b))], ['retracted', 'retracted'])
  assert.deepEqual(await asked(s.ls, office, 0), [id(sw)])
  assert.deepEqual(await asked(s.ls, office2, 1), [id(sw)], 'the office whose carrier it retracts')
  assert.equal(s.ls.sweepCount, 1)
  assert.deepEqual(new Set(s.journal.lines.filter((l) => l.k === 's').map((l) => l.office)), new Set([office, office2]), 'the journal names the sweep in each office')
  // Its tombstone is spent in both topics: a restore is handed neither row, and reads both back by name.
  for (const t of [topic, topic2]) await s.storage.markUTXOAsSpent(id(sw), 0, t)
  const again = bboxModule(new FakeHost(), config, [topic, topic2], s.journal).ls
  const handed = [...(await s.storage.findUTXOsForTopic(topic, undefined, undefined, true)), ...(await s.storage.findUTXOsForTopic(topic2, undefined, undefined, true))]
  assert.equal(handed.some((o) => o.txid === id(sw)), false)
  await again.restore(handed, s.storage)
  assert.deepEqual([await asked(again, office, 0), await asked(again, office2, 1)], [[id(sw)], [id(sw)]])
  // Dropped from one topic, it still answers in the other.
  s.ls.outputNoLongerRetainedInHistory(id(sw), 0, topic)
  assert.deepEqual([await asked(s.ls, office, 0), await asked(s.ls, office2, 1)], [[], [id(sw)]])
  assert.equal(s.ls.sweepCount, 1)
  // Retention deletes it from every topic that still holds it.
  const late = new BboxLookupService({ offices: new Set([office, office2]), host: new FakeHost(), journal: s.journal, now: () => now() + 32 * 86400 })
  await late.restore(handed, s.storage)
  await late.prune()
  assert.deepEqual([await s.storage.findOutput(id(sw), 0, topic), await s.storage.findOutput(id(sw), 0, topic2)], [null, null])
})

test('a member named __proto__ is a member no class defines: refused, never dropped', async () => {
  const s = await setup(new Chain(12500))
  const q = `{"office":"${office}","to":"${bob.hex}","__proto__":"x"}`
  let got = 'parsed'
  try {
    parseQuery(JSON.parse(q) as Record<string, unknown>)
  } catch (e) {
    got = e instanceof Refusal ? e.reason : 'other'
  }
  assert.equal(got, 'query')
  await assert.rejects(s.engine.lookup({ service: LookupService, query: JSON.parse(q) as unknown }), /refused: query/)
  assert.equal(parseQuery(JSON.parse(`{"office":"${office}","to":"${bob.hex}"}`) as Record<string, unknown>).class.name, 'inbox')
})

test('what the index drops is deleted from storage at once, and a store that cannot delete does not grow a queue', async () => {
  const chain = new Chain(13000)
  const s = await setup(chain)
  const many = await equivocations(chain, 11)
  for (const x of many) assert.deepEqual(await s.submit(x), [0])
  // Nine kept, two dropped, with no retention pass in between.
  assert.deepEqual([s.ls.envelopeCount, (await s.storage.findUTXOsForTopic(topic)).length, s.ls.pendingDeletes], [9, 9, 0])
  for (const x of many.slice(9)) assert.equal(await s.storage.findOutput(id(x), 0, topic), null)
  // A store with no deleteOutput keeps what was dropped; the module does not keep a list of it.
  const chain2 = new Chain(13500)
  const more = await equivocations(chain2, 31)
  const cannot = new Proxy(new MemoryStorage(), { get: (t, k) => (k === 'deleteOutput' ? undefined : Reflect.get(t, k)) }) as MemoryStorage
  const host = new FakeHost()
  const ls = bboxModule(host, config, [topic, topic2], new MemoryJournal()).ls
  await ls.restore([], cannot)
  for (const x of more) await ls.outputAdmittedByTopic({ mode: 'whole-tx', topic, atomicBEEF: x.toAtomicBEEF(), outputIndex: 0 })
  await ls.prune()
  assert.deepEqual([ls.envelopeCount, ls.pendingDeletes], [9, 0])
  assert.equal(host.lines.filter((l) => l.msg.includes('cannot delete dropped outputs')).length, 1, 'said once')
})

test('an admission that meets a restore in flight waits for it, and is decided as any other', async () => {
  const chain = new Chain(14000)
  const s = await setup(chain)
  const many = await equivocations(chain, 10)
  for (const x of many.slice(1)) assert.deepEqual(await s.submit(x), [0])
  const journal = new MemoryJournal()
  for (const l of s.journal.lines) journal.append(l)
  const ls = bboxModule(new FakeHost(), config, [topic, topic2], journal).ls
  // While the restore reads its first row, the lowest commitment of all is admitted on the full row.
  let admitted: Promise<void> | undefined
  let seenAtOnce: unknown = 'not asked'
  const meanwhile = new Proxy(s.storage, {
    get: (t, k) =>
      k === 'findOutput'
        ? async (...args: Parameters<MemoryStorage['findOutput']>): Promise<Output | null> => {
            if (admitted === undefined) {
              admitted = Promise.resolve(ls.outputAdmittedByTopic({ mode: 'whole-tx', topic, atomicBEEF: many[0]!.toAtomicBEEF(), outputIndex: 0 }))
              seenAtOnce = ls.status(c(many[0]!))
              await new Promise((resolve) => setTimeout(resolve, 20))
            }
            return await t.findOutput(...args)
          }
        : Reflect.get(t, k),
  }) as MemoryStorage
  // Rows handed without their BEEF are read again from storage.
  await ls.restore(await s.storage.findUTXOsForTopic(topic), meanwhile)
  assert.notEqual(admitted, undefined, 'the admission came during the restore')
  assert.equal(seenAtOnce, undefined, 'not indexed while the restore was in flight')
  await admitted
  assert.equal(ls.status(c(many[0]!)), 'held', 'the lowest ever admitted is the winner')
  assert.equal(ls.status(c(many[1]!)), 'superseded')
  assert.ok(ls.dropped(c(many[9]!)), 'the row keeps eight superseded: the highest went')
  assert.equal(ls.envelopeCount, 9)
})
