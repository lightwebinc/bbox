/**
 * The shortest-form rule of the payment check's BEEF reading (wideCount),
 * against the structural walk bcommon's checkBEEF does before it: what the
 * Go guard refuses inside a transaction, and what it leaves alone outside.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { checkBEEF } from '@lightwebinc/bcommon'
import { wideCount } from './beef.js'

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
