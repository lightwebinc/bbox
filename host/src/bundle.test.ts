/**
 * The shipped file rather than the tsc tree: bundle/bbox-module.js as the
 * bundle step leaves it, mounted the way the host mounts a module, admitting
 * golden carriers and a sweep and answering ls_bbox. Every other test runs
 * on dist/, which resolves the library from node_modules, so only this one
 * proves the bundle works with the library inlined and nothing but
 * @bsv/sdk and Node's built-ins beside it.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { Module, ModuleHost } from '@lightwebinc/bcommon'
import { FakeHost, beefOf, byName, heldOf, txOf, txVectors } from './testutil.js'

const v = txVectors()
// dist/bundle.test.js -> host/. Imported by URL, so the type checker does not
// go looking for a file that exists only after the bundle step.
const bundleUrl = new URL('../bundle/bbox-module.js', import.meta.url)

function version(pkg: string): string {
  const url = new URL(`../node_modules/${pkg}/package.json`, import.meta.url)
  return (JSON.parse(readFileSync(url, 'utf8')) as { version: string }).version
}

async function mount(host: FakeHost, dir: string): Promise<Module> {
  const { default: create } = (await import(bundleUrl.href)) as { default: (host: ModuleHost) => Promise<Module> }
  const keys = ['BBOX_OFFICES', 'BBOX_STATE_DIR', 'OVERLAY_TOPICS'] as const
  const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]))
  try {
    process.env['BBOX_OFFICES'] = v.office
    process.env['BBOX_STATE_DIR'] = dir
    process.env['OVERLAY_TOPICS'] = `tm_anytx,${v.topic}`
    return await create(host)
  } finally {
    for (const k of keys) {
      if (saved[k] === undefined) delete process.env[k]
      else process.env[k] = saved[k]
    }
  }
}

test('the shipped bundle admits golden carriers and a sweep, refuses by raising, and ls_bbox answers them', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'bbox-bundle-'))
  try {
    const host = new FakeHost()
    const mod = await mount(host, dir)
    const tm = mod.topics?.[v.topic]
    const ls = mod.lookups?.['ls_bbox']
    if (tm === undefined || ls === undefined) throw new Error('the bundle did not mount the office and ls_bbox')
    for (const name of ['carrier-envelope-payment', 'carrier-envelope-refs', 'carrier-receipt', 'sweep']) {
      const t = byName(v, name)
      assert.deepEqual(await tm.identifyAdmissibleOutputs(beefOf(t), heldOf(t)), { outputsToAdmit: t.admits!.outputsToAdmit, coinsToRetain: t.admits!.coinsToRetain }, name)
      await ls.outputAdmittedByTopic({ mode: 'whole-tx', atomicBEEF: txOf(t).toAtomicBEEF(), outputIndex: 0, topic: v.topic })
    }
    assert.equal(host.count('bbox_admitted_total', { kind: 'envelope' }), 2)
    // A refusal raises, with the label the tsc tree counts under.
    await assert.rejects(tm.identifyAdmissibleOutputs(beefOf(byName(v, 'carrier-final-input')), []), /mineable/)
    assert.equal(host.count('bbox_refused_total', { reason: 'mineable' }), 1)
    // The sweep retracted the refs envelope; the payment envelope is out of the window, so history answers it.
    const answer = await ls.lookup({ service: 'ls_bbox', query: { office: v.office, history: v.recipientIdentityKey } })
    assert.deepEqual(answer.map((f) => f.txid), [byName(v, 'carrier-envelope-payment').txid])
    const receipt = await ls.lookup({ service: 'ls_bbox', query: { office: v.office, by: v.recipientIdentityKey, receiptFor: byName(v, 'carrier-envelope-refs').txid } })
    assert.deepEqual(receipt.map((f) => f.txid), [byName(v, 'carrier-receipt').txid])
    assert.equal(host.gauges.get('bbox_envelopes')?.(), 2)
    assert.ok(existsSync(join(dir, 'outpoints.jsonl')), 'the outpoint rows are on disk')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('the bundle names its versions in its banner and in the host log', async () => {
  const banner = readFileSync(bundleUrl, 'utf8').split('\n', 1)[0] ?? ''
  assert.ok(banner.startsWith('// @lightwebinc/bbox-host '), banner)
  assert.ok(banner.includes(`@lightwebinc/bcommon ${version('@lightwebinc/bcommon')} inlined`), banner)
  assert.ok(banner.includes(`imports @bsv/sdk (built against ${version('@bsv/sdk')})`), banner)
  const dir = mkdtempSync(join(tmpdir(), 'bbox-bundle-'))
  try {
    const host = new FakeHost()
    await mount(host, dir)
    const mounted = host.lines.filter((l) => l.msg === 'bbox module mounted')
    assert.equal(mounted.length, 1)
    assert.equal(`// ${String(mounted[0]?.extra?.['build'])}`, banner)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('the bundle imports nothing but @bsv/sdk and Node built-ins, and carries no source map', () => {
  const text = readFileSync(bundleUrl, 'utf8')
  const specifiers = new Set([...text.matchAll(/^\s*(?:import|export)\b[^'"]*?from\s*["']([^"']+)["']/gm)].map((m) => m[1]!))
  assert.ok(specifiers.has('@bsv/sdk'))
  assert.deepEqual([...specifiers].filter((s) => s !== '@bsv/sdk' && !s.startsWith('node:')), [])
  assert.equal(/\bimport\s*\(|\brequire\s*\(/.test(text), false, 'no dynamic import or require')
  assert.equal(text.includes('sourceMappingURL'), false)
  assert.equal(existsSync(new URL('../bundle/bbox-module.js.map', import.meta.url)), false)
})
