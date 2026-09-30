/**
 * ls_bbox on its own, fed what tm_bbox admits: every question class and its
 * order and pages, the answer window, the one winner per funding outpoint,
 * the evidence cap, arrival order, retraction, retention floors, and the
 * outpoint rows that outlive every drop.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { Transaction } from '@bsv/sdk'
import { LockTime } from '@lightwebinc/bcommon'
import { topic } from './boxrec.js'
import { FileJournal, MemoryJournal, type Journal } from './journal.js'
import { BboxLookupService, WindowBefore } from './ls_bbox.js'
import { Chain, Party, carrier, commitment, envelopeRecord, fundingTree, receiptRecord, sweep } from './testmint.js'
import { FakeHost, readVectors } from './testutil.js'
import { BboxTopicManager } from './tm_bbox.js'
import { toHex } from './util.js'

const office = 'example_office_qzxkvbmwtr'
const t = topic(office)
const T0 = 1_800_000_000
const day = 86400
const alice = new Party('alice')
const bob = new Party('bob')
const carol = new Party('carol')
const dave = new Party('dave')

interface Rig {
  host: FakeHost
  ls: BboxLookupService
  tm: BboxTopicManager
  clock: { now: number }
  journal: Journal
}

function rig(chain: Chain, opts: { journal?: Journal; priced?: string[]; now?: number; retentionDays?: number } = {}): Rig {
  const host = new FakeHost()
  const clock = { now: opts.now ?? T0 }
  const journal = opts.journal ?? new MemoryJournal()
  const ls = new BboxLookupService({
    offices: new Set([office]),
    host,
    journal,
    priced: new Set(opts.priced ?? []),
    now: () => clock.now,
    retentionDays: opts.retentionDays,
  })
  return { host, ls, tm: new BboxTopicManager(t, host, chain.tracker), clock, journal }
}

async function feed(r: Rig, tx: Transaction): Promise<number[]> {
  const beef = tx.toAtomicBEEF()
  const a = await r.tm.identifyAdmissibleOutputs(beef, [])
  for (const i of a.outputsToAdmit) r.ls.outputAdmittedByTopic({ mode: 'whole-tx', atomicBEEF: beef, outputIndex: i, topic: t })
  return a.outputsToAdmit
}

async function ask(r: Rig, query: Record<string, unknown>): Promise<string[]> {
  return (await r.ls.lookup({ service: 'ls_bbox', query })).map((f) => f.txid)
}

const c = (tx: Transaction): string => toHex(commitment(tx))

async function letter(tree: Transaction, vout: number, from: Party, to: Party, box: string, created: number, expires = 0, lockTime = LockTime): Promise<Transaction> {
  return await carrier(tree, vout, from, await envelopeRecord({ office, from, to, box, created, expires }), lockTime)
}

async function receipt(tree: Transaction, vout: number, by: Party, acks: Transaction[], created: number): Promise<Transaction> {
  return await carrier(tree, vout, by, receiptRecord(office, by, acks.map(commitment), created))
}

test('every free class answers open envelopes, receipts and sweeps in order, and history answers the rest', async () => {
  const chain = new Chain(7000)
  const r = rig(chain)
  const aTree = fundingTree(alice, 10, chain)
  const cTree = fundingTree(carol, 2, chain)
  const bTree = fundingTree(bob, 2, chain)
  const e1 = await letter(aTree, 0, alice, bob, 'inbox', T0 - 300)
  const e2 = await letter(aTree, 1, alice, bob, 'files', T0 - 200)
  const e3 = await letter(cTree, 0, carol, bob, 'inbox', T0 - 100)
  const e4 = await letter(aTree, 2, alice, dave, 'inbox', T0 - 50)
  const expired = await letter(aTree, 3, alice, bob, 'inbox', T0 - 400, T0 - 1)
  const old = await letter(aTree, 4, alice, bob, 'inbox', T0 - WindowBefore - 1)
  const ahead = await letter(aTree, 5, alice, bob, 'inbox', T0 + 3601)
  for (const e of [e1, e2, e3, e4, expired, old, ahead]) assert.deepEqual(await feed(r, e), [0])
  const to = bob.hex
  assert.deepEqual(await ask(r, { office, to }), [e1.id('hex'), e2.id('hex'), e3.id('hex')])
  assert.deepEqual(await ask(r, { office, to, box: 'inbox' }), [e1.id('hex'), e3.id('hex')])
  assert.deepEqual(await ask(r, { office, to, from: alice.hex }), [e1.id('hex'), e2.id('hex')])
  assert.deepEqual(await ask(r, { office, to, after: `${T0 - 300}:${e1.id('hex')}` }), [e2.id('hex'), e3.id('hex')])
  assert.deepEqual(await ask(r, { office, to, box: 'inbox', after: `${T0 - 300}:${e1.id('hex')}` }), [e3.id('hex')])
  assert.deepEqual(await ask(r, { office, to, from: alice.hex, after: `${T0 - 300}:${'0'.repeat(64)}` }), [e1.id('hex'), e2.id('hex')])
  assert.deepEqual(await ask(r, { office, to: dave.hex }), [e4.id('hex')])
  // Expired and out of the window: history; dated ahead: nothing yet.
  assert.deepEqual(await ask(r, { office, history: to }), [old.id('hex'), expired.id('hex')])
  assert.deepEqual(await ask(r, { office, history: to, after: `${T0 - WindowBefore - 1}:${old.id('hex')}` }), [expired.id('hex')])

  // Bob acknowledges e1 and e2: out of the inbox, into history, and the receipt answers for each.
  const rc = await receipt(bTree, 0, bob, [e1, e2], T0)
  assert.deepEqual(await feed(r, rc), [0])
  assert.deepEqual(await ask(r, { office, to }), [e3.id('hex')])
  assert.equal(r.ls.status(c(e1)), 'acknowledged')
  assert.deepEqual(await ask(r, { office, by: to, receiptFor: e1.id('hex') }), [rc.id('hex')])
  assert.deepEqual(await ask(r, { office, by: to, receiptFor: e3.id('hex') }), [])
  assert.deepEqual(await ask(r, { office, by: alice.hex, receiptFor: e1.id('hex') }), [])
  assert.deepEqual(await ask(r, { office, history: to }), [old.id('hex'), expired.id('hex'), e1.id('hex'), e2.id('hex')])

  // Alice sweeps e1's funding output: retracted everywhere, and the sweep answers for the outpoint.
  const sw = await sweep(aTree, [0], alice, chain)
  assert.deepEqual(await feed(r, sw), [0])
  assert.equal(r.ls.status(c(e1)), 'retracted')
  assert.deepEqual(await ask(r, { office, history: to }), [old.id('hex'), expired.id('hex'), e2.id('hex')])
  assert.deepEqual(await ask(r, { office, spent: `${aTree.id('hex')}.0` }), [sw.id('hex')])
  assert.deepEqual(await ask(r, { office, spent: `${aTree.id('hex')}.1` }), [])
  assert.equal(r.host.count('bbox_lookups_total', { class: 'history' }), 3)
})

test('a page holds 64 envelopes, and -after pages on', async () => {
  const chain = new Chain(7100)
  const r = rig(chain)
  const tree = fundingTree(alice, 70, chain)
  const all: string[] = []
  for (let i = 0; i < 70; i++) {
    const e = await letter(tree, i, alice, bob, 'inbox', T0 - 1000 + Math.floor(i / 3))
    await feed(r, e)
    all.push(`${T0 - 1000 + Math.floor(i / 3)}:${e.id('hex')}`)
  }
  all.sort((a, b) => Number(a.split(':')[0]) - Number(b.split(':')[0]) || (a < b ? -1 : 1))
  const first = await ask(r, { office, to: bob.hex })
  assert.equal(first.length, 64)
  assert.deepEqual(first, all.slice(0, 64).map((x) => x.split(':')[1]))
  const rest = await ask(r, { office, to: bob.hex, after: all[63]! })
  assert.deepEqual(rest, all.slice(64).map((x) => x.split(':')[1]))
})

test('a question is refused, never answered empty, unless it is exactly one class for an office the host carries', async () => {
  const chain = new Chain(7200)
  const r = rig(chain, { priced: ['history', 'history-after'] })
  const q = readVectors<{ questions: Array<{ name: string; question: string; class?: string; refused?: boolean; reason?: string }> }>('query-v1.json')
  let refused = 0
  for (const x of q.questions) {
    const query = JSON.parse(x.question) as Record<string, unknown>
    if (x.class === undefined) {
      await assert.rejects(r.ls.lookup({ service: 'ls_bbox', query }), /ls_bbox: refused/, x.name)
      refused++
    } else if (x.class.startsWith('history')) {
      await assert.rejects(r.ls.lookup({ service: 'ls_bbox', query }), /priced on this host/, x.name)
      assert.deepEqual(await r.ls.answer({ service: 'ls_bbox', query }, true), [], x.name)
    } else {
      assert.deepEqual(await r.ls.lookup({ service: 'ls_bbox', query }), [], x.name)
    }
  }
  assert.ok(refused >= 10, `${refused} refused questions`)
  await assert.rejects(r.ls.lookup({ service: 'ls_bbox', query: { office: 'other_office_abcdefghij', to: bob.hex } }), /does not carry office/)
  await assert.rejects(r.ls.lookup({ service: 'ls_log', query: { office, to: bob.hex } }), /this service is ls_bbox/)
  await assert.rejects(r.ls.lookup({ service: 'ls_bbox', query: [office] }), /not an object/)
  // Unpriced, the history classes answer through the engine.
  const free = rig(chain)
  assert.deepEqual(await ask(free, { office, history: bob.hex }), [])
})

test('one answered carrier per funding outpoint: the lowest C wins, an acknowledged envelope wins over it, and a receipt by another key is inert', async () => {
  const chain = new Chain(7300)
  const r = rig(chain)
  const tree = fundingTree(alice, 1, chain)
  const bTree = fundingTree(bob, 1, chain)
  const cTree = fundingTree(carol, 1, chain)
  const three = [await letter(tree, 0, alice, bob, 'inbox', T0 - 30, 0, LockTime), await letter(tree, 0, alice, bob, 'inbox', T0 - 20, 0, LockTime + 1), await letter(tree, 0, alice, bob, 'inbox', T0 - 10, 0, LockTime + 2)]
  for (const e of three) assert.deepEqual(await feed(r, e), [0])
  const byC = [...three].sort((a, b) => (c(a) < c(b) ? -1 : 1))
  assert.deepEqual(await ask(r, { office, to: bob.hex }), [byC[0]!.id('hex')], 'only the lowest C is answered')
  assert.deepEqual(byC.map((e) => r.ls.status(c(e))), ['held', 'superseded', 'superseded'])
  // Carol cannot acknowledge envelopes to Bob.
  await feed(r, await receipt(cTree, 0, carol, [byC[2]!], T0))
  assert.deepEqual(byC.map((e) => r.ls.status(c(e))), ['held', 'superseded', 'superseded'])
  // Bob acknowledges the highest: it becomes the winner, so a lower C cannot replace a read message.
  await feed(r, await receipt(bTree, 0, bob, [byC[2]!], T0))
  assert.deepEqual(byC.map((e) => r.ls.status(c(e))), ['superseded', 'superseded', 'acknowledged'])
  assert.deepEqual(await ask(r, { office, to: bob.hex }), [])
  assert.deepEqual(await ask(r, { office, history: bob.hex }), [byC[2]!.id('hex')])
})

test('at most 8 superseded carriers are kept per outpoint, 64 once the winner is acknowledged; the rest are dropped', async () => {
  const chain = new Chain(7400)
  const r = rig(chain)
  const tree = fundingTree(alice, 2, chain)
  const bTree = fundingTree(bob, 1, chain)
  const many: Transaction[] = []
  for (let i = 0; i < 12; i++) many.push(await letter(tree, 0, alice, bob, 'inbox', T0 - 100, 0, LockTime + i))
  for (const e of many) await feed(r, e)
  const sorted = many.map(c).sort()
  assert.equal(r.host.count('bbox_dropped_total', { why: 'evidence' }), 3)
  assert.deepEqual(sorted.map((x) => r.ls.dropped(x)), [...Array(9).fill(false), true, true, true], 'the highest commitments go')
  assert.equal(r.ls.envelopeCount, 9)
  // With the winner acknowledged, 64 are kept.
  const more: Transaction[] = []
  for (let i = 0; i < 12; i++) more.push(await letter(tree, 1, alice, bob, 'files', T0 - 100, 0, LockTime + i))
  const winner = [...more].sort((a, b) => (c(a) < c(b) ? -1 : 1))[0]!
  await feed(r, await receipt(bTree, 0, bob, [winner], T0))
  for (const e of more) await feed(r, e)
  assert.equal(r.host.count('bbox_dropped_total', { why: 'evidence' }), 3)
  assert.equal(r.ls.status(c(winner)), 'acknowledged')
})

/** A seeded shuffle, so a failing order can be reproduced. */
function shuffle<T>(xs: T[], seed: number): T[] {
  const out = [...xs]
  let s = seed
  for (let i = out.length - 1; i > 0; i--) {
    s = (s * 1103515245 + 12345) % 2147483648
    const j = s % (i + 1)
    ;[out[i], out[j]] = [out[j]!, out[i]!]
  }
  return out
}

