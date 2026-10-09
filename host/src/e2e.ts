/**
 * The host module end to end on a local reference overlay host, loaded
 * from the bundle (bundle/bbox-module.js) through OVERLAY_MODULES.
 *
 *   node dist/e2e.js /path/to/reference-host
 *
 * The reference host directory holds its compiled dist/index.js and its
 * node_modules. The bundle is staged as a deployment places it: copied into
 * a modules/ directory whose only node_modules is the host's, so it loads
 * with nothing but what the host provides, and the SDK it imports is the
 * host's own copy. The run needs Docker, for a throwaway MySQL (the host's
 * only store), and serves block headers itself in the host's native header
 * shape (/v1/root/<height>, /v1/tip): the golden vectors' and those of a
 * local chain it mines fresh funding trees, sweeps and payment coins on.
 *
 * It submits envelopes, lists them, acknowledges one with a receipt, sweeps
 * another's funding output, replays the suppression attack spec section 8.1
 * guards against, asks a priced class through the terms route and pays its 402,
 * restarts the host, and asks everything again of the restored index.
 * Everything it starts, it stops.
 */
import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync } from 'node:fs'
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http'
import type { AddressInfo } from 'node:net'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { AuthFetch, PrivateKey, Transaction, type WalletInterface } from '@bsv/sdk'
import { LockTime } from '@lightwebinc/bcommon'
import { TestNetwork, serveNetwork } from '@lightwebinc/bcommon/testing'
import { Chain, Party, PayingWallet, carrier, commitment, envelopeRecord, fundingTree, receiptRecord, sweep } from './testmint.js'
import { beefOf, byName, txVectors } from './testutil.js'

const v = txVectors()
const office = v.office
const topic = v.topic
const password = 'bbox-e2e'
const payeeKey = '11'.repeat(32)
const bundle = fileURLToPath(new URL('../bundle/bbox-module.js', import.meta.url))

function fail(msg: string): never {
  throw new Error(`e2e: ${msg}`)
}

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms))

/** Stages the bundle beside the reference host's node_modules; returns the module's path and the root to remove. */
function stage(dir: string): { file: string; root: string } {
  const root = mkdtempSync(join(tmpdir(), 'bbox-e2e-'))
  symlinkSync(resolve(dir, 'node_modules'), join(root, 'node_modules'))
  mkdirSync(join(root, 'modules', 'bbox'), { recursive: true })
  mkdirSync(join(root, 'state'))
  const file = join(root, 'modules', 'bbox', 'bbox-module.js')
  copyFileSync(bundle, file)
  return { file, root }
}

/** The chain's headers, served in the reference host's native shape, read at each request. */
async function headers(chain: Chain, net: TestNetwork): Promise<Server> {
  const server = createServer((req, res) => {
    void serveNetwork(net, req, res).then((served) => {
      if (!served) headersOnly(chain, req, res)
    })
  })
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r))
  return server
}

function headersOnly(chain: Chain, req: IncomingMessage, res: ServerResponse): void {
  {
    const m = /^\/v1\/root\/(\d+)$/.exec(req.url ?? '')
    const tip = Math.max(...chain.roots.keys())
    const body = req.url === '/v1/tip' ? { height: tip } : m !== null && chain.roots.has(Number(m[1])) ? { merkleRoot: chain.roots.get(Number(m[1])) } : undefined
    res.writeHead(body === undefined ? 404 : 200, { 'content-type': 'application/json' })
    res.end(JSON.stringify(body ?? {}))
  }
}

