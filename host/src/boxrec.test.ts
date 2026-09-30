/**
 * Unit checks of the codec beside the vectors: grammars, bounds, the
 * helpers the transaction rules are built on, and admission's handling of
 * its host and previous coins.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { MerklePath, PrivateKey, Transaction, UnlockingScript, LockingScript } from '@bsv/sdk'
import { CborMap } from '@lightwebinc/bcommon'
import { MaxKeys, Refusal, checkBox, checkOffice, encodeEnvelope, newOffice, type Envelope } from './boxrec.js'
import { admit } from './admit.js'
import { beefCounts, minimalPath } from './beef.js'
import { rfc3339 } from './content.js'
import { canonical, parseJSON } from './jcs.js'
import { parseAfter, parseOutpoint } from './query.js'
import { fundingScript, isFundingShape, pushFields, pushDropScript, strictSignature } from './script.js'
import { strictBase64 } from './util.js'

const refuses = (f: () => unknown, reason: string): void => {
  assert.throws(f, (e: unknown) => e instanceof Refusal && e.reason === reason)
}

test('office and box grammars', () => {
  for (const ok of ['a_abcdefghij', 'example_office_qzxkvbmwtr', 'x'.repeat(31) + '_abcdefghij']) checkOffice(ok)
  for (const bad of ['example_office', 'a__b_abcdefghij', '_a_abcdefghij', 'a_ABCDEFGHIJ', 'x'.repeat(32) + '_abcdefghij', 'a1_abcdefghij', 'é_abcdefghij']) {
    refuses(() => checkOffice(bad), 'office')
  }
  for (const ok of ['inbox', 'payment_inbox', 'files2', 'b'.repeat(50)]) checkBox(ok)
  for (const bad of ['1inbox', 'inbox_', 'in__box', 'Inbox', 'b'.repeat(51), '']) refuses(() => checkBox(bad), 'box')
  assert.throws(() => newOffice('Bad'))
  assert.match(newOffice('mail'), /^mail_[a-z]{10}$/)
})

test('a record of more than 64 keys is refused before it is written', () => {
  const key = PrivateKey.fromHex('42'.repeat(32)).toPublicKey().encode(true) as number[]
  const e: Envelope = {
    office: 'example_office_qzxkvbmwtr', to: Uint8Array.from(key), box: 'inbox', from: Uint8Array.from(key),
    created: 1, expires: 0, content: Uint8Array.of(1),
    extra: Array.from({ length: MaxKeys - 7 }, (_, i) => ({ key: BigInt(8 + i), val: 0n })),
  }
  refuses(() => encodeEnvelope(e), 'too-large')
  e.extra = [{ key: 7n, val: 0n }]
  refuses(() => encodeEnvelope(e), 'key-type')
  e.extra = [{ key: 8n, val: new CborMap([]) }]
  assert.ok(encodeEnvelope(e).length > 0)
})

test('RFC 3339 created, cursors and outpoints', () => {
  assert.equal(rfc3339(1767225600), '2026-01-01T00:00:00Z')
  assert.equal(rfc3339(253402300799), '9999-12-31T23:59:59Z')
  assert.equal(parseAfter('1:' + '0'.repeat(64))?.created, 1)
  for (const bad of ['01:' + '0'.repeat(64), '253402300800:' + '0'.repeat(64), '1:' + 'A'.repeat(64), '1'] ) assert.equal(parseAfter(bad), undefined)
  assert.equal(parseOutpoint('0'.repeat(64) + '.4294967295')?.vout, 4294967295)
  for (const bad of ['0'.repeat(64) + '.4294967296', '0'.repeat(64) + '.01', '0'.repeat(64) + ':0']) assert.equal(parseOutpoint(bad), undefined)
})

test('the JSON subset refuses what JSON.parse takes', () => {
  const enc = new TextEncoder()
  for (const s of ['{"a":1,"a":2}', '{"a":"\\ud800"}', '{"a":9007199254740992}', '{"a":1e2}', '﻿{}', '{"a":-0}']) {
    assert.throws(() => parseJSON(enc.encode(s)), s)
  }
  assert.equal(new TextDecoder().decode(canonical(parseJSON(enc.encode('{"b":[1,"\\u0041"],"a":null}')))), '{"a":null,"b":[1,"A"]}')
})

test('strict base64 and strict signatures', () => {
  assert.deepEqual(strictBase64('AAE='), Uint8Array.of(0, 1))
  for (const bad of ['', 'AAE', 'AAF=', 'AA\nE=', 'AA-_']) assert.equal(strictBase64(bad), undefined, bad)
  const sig = PrivateKey.fromHex('42'.repeat(32)).sign([1, 2, 3]).toDER() as number[]
  assert.ok(strictSignature(Uint8Array.from(sig)))
  assert.ok(!strictSignature(Uint8Array.from([0x30, 0x06, 0x02, 0x01, 0x00, 0x02, 0x01, 0x01])))
})

test('scripts: rebuild, read back, funding shape', () => {
  const key = Uint8Array.from(PrivateKey.fromHex('42'.repeat(32)).toPublicKey().encode(true) as number[])
  for (const n of [3, 75, 76, 255, 256, 65535, 65536]) {
    const f = new Uint8Array(n).fill(0xa5)
    const s = pushDropScript(key, [f], Uint8Array.of(0x30, 1))
    assert.deepEqual(pushFields(s), [f, Uint8Array.of(0x30, 1)])
  }
  assert.ok(isFundingShape(fundingScript(key)))
  const loose = fundingScript(key)
  assert.ok(!isFundingShape(Uint8Array.from([...loose.subarray(0, 35), 0x4c, 3, ...loose.subarray(36)])))
})

test('a proof is minimal only with the hashes it needs', () => {
  const t = 'aa'.repeat(32)
  const s = 'bb'.repeat(32)
  const at = { offset: 2, hash: t, txid: true }
  // Only the path's shape is read, so the SDK's own validation is left out.
  const mp = (path: Array<Array<{ offset: number; hash?: string; txid?: boolean; duplicate?: boolean }>>): MerklePath => ({ path }) as unknown as MerklePath
  assert.ok(minimalPath(mp([[at, { offset: 3, hash: s }], [{ offset: 0, hash: s }]]), t))
  assert.ok(minimalPath(mp([[at, { offset: 3, duplicate: true }], [{ offset: 0, hash: s }]]), t))
  assert.ok(!minimalPath(mp([[{ offset: 0, hash: s }, at, { offset: 3, hash: s }], [{ offset: 0, hash: s }]]), t))
  assert.ok(!minimalPath(mp([[at, { offset: 3, hash: s }], [{ offset: 0, hash: s }, { offset: 1, hash: s }]]), t))
  assert.ok(!minimalPath(mp([[at, { offset: 1, hash: s }], [{ offset: 0, hash: s }]]), t))
  assert.ok(!minimalPath(mp([[at, { offset: 3, hash: s, txid: true }], [{ offset: 0, hash: s }]]), t))
})

test('admission: the host office, previous coins and bytes that are no BEEF', async () => {
  await assert.rejects(admit([], [], { office: 'example_office' }), /host office/)
  const h = { office: 'example_office_qzxkvbmwtr' }
  for (const b of [[], [0x01, 0x00, 0xbe, 0xef], new Array(64).fill(0xff)]) {
    await assert.rejects(admit(b, [], h), (e: unknown) => e instanceof Refusal && e.reason === 'beef')
  }
  const parent = new Transaction(1, [{ sourceTXID: '07'.repeat(32), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff }], [{ satoshis: 1, lockingScript: LockingScript.fromHex('51') }], 0)
  parent.merklePath = new MerklePath(1, [[{ offset: 0, hash: parent.id('hex'), txid: true }]])
  const tx = new Transaction(1, [{ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff }], [{ satoshis: 1, lockingScript: LockingScript.fromHex('51') }], 0)
  const beef = tx.toAtomicBEEF()
  assert.deepEqual(beefCounts(Uint8Array.from(beef)), { bumps: 1, txs: 2 })
  await assert.rejects(admit(beef, [1], h), /previous coin/)
  await assert.rejects(admit(beef, [], h), (e: unknown) => e instanceof Refusal && e.reason === 'not-bbox')
  const a = await admit(beef, [0, 0], h)
  assert.equal(a.kind, 'spend')
  assert.deepEqual(a.retain, [0])
  assert.deepEqual(a.outputs, [])
})