test('statuses and answers are the same whatever order the objects arrive in: receipts before envelopes, sweeps before carriers', async () => {
  const chain = new Chain(7500)
  const aTree = fundingTree(alice, 4, chain)
  const bTree = fundingTree(bob, 2, chain)
  const e1 = await letter(aTree, 0, alice, bob, 'inbox', T0 - 50)
  const e1b = await letter(aTree, 0, alice, bob, 'inbox', T0 - 40, 0, LockTime + 1)
  const e2 = await letter(aTree, 1, alice, bob, 'inbox', T0 - 30)
  const e3 = await letter(aTree, 2, alice, bob, 'files', T0 - 20)
  const e4 = await letter(aTree, 3, alice, bob, 'files', T0 - 10)
  const rc = await receipt(bTree, 0, bob, [e1b, e2], T0)
  const sw = await sweep(aTree, [2], alice, chain)
  const objects = [e1, e1b, e2, e3, e4, rc, sw]
  const snapshot = async (r: Rig): Promise<string> =>
    JSON.stringify({
      statuses: objects.slice(0, 6).map((x) => r.ls.status(c(x))),
      inbox: await ask(r, { office, to: bob.hex }),
      history: await ask(r, { office, history: bob.hex }),
      receipt: await ask(r, { office, by: bob.hex, receiptFor: e2.id('hex') }),
      sweep: await ask(r, { office, spent: `${aTree.id('hex')}.2` }),
    })
  const base = rig(chain)
  for (const x of objects) await feed(base, x)
  const want = await snapshot(base)
  assert.deepEqual(JSON.parse(want).statuses, ['superseded', 'acknowledged', 'acknowledged', 'retracted', 'held', 'held'])
  const orders = [[...objects].reverse(), [rc, sw, e4, e3, e2, e1b, e1]]
  for (let seed = 1; seed <= 20; seed++) orders.push(shuffle(objects, seed))
  for (const order of orders) {
    const r = rig(chain)
    for (const x of order) await feed(r, x)
    assert.equal(await snapshot(r), want, order.map((x) => objects.indexOf(x)).join(','))
  }
})

