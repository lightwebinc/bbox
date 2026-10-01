/**
 * The compiled module as a reference host loads it: imported by absolute
 * path, its default export called with the host's logger and metrics, the
 * result held to the loader's shape rules, and its configuration refused
 * rather than guessed.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import type { Module, ModuleHost } from '@lightwebinc/bcommon'
import { parseConfig, parseOffices } from './module.js'
import { FakeHost } from './testutil.js'

const office = 'example_office_qzxkvbmwtr'
const other = 'support_mkvbqzxtrw'

test('BBOX_OFFICES: office identifiers, once each; anything else refused', () => {
  assert.deepEqual(parseOffices(` ${office},${other} `), [office, other])
  for (const bad of [undefined, '', 'example_office', `${office},${office}`, 'Example_office_qzxkvbmwtr', `${office},`]) {
    assert.throws(() => parseOffices(bad), JSON.stringify(bad))
  }
})

test('the configuration: a state directory is required, floors hold, and a price needs its route, payee and headers', () => {
  const base = { BBOX_OFFICES: office, BBOX_STATE_DIR: '/var/lib/bbox' }
  const c = parseConfig(base)
  assert.deepEqual([c.retentionDays, c.maxBEEF, c.prices.size, c.listen], [31, 262144, 0, undefined])
  assert.throws(() => parseConfig({ BBOX_OFFICES: office }), /BBOX_STATE_DIR/)
  assert.throws(() => parseConfig({ ...base, BBOX_STATE_DIR: 'relative' }), /BBOX_STATE_DIR/)
  assert.throws(() => parseConfig({ ...base, BBOX_RETENTION_DAYS: '30' }), /at least 31/)
  assert.throws(() => parseConfig({ ...base, BBOX_MAX_BEEF: '1000' }), /at least 262144/)
  assert.throws(() => parseConfig({ ...base, BBOX_LISTEN: 'nowhere' }), /host:port/)
  assert.throws(() => parseConfig({ ...base, BBOX_PAYEE_KEY: 'AB'.repeat(32) }), /64 lowercase hex/)
  assert.throws(() => parseConfig({ ...base, BBOX_PRICES: 'history=5' }), /BBOX_LISTEN/)
  assert.throws(() => parseConfig({ ...base, BBOX_PRICES: 'history=0' }), /BBOX_LISTEN/)
  const priced = parseConfig({
    ...base,
    BBOX_PRICES: 'history=5',
    BBOX_LISTEN: '[::1]:8081',
    BBOX_PAYEE_KEY: '11'.repeat(32),
    OVERLAY_CHAIN_TRACKER_URL: 'http://127.0.0.1:1',
  })
  assert.deepEqual([priced.listen, priced.headersURL, priced.prices.get('history')], [{ host: '::1', port: 8081 }, 'http://127.0.0.1:1', 5])
  assert.equal(parseConfig({ ...base, BBOX_RETENTION_DAYS: '90', BBOX_MAX_BEEF: '1048576' }).retentionDays, 90)
  assert.deepEqual(c.sessions, { max: 10000, ttlSeconds: 600 }, 'the BRC-104 session bound has a default')
  assert.deepEqual(parseConfig({ ...base, BBOX_SESSIONS: '50', BBOX_SESSION_TTL: '60' }).sessions, { max: 50, ttlSeconds: 60 })
  assert.throws(() => parseConfig({ ...base, BBOX_SESSIONS: '0' }), /BBOX_SESSIONS/)
  assert.throws(() => parseConfig({ ...base, BBOX_SESSION_TTL: 'soon' }), /BBOX_SESSION_TTL/)
  assert.deepEqual(c.handshakes, { perSec: 4, burst: 8, perAddressPerSec: 1, addressBurst: 4 }, 'the handshake budget has a default')
  assert.deepEqual(
    parseConfig({ ...base, BBOX_HANDSHAKES_PER_SEC: '2.5', BBOX_HANDSHAKE_BURST: '5', BBOX_HANDSHAKES_PER_ADDR_PER_SEC: '0.5', BBOX_HANDSHAKE_ADDR_BURST: '2' }).handshakes,
    { perSec: 2.5, burst: 5, perAddressPerSec: 0.5, addressBurst: 2 },
  )
  for (const [k, v] of [['BBOX_HANDSHAKES_PER_SEC', '0'], ['BBOX_HANDSHAKES_PER_SEC', '-1'], ['BBOX_HANDSHAKES_PER_ADDR_PER_SEC', 'fast'], ['BBOX_HANDSHAKE_BURST', '0'], ['BBOX_HANDSHAKE_ADDR_BURST', '1.5']]) {
    assert.throws(() => parseConfig({ ...base, [k!]: v }), new RegExp(k!))
  }
})

/** The reference loader's checks on what a factory returned. */
function loaderShape(m: unknown): Module {
  assert.ok(typeof m === 'object' && m !== null)
  const mod = m as Module
  assert.ok(Object.keys(mod.topics ?? {}).length + Object.keys(mod.lookups ?? {}).length > 0, 'mounts something')
  for (const tm of Object.values(mod.topics ?? {})) assert.equal(typeof tm.identifyAdmissibleOutputs, 'function')
  for (const ls of Object.values(mod.lookups ?? {})) {
    assert.equal(typeof ls.lookup, 'function')
    assert.equal(typeof ls.outputAdmittedByTopic, 'function')
    assert.equal(typeof ls.admissionMode, 'string')
    assert.equal(typeof ls.restore, 'function', 'the index is in memory, so the module must restore')
  }
  return mod
}

test('dist/module.js loads by absolute path, as OVERLAY_MODULES names it, and mounts each office and ls_bbox', async () => {
  const path = fileURLToPath(new URL('./module.js', import.meta.url))
  const imported = (await import(pathToFileURL(path).href)) as { default: (h: ModuleHost) => Promise<Module> }
  assert.equal(typeof imported.default, 'function')
  const keys = ['BBOX_OFFICES', 'BBOX_STATE_DIR', 'OVERLAY_TOPICS'] as const
  const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]))
  const dir = mkdtempSync(join(tmpdir(), 'bbox-module-'))
  try {
    process.env['BBOX_OFFICES'] = `${office},${other}`
    process.env['BBOX_STATE_DIR'] = dir
    process.env['OVERLAY_TOPICS'] = `tm_anytx,tm_bbox_${office},tm_bbox_${other}`
    const host = new FakeHost()
    const mod = loaderShape(await imported.default(host))
    assert.deepEqual(Object.keys(mod.topics!).sort(), [`tm_bbox_${office}`, `tm_bbox_${other}`])
    assert.deepEqual(Object.keys(mod.lookups!), ['ls_bbox'])
    assert.ok(host.counters.has('bbox_refused_total{reason="beef"}'), 'counters are preset')
    assert.ok(host.gauges.has('bbox_outpoint_rows'))
    // A tm_bbox_ topic the host carries but the module does not configure
    // would fall to the admit-everything default: refused.
    process.env['OVERLAY_TOPICS'] = `tm_bbox_${office},tm_bbox_third_mkvbqzxtrw`
    await assert.rejects(imported.default(new FakeHost()), /tm_bbox_third_mkvbqzxtrw/)
    delete process.env['BBOX_OFFICES']
    await assert.rejects(imported.default(new FakeHost()), /BBOX_OFFICES/)
  } finally {
    for (const k of keys) {
      if (saved[k] === undefined) delete process.env[k]
      else process.env[k] = saved[k]
    }
    rmSync(dir, { recursive: true, force: true })
  }
})
