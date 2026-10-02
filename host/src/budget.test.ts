/** The handshake budget: the route's bucket, each address's, and the keys addresses share. */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { DefaultBudget, HandshakeBudget, KeyedBudget, MaxAddresses, SeenRequests, addressKey } from './budget.js'

test('address keys: IPv4 as is, IPv4-mapped as IPv4, IPv6 by its /64', () => {
  assert.equal(addressKey('192.0.2.7'), '192.0.2.7')
  assert.equal(addressKey('::ffff:192.0.2.7'), '192.0.2.7')
  assert.equal(addressKey('2001:db8:0:1::5'), '2001:db8:0:1::/64')
  assert.equal(addressKey('2001:0DB8:0000:0001:aaaa:bbbb:cccc:dddd'), '2001:db8:0:1::/64')
  assert.equal(addressKey('2001:db8::1'), '2001:db8:0:0::/64')
  assert.equal(addressKey('::1'), '0:0:0:0::/64')
  assert.equal(addressKey('fe80::1%eth0'), 'fe80:0:0:0::/64')
  assert.equal(addressKey(''), '')
})

test('an address spends its burst, then refills at its rate; the route bucket is shared', () => {
  let now = 0
  const b = new HandshakeBudget({ perSec: 2, burst: 5, perAddressPerSec: 1, addressBurst: 2 }, () => now)
  assert.deepEqual(b.take('192.0.2.1'), { ok: true })
  assert.deepEqual(b.take('192.0.2.1'), { ok: true })
  assert.deepEqual(b.take('192.0.2.1'), { ok: false, limit: 'address', retryAfter: 1 })
  assert.deepEqual(b.take('2001:db8::1'), { ok: true })
  assert.deepEqual(b.take('2001:db8::2'), { ok: true }, 'same /64, its second')
  assert.deepEqual(b.take('2001:db8::3'), { ok: false, limit: 'address', retryAfter: 1 })
  assert.deepEqual(b.take('192.0.2.2'), { ok: true }, 'the fifth of the route burst')
  assert.deepEqual(b.take('192.0.2.3'), { ok: false, limit: 'global', retryAfter: 1 })
  now += 500
  assert.deepEqual(b.take('192.0.2.3'), { ok: true }, 'half a second refills one route token')
  assert.deepEqual(b.take('192.0.2.1'), { ok: false, limit: 'address', retryAfter: 1 })
  now += 1000
  assert.deepEqual(b.take('192.0.2.1'), { ok: true })
})

test('refused handshakes take nothing; Retry-After covers a slow rate', () => {
  let now = 0
  const b = new HandshakeBudget({ perSec: 1, burst: 1, perAddressPerSec: 0.1, addressBurst: 1 }, () => now)
  assert.equal(b.take('192.0.2.1').ok, true)
  for (let i = 0; i < 100; i++) assert.deepEqual(b.take('192.0.2.1'), { ok: false, limit: 'address', retryAfter: 10 })
  now += 1000
  assert.equal(b.take('192.0.2.2').ok, true, 'the route bucket refilled; the refused flood took none of it')
})

test('addresses are forgotten once refilled, and never more than the cap', () => {
  let now = 0
  const b = new HandshakeBudget({ perSec: 1e6, burst: 1e6, perAddressPerSec: 1, addressBurst: 1 }, () => now)
  for (let i = 0; i < 10; i++) b.take(`192.0.2.${i}`)
  assert.equal(b.addresses, 10)
  now += 1000
  b.take('198.51.100.1')
  assert.equal(b.addresses, 1, 'the refilled ones went')
  assert.equal(b.take('198.51.100.1').ok, false, 'the one in use kept its bucket')
  for (let i = 0; i < MaxAddresses + 50; i++) b.take(`10.${(i >> 16) & 255}.${(i >> 8) & 255}.${i & 255}`)
  assert.equal(b.addresses, MaxAddresses)
})

test('a budget must be positive', () => {
  assert.throws(() => new HandshakeBudget({ ...DefaultBudget, perSec: 0 }), /perSec/)
  assert.throws(() => new HandshakeBudget({ ...DefaultBudget, addressBurst: 0.5 }), /burst/)
})

test('a keyed budget: each key its own bucket, refilled at its rate, and never more keys than the cap', () => {
  let now = 0
  const b = new KeyedBudget({ perSec: 2, burst: 3 }, 2, () => now)
  assert.deepEqual([b.take('a'), b.take('a'), b.take('a')], [{ ok: true }, { ok: true }, { ok: true }])
  assert.deepEqual(b.take('a'), { ok: false, retryAfter: 1 })
  assert.deepEqual(b.take('b'), { ok: true }, 'another key is not charged')
  now += 500
  assert.deepEqual(b.take('a'), { ok: true }, 'half a second refills one')
  b.take('c')
  assert.equal(b.size, 2, 'the least recently used key was forgotten')
  assert.throws(() => new KeyedBudget({ perSec: 0, burst: 1 }), /perSec/)
  assert.throws(() => new KeyedBudget({ perSec: 1, burst: 0 }), /burst/)
})

test('seen requests are remembered up to a bound, oldest forgotten first', () => {
  const s = new SeenRequests(3)
  for (const k of ['a', 'b', 'c']) s.add(k)
  assert.ok(s.has('a'))
  s.add('d')
  assert.deepEqual([s.has('a'), s.has('b'), s.has('d'), s.size], [false, true, true, 3])
  assert.throws(() => new SeenRequests(0), /max/)
})