function docker(...args: string[]): string {
  return execFileSync('docker', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim()
}

async function mysql(): Promise<{ id: string; port: number }> {
  const id = docker('run', '-d', '--rm', '-e', `MYSQL_ROOT_PASSWORD=${password}`, '-e', 'MYSQL_DATABASE=overlay', '-p', '127.0.0.1::3306', '--tmpfs', '/var/lib/mysql', 'mysql:9')
  const port = Number(docker('port', id, '3306/tcp').split('\n')[0]!.split(':').pop())
  for (let i = 0; i < 120; i++) {
    try {
      // Over TCP, which the image's initialisation server does not listen on.
      docker('exec', id, 'mysql', '-h127.0.0.1', '-uroot', `-p${password}`, '-e', 'select 1')
      return { id, port }
    } catch {
      await sleep(1000)
    }
  }
  docker('rm', '-f', id)
  fail('MySQL did not come up')
}

async function freePort(): Promise<number> {
  return await new Promise<number>((r) => {
    const s = createServer().listen(0, '127.0.0.1', () => {
      const p = (s.address() as AddressInfo).port
      s.close(() => r(p))
    })
  })
}

interface Host {
  proc: ChildProcess
  url: string
  terms: string
  out: string[]
}

interface Ports {
  db: number
  headers: number
  host: number
  terms: number
}

async function startHost(dir: string, staged: { file: string; root: string }, p: Ports): Promise<Host> {
  const out: string[] = []
  const proc = spawn(process.execPath, [resolve(dir, 'dist/index.js')], {
    env: {
      PATH: process.env['PATH'] ?? '',
      OVERLAY_TOPICS: topic,
      OVERLAY_MODULES: staged.file,
      OVERLAY_KNEX_URL: `mysql://root:${password}@127.0.0.1:${p.db}/overlay`,
      OVERLAY_CHAIN_TRACKER_URL: `http://127.0.0.1:${p.headers}`,
      OVERLAY_ADMIN_TOKEN: 'bbox-e2e',
      OVERLAY_LISTEN: '127.0.0.1',
      OVERLAY_PORT: String(p.host),
      BBOX_OFFICES: office,
      BBOX_STATE_DIR: join(staged.root, 'state'),
      BBOX_LISTEN: `127.0.0.1:${p.terms}`,
      BBOX_PRICES: 'history=5,history-after=5',
      BBOX_PAYEE_KEY: payeeKey,
      BBOX_ARCADE_URL: `http://127.0.0.1:${p.headers}/arcade`,
      BBOX_ASSET_URL: `http://127.0.0.1:${p.headers}`,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  proc.stdout!.on('data', (b: Buffer) => out.push(b.toString()))
  proc.stderr!.on('data', (b: Buffer) => out.push(b.toString()))
  const url = `http://127.0.0.1:${p.host}`
  for (let i = 0; i < 60; i++) {
    if (proc.exitCode !== null) fail(`the host exited ${proc.exitCode}:\n${out.join('')}`)
    try {
      if ((await fetch(`${url}/readyz`)).status === 200) return { proc, url, terms: `http://127.0.0.1:${p.terms}`, out }
    } catch {
      // not listening yet
    }
    await sleep(500)
  }
  proc.kill('SIGKILL')
  fail(`the host did not become ready:\n${out.join('')}`)
}

async function stopHost(h: Host): Promise<void> {
  if (h.proc.exitCode !== null) return
  const done = new Promise((r) => h.proc.once('exit', r))
  h.proc.kill('SIGTERM')
  await done
}

async function submit(h: Host, beef: number[], what: string): Promise<number[]> {
  const res = await fetch(`${h.url}/submit`, {
    method: 'POST',
    headers: { 'content-type': 'application/octet-stream', 'x-topics': topic },
    body: Uint8Array.from(beef),
  })
  const body = (await res.json()) as Record<string, { outputsToAdmit?: number[] }>
  if (res.status !== 200) fail(`submit ${what}: ${res.status} ${JSON.stringify(body)}`)
  return body[topic]?.outputsToAdmit ?? []
}

const txidsOf = (outputs: Array<{ beef: number[] }> | undefined): string[] => (outputs ?? []).map((o) => Transaction.fromBEEF(o.beef).id('hex'))

/** A question to the host's own /lookup: the answer's txids, or its error. */
async function ask(base: string, query: Record<string, unknown>): Promise<string[] | string> {
  const res = await fetch(`${base}/lookup`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ service: 'ls_bbox', query }) })
  const body = (await res.json()) as { outputs?: Array<{ beef: number[] }>; error?: string }
  if (res.status !== 200) return `${res.status} ${body.error ?? ''}`
  return txidsOf(body.outputs)
}

function expect(what: string, got: unknown, want: unknown): void {
  const g = JSON.stringify(got)
  const w = JSON.stringify(want)
  if (g !== w) fail(`${what}: got ${g}, want ${w}`)
  console.log(`ok  ${what}`)
}

function metric(text: string, name: string, ...labels: string[]): string | undefined {
  return text
    .split('\n')
    .find((l) => l.startsWith(name) && labels.every((x) => l.includes(x)))
    ?.trim()
    .split(' ')
    .pop()
}

async function main(): Promise<void> {
  const dir = process.argv[2]
  if (dir === undefined) fail('usage: node dist/e2e.js /path/to/reference-host')
  const chain = new Chain(30000, v.headers)
  const net = new TestNetwork(chain)
  const hdr = await headers(chain, net)
  const db = await mysql()
  const ports: Ports = { db: db.port, headers: (hdr.address() as AddressInfo).port, host: await freePort(), terms: await freePort() }
  const staged = stage(dir)
  let host: Host | undefined
  try {
    host = await startHost(dir, staged, ports)
    const mounted = host.out.join('').split('\n').find((l) => l.includes('bbox module mounted'))
    if (mounted === undefined || !mounted.includes('with @lightwebinc/bcommon')) fail(`the bundle's mount line is missing:\n${host.out.join('')}`)
    console.log("ok  the bundle mounts from beside the host's node_modules")

    const now = Math.floor(Date.now() / 1000)
    const alice = new Party('e2e alice')
    const bob = new Party('e2e bob')
    const aTree = fundingTree(alice, 4, chain)
    const bTree = fundingTree(bob, 1, chain)
    const letter = async (vout: number, box: string, created: number, lock = LockTime): Promise<Transaction> =>
      await carrier(aTree, vout, alice, await envelopeRecord({ office, from: alice, to: bob, box, created, body: `e2e ${vout}` }), lock)
    const e1 = await letter(0, 'inbox', now - 300)
    const e2 = await letter(1, 'files', now - 200)
    const e3 = await letter(2, 'inbox', now - 100)
    const e1b = await letter(0, 'inbox', now - 50, LockTime + 1)
    const id = (t: Transaction): string => t.id('hex')
    const winner1 = [e1, e1b].sort((a, b) => Buffer.compare(Buffer.from(commitment(a)), Buffer.from(commitment(b))))[0]!
    const rc = await carrier(bTree, 0, bob, receiptRecord(office, bob, [commitment(winner1)], now))
    const sw = await sweep(aTree, [2], alice, chain)
    const inbox = { office, to: bob.hex }

    for (const [name, x] of [['e1', e1], ['e2', e2], ['e3', e3], ['a second carrier on e1\'s funding output', e1b]] as const) {
      expect(`submit ${name}`, await submit(host, x.toAtomicBEEF(), name), [0])
    }
    const created = new Map([[e1, now - 300], [e2, now - 200], [e3, now - 100], [e1b, now - 50]])
    const byCreated = [winner1, e2, e3].sort((a, b) => created.get(a)! - created.get(b)!).map(id)
    expect('inbox: one answered carrier per funding output, by created', await ask(host.url, inbox), byCreated)
    expect('box inbox', (await ask(host.url, { ...inbox, box: 'inbox' })).length, 2)
    expect('sender', (await ask(host.url, { ...inbox, from: alice.hex })).length, 3)

    expect('submit the receipt', await submit(host, rc.toAtomicBEEF(), 'receipt'), [0])
    expect('inbox after the receipt', await ask(host.url, inbox), [id(e2), id(e3)])
    expect('receipt class', await ask(host.url, { office, by: bob.hex, receiptFor: id(winner1) }), [id(rc)])

    expect('submit the sweep of e3\'s funding output', await submit(host, sw.toAtomicBEEF(), 'sweep'), [0])
    expect('inbox after the sweep', await ask(host.url, inbox), [id(e2)])
    expect('sweep class', await ask(host.url, { office, spent: `${id(aTree)}.2` }), [id(sw)])

    // The suppression attack: the golden note carrier offered first in a
    // BEEF whose funding tree is unproven. The manager raises, the host
    // records nothing, and the genuine carrier is admitted after it.
    expect('the note in a worse BEEF admits nothing', await submit(host, beefOf(byName(v, 'carrier-beef-tree-unproven')), 'worse note'), [])
    expect('the genuine note after it is admitted', await submit(host, beefOf(byName(v, 'carrier-envelope-note')), 'note'), [0])
    const metrics = await (await fetch(`${host.url}/metrics`)).text()
    expect('the refusal is counted by reason', metric(metrics, 'bbox_refused_total', 'reason="beef"'), '1')
    expect('the host counts it as one failed topic admission', metric(metrics, 'overlay_host_topic_failures_total', topic), '1')

    // Priced: refused on the host's own route, 402 then answered on the terms route.
    const refused = await ask(host.url, { office, history: bob.hex })
    if (typeof refused !== 'string' || !refused.includes('priced')) fail(`history on the host route: ${JSON.stringify(refused)}`)
    console.log('ok  history on the host route is refused: it is priced')
    const terms = (await (await fetch(`${host.terms}/ls_bbox/terms`)).json()) as unknown
    expect('the terms document', terms, { service: 'ls_bbox', terms: 1, classes: [{ class: 'history', satoshis: 5 }, { class: 'history-after', satoshis: 5 }] })
    expect('a free class on the terms route', await ask(host.terms, inbox), [id(e2)])
    expect('history on the terms route without BRC-104', await ask(host.terms, { office, history: bob.hex }), '401 ')
    const wallet = new PayingWallet(PrivateKey.fromHex('22'.repeat(32)), chain)
    const paidHistory = async (who: string): Promise<{ status: number; paid: string | null; txids: string[] }> => {
      const saved = { warn: console.warn, info: console.info }
      console.warn = () => {}
      console.info = () => {}
      try {
        const res = await new AuthFetch(wallet as unknown as WalletInterface).fetch(`${host!.terms}/lookup`, {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ service: 'ls_bbox', query: { office, history: who } }),
        })
        const body = (await res.json()) as { outputs?: Array<{ beef: number[] }> }
        return { status: res.status, paid: res.headers.get('x-bsv-payment-satoshis-paid'), txids: txidsOf(body.outputs) }
      } finally {
        Object.assign(console, saved)
      }
    }
    expect('history paid through its 402', await paidHistory(bob.hex), { status: 200, paid: '5', txids: [id(winner1)] })
    expect('the golden note, out of the window, is history for its recipient', (await paidHistory(v.recipientIdentityKey)).txids, [byName(v, 'carrier-envelope-note').txid])
    const ledger = join(staged.root, 'state', 'payments.jsonl')
    const lines = readFileSync(ledger, 'utf8').trim().split('\n').map((l) => JSON.parse(l) as { txid: string; decision?: string })
    expect('both payments are in the ledger, taken fast', lines.map((l) => l.decision), ['fast', 'fast'])
    expect('the host broadcast both before answering', lines.every((l) => net.sent.includes(l.txid)), true)

    // Restart: the index is rebuilt from storage and the outpoint rows.
    await stopHost(host)
    host = await startHost(dir, staged, ports)
    if (!host.out.join('').includes('module lookup restored from storage')) fail(`no restore line:\n${host.out.join('')}`)
    expect('restored: inbox', await ask(host.url, inbox), [id(e2)])
    expect('restored: receipt class', await ask(host.url, { office, by: bob.hex, receiptFor: id(winner1) }), [id(rc)])
    expect('restored: sweep class', await ask(host.url, { office, spent: `${id(aTree)}.2` }), [id(sw)])
    expect('restored: history paid again', await paidHistory(bob.hex), { status: 200, paid: '5', txids: [id(winner1)] })
    expect('restored: a replayed carrier is a duplicate', await submit(host, e1b.toAtomicBEEF(), 'e1b again'), [])
    expect('restored: still one answered carrier on e1\'s funding output', await ask(host.url, { ...inbox, box: 'inbox' }), [])
    console.log('e2e: PASS')
  } finally {
    if (host !== undefined) await stopHost(host)
    docker('rm', '-f', db.id)
    hdr.close()
    rmSync(staged.root, { recursive: true, force: true })
  }
}

main().catch((e: unknown) => {
  console.error(e instanceof Error ? e.message : e)
  process.exit(1)
})