test('a retracted receipt acknowledges nothing', async () => {
  const chain = new Chain(7600)
  const r = rig(chain)
  const aTree = fundingTree(alice, 1, chain)
  const bTree = fundingTree(bob, 1, chain)
  const e = await letter(aTree, 0, alice, bob, 'inbox', T0 - 60)
  const rc = await receipt(bTree, 0, bob, [e], T0)
  await feed(r, e)
  await feed(r, rc)
  assert.deepEqual(await ask(r, { office, to: bob.hex }), [])
  await feed(r, await sweep(bTree, [0], bob, chain))
  assert.equal(r.ls.status(c(rc)), 'retracted')
  assert.equal(r.ls.status(c(e)), 'held')
  assert.deepEqual(await ask(r, { office, to: bob.hex }), [e.id('hex')])
  assert.deepEqual(await ask(r, { office, by: bob.hex, receiptFor: e.id('hex') }), [])
})

test('retention keeps receipts and sweeps 31 days from first sight and open envelopes while open; outpoint rows outlive every drop', async () => {
  const chain = new Chain(7700)
  const journal = new MemoryJournal()
  const r = rig(chain, { journal })
  const aTree = fundingTree(alice, 4, chain)
  const bTree = fundingTree(bob, 1, chain)
  const read = await letter(aTree, 0, alice, bob, 'inbox', T0 - 60)
  const unread = await letter(aTree, 1, alice, bob, 'inbox', T0 - 60)
  const rc = await receipt(bTree, 0, bob, [read], T0 + 10 * 365 * day)
  const sw = await sweep(aTree, [2], alice, chain)
  for (const x of [read, unread, rc, sw]) await feed(r, x)

  // A receipt's claimed created, ten years ahead, pins nothing: the floor runs from first sight.
  r.clock.now = T0 + 31 * day - 1
  assert.equal(await r.ls.prune(), 0, 'nothing is past the floor a second before it')
  assert.equal(r.ls.envelopeCount + r.ls.receiptCount + r.ls.sweepCount, 4)
  assert.deepEqual(await ask(r, { office, history: bob.hex }), [read.id('hex'), unread.id('hex')].sort((a, b) => (a < b ? -1 : 1)))
  r.clock.now = T0 + 31 * day
  assert.equal(await r.ls.prune(), 4, 'the receipt, the sweep and both envelopes, which the window has left')
  assert.equal(r.ls.receiptCount + r.ls.sweepCount + r.ls.envelopeCount, 0)
  assert.equal(r.host.count('bbox_dropped_total', { why: 'retention' }), 4)

  // Postage is spent for the host's life: a new carrier on a dropped winner's
  // outpoint is superseded, one on a swept outpoint retracted, and a receipt
  // dropped by retention still makes its envelope the winner.
  const now = r.clock.now
  const again = await letter(aTree, 0, alice, bob, 'inbox', now - 10, 0, LockTime + 5)
  const swept = await letter(aTree, 2, alice, bob, 'inbox', now - 10)
  const fresh = await letter(aTree, 3, alice, bob, 'inbox', now - 10)
  for (const x of [again, swept, fresh]) assert.deepEqual(await feed(r, x), [0])
  assert.equal(r.ls.status(c(again)), 'superseded')
  assert.equal(r.ls.status(c(swept)), 'retracted')
  assert.deepEqual(await ask(r, { office, to: bob.hex }), [fresh.id('hex')])

  // The same from the journal alone, as after a restart.
  const again2 = rig(chain, { journal, now })
  for (const x of [again, swept, fresh]) again2.ls.outputAdmittedByTopic({ mode: 'whole-tx', atomicBEEF: x.toAtomicBEEF(), outputIndex: 0, topic: t })
  assert.equal(again2.ls.status(c(again)), 'superseded')
  assert.equal(again2.ls.status(c(swept)), 'retracted')
  assert.equal(again2.ls.status(c(read)), 'superseded', 'the dropped winner closed its outpoint')
  assert.deepEqual(await ask(again2, { office, to: bob.hex }), [fresh.id('hex')])

  // An open envelope is kept past any retention.
  const open = rig(chain, { now: T0 })
  const kept = await letter(fundingTree(carol, 1, chain), 0, carol, bob, 'inbox', T0 + 20 * day)
  await feed(open, kept)
  open.clock.now = T0 + 40 * day
  assert.equal(await open.ls.prune(), 0)
  assert.deepEqual(await ask(open, { office, to: bob.hex }), [kept.id('hex')])
})

test('retention under the floor is refused; the file journal survives a torn last line and refuses a corrupt one', () => {
  assert.throws(() => rig(new Chain(7800), { retentionDays: 30 }), /floor/)
  const dir = mkdtempSync(join(tmpdir(), 'bbox-journal-'))
  try {
    const j = new FileJournal(dir)
    j.append({ k: 's', op: `${'aa'.repeat(32)}.0`, s: 'bb'.repeat(32), office, h: 9, at: 1 })
    j.close()
    writeFileSync(j.path, readFileSync(j.path, 'utf8') + '{"k":"s","op"')
    const k = new FileJournal(dir)
    assert.equal(k.load().length, 1, 'the torn line is ignored')
    k.append({ k: 'd', op: `${'aa'.repeat(32)}.0`, c: 'cc'.repeat(32), w: true })
    k.close()
    assert.equal(new FileJournal(dir).load().length, 2, 'and the next line starts on its own')
    writeFileSync(j.path, 'garbage\n' + readFileSync(j.path, 'utf8'))
    assert.throws(() => new FileJournal(dir).load(), /line 1/)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})
