/**
 * The shortest-form rule of the payment check's BEEF reading (wideCount),
 * against the structural walk bcommon's checkBEEF does before it: what the
 * Go guard refuses inside a transaction, and what it leaves alone outside.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { checkBEEF } from '@lightwebinc/bcommon'
import { wideCount } from './beef.js'
import { admit } from './admit.js'
import { Refusal, type Envelope } from './boxrec.js'
import { checkPayment, type Payment } from './payment.js'
import type { ChainTracker } from '@bsv/sdk'

type Count = (n: number) => number[]

const shortest: Count = (n) => (n < 0xfd ? [n] : [0xfd, n & 0xff, n >> 8])
const fd: Count = (n) => [0xfd, n & 0xff, n >> 8]
const fe: Count = (n) => [0xfe, n & 0xff, (n >> 8) & 0xff, (n >> 16) & 0xff, n >>> 24]
const ff: Count = (n) => [0xff, ...fe(n).slice(1), 0, 0, 0, 0]

/**
 * A BEEF V2 of one raw transaction (data format 0): one input whose script
 * is scriptLen bytes, one output with an empty script, each count written
 * by count.
 */
function beef(count: Count, scriptLen = 1): Uint8Array {
  const tx = [
    1, 0, 0, 0,
    ...count(1),
    ...new Array<number>(36).fill(0x11),
    ...count(scriptLen),
    ...new Array<number>(scriptLen).fill(0x51),
    0xff, 0xff, 0xff, 0xff,
    ...count(1),
    ...new Array<number>(8).fill(0),
    ...count(0),
    0, 0, 0, 0,
  ]
  return Uint8Array.from([0x02, 0x00, 0xbe, 0xef, 0x00, 0x01, 0x00, ...tx])
}

const bound = 1 << 16

test('a count inside a transaction written wider than its shortest form is refused, in any of the wide forms', () => {
  assert.equal(checkBEEF(beef(shortest), bound), undefined)
  assert.equal(wideCount(beef(shortest)), undefined)
  for (const wide of [fd, fe, ff]) {
    const b = beef(wide)
    assert.equal(checkBEEF(b, bound), undefined, 'the structural walk passes it')
    assert.match(wideCount(b) ?? '', /^transaction 0: the input count 1 is not written in its shortest form$/)
  }
})

test('a script length that needs three bytes is in its shortest form there, and wide in five', () => {
  assert.equal(wideCount(beef(shortest, 0xfd)), undefined)
  const b = beef((n) => (n === 0xfd ? fe(n) : shortest(n)), 0xfd)
  assert.equal(checkBEEF(b, bound), undefined)
  assert.equal(wideCount(b), "transaction 0: input 0's script length 253 is not written in its shortest form")
})

test("the BEEF's own counts are not held to the rule, as the Go guard does not hold them", () => {
  const b = beef(shortest)
  // The transaction count, one, written in three bytes.
  const wideTxs = Uint8Array.from([...b.subarray(0, 5), 0xfd, 0x01, 0x00, ...b.subarray(6)])
  assert.equal(checkBEEF(wideTxs, bound), undefined)
  assert.equal(wideCount(wideTxs), undefined)
})

// The empty BEEF, the empty proof and the inputless transaction are refused
// by bcommon's checkBEEF, which admit and checkPayment now rely on in place of
// checks of their own; these keep both paths refusing each of them.
const le4 = [1, 0, 0, 0]
const noTx = [0x01, 0x00, 0xbe, 0xef, 0x00, 0x00]
const emptyProof = [0x01, 0x00, 0xbe, 0xef, 0x01, 0x00, 0x01, 0x00, 0x00]
const noInputs = [0x02, 0x00, 0xbe, 0xef, 0x00, 0x01, 0x00, ...le4, 0x00, 0x01, ...new Array<number>(8).fill(0), 0x00, 0, 0, 0, 0, ...new Array<number>(40).fill(0)]
const atomic = (inner: number[]): number[] => [0x01, 0x01, 0x01, 0x01, ...new Array<number>(32).fill(0xaa), ...inner]
const refusedShapes: Array<[string, number[]]> = [
  ['an empty BEEF', noTx],
  ['an empty proof', emptyProof],
  ['a transaction of no inputs', noInputs],
]

test('admit refuses an empty BEEF, an empty proof and a transaction of no inputs', async () => {
  for (const [name, b] of refusedShapes) {
    assert.notEqual(checkBEEF(Uint8Array.from(b), bound), undefined, `${name}: checkBEEF`)
    await assert.rejects(admit(b, [], { office: 'example_office_qzxkvbmwtr' }), (e: unknown) => e instanceof Refusal && e.reason === 'beef', name)
  }
})

test('checkPayment refuses an empty BEEF, an empty proof and a transaction of no inputs', async () => {
  const e = { created: 0, expires: 1e9, from: '' } as unknown as Envelope
  for (const [name, b] of refusedShapes) {
    const p = { beef: Uint8Array.from(atomic(b)), outputs: [] } as unknown as Payment
    await assert.rejects(checkPayment({} as Parameters<typeof checkPayment>[0], e, p, {} as ChainTracker, 0), (x: unknown) => x instanceof Refusal && x.reason === 'payment-beef', name)
  }
})
