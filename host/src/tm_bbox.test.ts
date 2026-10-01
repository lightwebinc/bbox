/**
 * tm_bbox on its own: every golden transaction admitted or refused through
 * the topic manager exactly as the vectors say, every refusal record of
 * refusal-v1.json refused inside a real carrier for the reason the vectors
 * give at admission, a BEEF over the host's bound, and every refusal label
 * a host counts shown to be reachable.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { P2PKH, PrivateKey, Transaction } from '@bsv/sdk'
import { LockTime } from '@lightwebinc/bcommon'
import { DefaultMaxBEEF } from './beef.js'
import { Chain, Party, carrier, fromNowhere, fundingTree } from './testmint.js'
import { FakeHost, beefOf, heldOf, refusalVectors, tracker, txVectors } from './testutil.js'
import { BboxTopicManager, Reasons, Refused } from './tm_bbox.js'
import { fromHex } from './util.js'

const v = txVectors()
const seen = new Set<string>()

async function verdict(tm: BboxTopicManager, beef: number[], held: number[]): Promise<{ admit?: { outputsToAdmit: number[]; coinsToRetain: number[] }; reason?: string }> {
  try {
    return { admit: await tm.identifyAdmissibleOutputs(beef, held) }
  } catch (e) {
    if (!(e instanceof Refused)) throw e
    seen.add(e.reason)
    return { reason: e.reason }
  }
}

test('every transaction vector is admitted or refused, raising, as the vectors say', async () => {
  const host = new FakeHost()
  const tm = new BboxTopicManager(v.topic, host, tracker(v.headers))
  assert.equal(tm.office, v.office)
  let admitted = 0
  let refused = 0
  for (const t of v.transactions) {
    const got = await verdict(tm, beefOf(t), heldOf(t))
    if (t.verdict === 'admit') {
      assert.deepEqual(got.admit, { outputsToAdmit: t.admits!.outputsToAdmit, coinsToRetain: t.admits!.coinsToRetain }, t.name)
      admitted++
    } else {
      assert.equal(got.reason, t.reason, t.name)
      refused++
    }
  }
  assert.equal(admitted + refused, v.transactions.length)
  assert.ok(admitted >= 9 && refused >= 45, `${admitted} admitted, ${refused} refused`)
  // Five envelope admissions, one of them a txid admitted before in
  // another BEEF: counted once, and once as a repeat.
  assert.equal(host.count('bbox_admitted_total', { kind: 'envelope' }), 4)
  assert.equal(host.count('bbox_admitted_repeats_total', { kind: 'envelope' }), 1)
  assert.equal(host.count('bbox_admitted_total', { kind: 'sweep' }), 2)
  assert.equal(host.count('bbox_refused_total', { reason: 'unlock' }), 6)
})

test('every refusal record, carried by an otherwise valid carrier, is refused for its admission reason', async () => {
  const chain = new Chain(6000)
  // The vectors' sender: 32 bytes of 0x42, a public test key.
  const sender = new Party('vector sender', PrivateKey.fromHex('42'.repeat(32)))
  const vectors = refusalVectors()
  const tree = fundingTree(sender, vectors.length, chain)
  const tm = new BboxTopicManager(v.topic, new FakeHost(), chain.tracker)
  for (const [i, r] of vectors.entries()) {
    const k = await carrier(tree, i, sender, fromHex(r.record))
    const got = await verdict(tm, k.toAtomicBEEF(), [])
    const want = r.admissionClaims === 'none' ? 'not-bbox' : r.admissionReason
    assert.equal(got.reason, want, r.name)
  }
})

const note = fromHex(v.transactions.find((t) => t.name === 'carrier-envelope-note')!.admits!.record!)

test('a BEEF over the host bound is refused beef, whatever it carries', async () => {
  const chain = new Chain(6100)
  const owner = new Party('big tree owner')
  // 6000 funding outputs of 49 bytes and more: the carrier's BEEF is over 256 KiB.
  const tree = fundingTree(owner, 6000, chain)
  const beef = (await carrier(tree, 0, owner, note)).toAtomicBEEF()
  assert.ok(beef.length > DefaultMaxBEEF)
  const tm = new BboxTopicManager(v.topic, new FakeHost(), chain.tracker)
  assert.equal((await verdict(tm, beef, [])).reason, 'beef')
  // A host that names a larger bound reads it, and refuses it for what it is:
  // the note's record names the vectors' sender, not this owner.
  const roomy = new BboxTopicManager(v.topic, new FakeHost(), chain.tracker, beef.length)
  assert.equal((await verdict(roomy, beef, [])).reason, 'lock')
  // Bytes that are no BEEF at all.
  assert.equal((await verdict(tm, [1, 2, 3], [])).reason, 'beef')
})

test('a transaction that is not bbox is refused not-bbox, and a carrier that could be mined is refused mineable', async () => {
  const chain = new Chain(6200)
  const payer = new Party('payer')
  const coin = chain.mine(fromNowhere([{ satoshis: 5000, lockingScript: new P2PKH().lock(payer.key.toAddress()) }]))
  const tm = new BboxTopicManager(v.topic, new FakeHost(), chain.tracker)
  const plain = new Transaction(
    1,
    [{ sourceTransaction: coin, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(payer.key), sequence: 0xffffffff }],
    [{ satoshis: 4000, lockingScript: new P2PKH().lock(new Party('payee').key.toAddress()) }],
    0,
  )
  await plain.sign()
  assert.equal((await verdict(tm, plain.toAtomicBEEF(), [])).reason, 'not-bbox')
  const owner = new Party('mineable owner')
  const mineable = await carrier(fundingTree(owner, 1, chain), 0, owner, note, LockTime - 1)
  assert.equal((await verdict(tm, mineable.toAtomicBEEF(), [])).reason, 'mineable')
})

test('every refusal label a host counts is reachable, and each has a test above', () => {
  assert.deepEqual(Reasons.filter((r) => !seen.has(r)), [])
})

test('the manager needs no inputs and documents itself', async () => {
  const tm = new BboxTopicManager(v.topic, new FakeHost())
  assert.deepEqual(await tm.identifyNeededInputs(), [])
  assert.match(await tm.getDocumentation(), /raised/)
  assert.deepEqual(await tm.getMetaData(), { name: v.topic, shortDescription: `Admits the bbox office ${v.office}.` })
  assert.throws(() => new BboxTopicManager('tm_log_x_abcdefghij', new FakeHost()))
  assert.throws(() => new BboxTopicManager(v.topic, new FakeHost(), undefined, 1000))
})

test('an admission is counted once per transaction; a repeat the engine lets through is counted apart', async () => {
  const host = new FakeHost()
  const tm = new BboxTopicManager(v.topic, host, tracker(v.headers))
  const sweep = v.transactions.find((t) => t.verdict === 'admit' && t.name.includes('sweep'))
  assert.ok(sweep !== undefined, 'a sweep vector')
  for (let i = 0; i < 3; i++) await verdict(tm, beefOf(sweep), heldOf(sweep))
  assert.equal(host.count('bbox_admitted_total', { kind: 'sweep' }), 1)
  assert.equal(host.count('bbox_admitted_repeats_total', { kind: 'sweep' }), 2)
})
