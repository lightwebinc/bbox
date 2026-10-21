/**
 * Gap A13 harness: can topic B's manager read topic A's outputs stably at
 * admission? Runs repeated trials of each condition against one of two
 * backends and prints, per condition, what B decided and what every read
 * path saw (seam-module.mjs names the paths p1..p4).
 *
 *   engine   @bsv/overlay 2.3.1 Engine in this process, on the engine's own
 *            KnexStorage over MySQL. Restore is a new Engine and a new module
 *            over the same database. Eviction is reachable here only.
 *   refhost  the v0.2.2 reference host (A13_REFHOST: its directory, built,
 *            with its node_modules) as child processes on MySQL, the
 *            module loaded by OVERLAY_MODULES, submits over HTTP. Restore is
 *            a process restart. GASP is two hosts, the second syncing from
 *            the first through /admin/startGASPSync.
 *
 * Needs Node 24 and a throwaway MySQL. Not part of `make verify`.
 *
 *   docker run -d --name a13-mysql -e MYSQL_ROOT_PASSWORD=a13 \
 *     --tmpfs /var/lib/mysql -p 127.0.0.1:13313:3306 mysql:9
 *   (cd host/a13-seam && npm ci)
 *   node24 host/a13-seam/run.mjs --backend engine --trials 50 --out engine.json
 *   A13_REFHOST=/path/to/reference-host \
 *     node24 host/a13-seam/run.mjs --backend refhost --trials 20 --out refhost.json
 *   docker rm -f a13-mysql
 *
 * A13_MYSQL overrides the server URL (default the container above).
 */
import { spawn } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { readFileSync, writeFileSync, existsSync, createWriteStream } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'
import knexLib from 'knex'
import { Engine, KnexStorage, KnexStorageMigrations } from '@bsv/overlay'
import { seamModule, POL, ENV } from './seam-module.mjs'
import { Chain, policyTx, envelopeTx, policySpend, combinedTx, beef } from './mint.mjs'

const here = dirname(fileURLToPath(import.meta.url))
const { values: args } = parseArgs({
  options: {
    backend: { type: 'string', default: 'engine' },
    trials: { type: 'string', default: '20' },
    only: { type: 'string' },
    out: { type: 'string' },
  },
})
const N = Number(args.trials)
const MYSQL = process.env.A13_MYSQL ?? 'mysql://root:a13@127.0.0.1:13313'
const REFHOST = process.env.A13_REFHOST
if (args.backend === 'refhost' && (REFHOST === undefined || REFHOST === '')) {
  console.error('a13: set A13_REFHOST to the reference host directory (built, with its node_modules)')
  process.exit(2)
}
const RUN = randomBytes(3).toString('hex')
const work = join(tmpdir(), `a13-${RUN}`)
const quiet = Object.assign(Object.create(console), { log() {}, info() {}, warn() {}, debug() {}, error() {} })

const migrations = KnexStorageMigrations.default ?? KnexStorageMigrations
class Migrations {
  async getMigrations() { return migrations }
  getMigrationName(m) { return `engine-${String(migrations.indexOf(m)).padStart(4, '0')}` }
  async getMigration(m) { return m }
}

const root = knexLib({ client: 'mysql2', connection: MYSQL, pool: { min: 0, max: 2 } })
async function freshDB(name) {
  await root.raw(`DROP DATABASE IF EXISTS \`${name}\``)
  await root.raw(`CREATE DATABASE \`${name}\``)
}
const dbs = new Map()
function dbFor(name) {
  if (!dbs.has(name)) dbs.set(name, knexLib({ client: 'mysql2', connection: `${MYSQL}/${name}`, pool: { min: 0, max: 10 } }))
  return dbs.get(name)
}
/** Whether output vout of txid is held for topic on a node, read from its database. */
async function held(dbName, txid, topic, vout = 0) {
  const r = await dbFor(dbName)('outputs').where({ txid, outputIndex: vout, topic }).first()
  return r !== undefined
}

const admittedIn = (steak, topic) => (steak?.[topic]?.outputsToAdmit?.length ?? 0) > 0

// ---------------------------------------------------------------- backends

class EngineNode {
  constructor(name, chain, opts = {}) {
    this.name = name
    this.chain = chain
    this.opts = opts
    this.dbName = `a13_${RUN}_${name}`
    this.records = []
  }
  async start(fresh = true) {
    if (fresh) await freshDB(this.dbName)
    const db = dbFor(this.dbName)
    await db.migrate.latest({ migrationSource: new Migrations() })
    this.storage = new KnexStorage(db)
    const { module, state } = seamModule({ node: this.name, ...this.opts })
    this.state = state
    state.records = this.records // one record list across restarts
    // As the reference host does at startup: the module's unspent rows, with
    // BEEF, and the storage object itself.
    const rows = []
    for (const t of Object.keys(module.topics)) rows.push(...(await this.storage.findUTXOsForTopic(t, undefined, undefined, true)))
    if (this.opts.noRestoreHook !== true) await module.lookups[Object.keys(module.lookups)[0]].restore(rows, this.storage)
    this.engine = new Engine(module.topics, module.lookups, this.storage, this.chain.tracker, undefined, [], [], undefined, undefined,
      Object.fromEntries(Object.keys(module.topics).map((t) => [t, false])), false, undefined, undefined, undefined, quiet)
    state.engine = this.engine
  }
  async restart() { await this.start(false) }
  async stop() {}
  async submit(tx, topics) {
    try {
      return { steak: await this.engine.submit({ beef: beef(tx), topics }) }
    } catch (e) {
      return { error: String(e.message ?? e) }
    }
  }
  async evict(txid) { return await this.engine.evictAppliedTransaction(txid) }
  async recordsFor(txid) { return this.records.filter((r) => r.txid === txid) }
}

const nodeBin = process.execPath
let nextPort = 18700 + Math.floor(Math.random() * 500)

class RefNode {
  constructor(name, chain, opts = {}) {
    this.name = name
    this.chain = chain
    this.opts = opts
    this.dbName = `a13_${RUN}_${name}`
    this.port = nextPort++
    this.url = `http://127.0.0.1:${this.port}`
    this.log = join(work, `${name}.decisions.jsonl`)
    this.token = randomBytes(16).toString('hex')
  }
  async start(fresh = true) {
    if (fresh) await freshDB(this.dbName)
    const topics = this.opts.topics ?? [POL, ENV]
    const env = {
      PATH: process.env.PATH,
      OVERLAY_TOPICS: topics.join(','),
      OVERLAY_KNEX_URL: `${MYSQL}/${this.dbName}`,
      OVERLAY_CHAIN_TRACKER_URL: this.opts.headers,
      OVERLAY_ADMIN_TOKEN: this.token,
      OVERLAY_PORT: String(this.port),
      OVERLAY_LISTEN: '127.0.0.1',
      OVERLAY_MODULES: join(here, 'seam-module.mjs'),
      OVERLAY_MAX_LOOKUP_RESULTS: '-1',
      A13_NODE: this.name,
      A13_LOG: this.log,
      A13_SELF_URL: this.url,
      A13_PATH: this.opts.path ?? 'p1',
      A13_REFUSE: this.opts.refuse ?? 'throw',
      A13_RETAIN: this.opts.retain ? '1' : '0',
    }
    if (this.opts.peer !== undefined) env.OVERLAY_SYNC_PEERS = topics.map((t) => `${t}=${this.opts.peer}`).join(';')
    const out = createWriteStream(join(work, `${this.name}.host.log`), { flags: 'a' })
    this.proc = spawn(nodeBin, [join(REFHOST, 'dist/index.js')], { env, stdio: ['ignore', 'pipe', 'pipe'] })
    this.proc.stdout.pipe(out)
    this.proc.stderr.pipe(out)
    this.exited = new Promise((r) => this.proc.once('exit', r))
    for (let i = 0; i < 200; i++) {
      try {
        const r = await fetch(`${this.url}/readyz`)
        if (r.status === 200) return
      } catch {}
      if (this.proc.exitCode !== null) throw new Error(`${this.name} exited ${this.proc.exitCode}; see ${work}/${this.name}.host.log`)
      await new Promise((r) => setTimeout(r, 100))
    }
    throw new Error(`${this.name} not ready`)
  }
  async stop() {
    if (this.proc?.exitCode === null) {
      this.proc.kill('SIGTERM')
      await this.exited
    }
  }
  async restart() {
    await this.stop()
    await this.start(false)
  }
  async submit(tx, topics) {
    const res = await fetch(`${this.url}/submit`, { method: 'POST', headers: { 'content-type': 'application/octet-stream', 'x-topics': JSON.stringify(topics) }, body: Buffer.from(beef(tx)) })
    const body = await res.json()
    return res.status === 200 ? { steak: body } : { error: body.error }
  }
  async sync() {
    const res = await fetch(`${this.url}/admin/startGASPSync`, { method: 'POST', headers: { authorization: `Bearer ${this.token}` } })
    return { status: res.status, body: await res.json() }
  }
  async evict() { return 'unreachable' }
  async recordsFor(txid) {
    if (!existsSync(this.log)) return []
    return readFileSync(this.log, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l)).filter((r) => r.txid === txid)
  }
}

// ---------------------------------------------------------------- tallies

class Tally {
  constructor() { this.n = {} }
  add(k, by = 1) { this.n[k] = (this.n[k] ?? 0) + by }
  /** What each read path saw on the record of a non-dry call. */
  seen(prefix, recs) {
    const r = recs.filter((x) => !x.dryRun).at(-1)
    if (r === undefined) return this.add(`${prefix}.no-call`)
    for (const [p, v] of Object.entries(r.seen)) this.add(`${prefix}.${p}=${v}`)
    if (new Set(Object.values(r.seen)).size > 1) this.add(`${prefix}.paths-disagree`)
  }
}

const recipient = () => `r-${randomBytes(6).toString('hex')}`

// ---------------------------------------------------------------- scenarios

const scenarios = {
  /** A, then B once A's submit returned. The control. */
  async sequential(ctx) {
    const t = new Tally()
    const a = await ctx.node('seq')
    for (let i = 0; i < N; i++) {
      const r = recipient()
      const p = policyTx(ctx.chain, r)
      await a.submit(p, [POL])
      const e = envelopeTx(ctx.chain, r, p)
      const res = await a.submit(e, [ENV])
      t.add(admittedIn(res.steak, ENV) ? 'admitted' : 'refused')
      t.seen('seen', await a.recordsFor(e.id('hex')))
    }
    return t.n
  },

  /** A and B submitted at the same moment, as two clients would. */
  async concurrentPair(ctx) {
    const t = new Tally()
    const a = await ctx.node('pair')
    for (let i = 0; i < N; i++) {
      const r = recipient()
      const p = policyTx(ctx.chain, r)
      const e = envelopeTx(ctx.chain, r, p)
      const [, res] = await Promise.all([a.submit(p, [POL]), a.submit(e, [ENV])])
      t.add(admittedIn(res.steak, ENV) ? 'admitted' : 'refused')
      t.seen('seen', await a.recordsFor(e.id('hex')))
    }
    return t.n
  },

  /** A with eight B's in flight, B's spread over 0..20 ms after A starts. */
  async concurrentBurst(ctx) {
    const t = new Tally()
    const a = await ctx.node('burst')
    for (let i = 0; i < N; i++) {
      const r = recipient()
      const p = policyTx(ctx.chain, r)
      const es = Array.from({ length: 8 }, () => envelopeTx(ctx.chain, r, p))
      const sleep = (ms) => new Promise((x) => setTimeout(x, ms))
      const results = await Promise.all([a.submit(p, [POL]), ...es.map(async (e, k) => { await sleep(k * 2.5); return await a.submit(e, [ENV]) })])
      const admitted = results.slice(1).filter((x) => admittedIn(x.steak, ENV)).length
      for (const e of es) t.seen('seen', await a.recordsFor(e.id('hex')))
      t.add('envelopes', 8)
      t.add('admitted', admitted)
      t.add(`trials-with-${admitted}-of-8`)
    }
    return t.n
  },

  /** A and B outputs in ONE transaction submitted to both topics. */
  async sameTx(ctx) {
    const t = new Tally()
    const a = await ctx.node('same', { path: 'p2' })
    for (let i = 0; i < N; i++) {
      const r = recipient()
      const c = combinedTx(ctx.chain, r)
      const res = await a.submit(c, [POL, ENV])
      t.add(admittedIn(res.steak, POL) ? 'policy-admitted' : 'policy-refused')
      t.add(admittedIn(res.steak, ENV) ? 'envelope-admitted' : 'envelope-refused')
      t.seen('seen', await a.recordsFor(c.id('hex')))
    }
    return t.n
  },

  /** B arrives before A, then is resubmitted once A is held. */
  async outOfOrder(ctx) {
    const out = {}
    for (const refuse of ['throw', 'empty']) {
      const t = new Tally()
      const a = await ctx.node(`ooo-${refuse}`, { refuse })
      for (let i = 0; i < N; i++) {
        const r = recipient()
        const p = policyTx(ctx.chain, r)
        const e = envelopeTx(ctx.chain, r, p)
        const first = await a.submit(e, [ENV])
        t.add(admittedIn(first.steak, ENV) ? 'first-admitted' : 'first-refused')
        await a.submit(p, [POL])
        const retry = await a.submit(e, [ENV])
        t.add(admittedIn(retry.steak, ENV) ? 'retry-admitted' : 'retry-refused')
        const calls = (await a.recordsFor(e.id('hex'))).length
        t.add(`manager-calls-${calls}`)
        t.add((await held(a.dbName, e.id('hex'), ENV)) ? 'held-after-retry' : 'not-held-after-retry')
      }
      out[`refuse=${refuse}`] = t.n
    }
    return out
  },

  /** A admitted, node restarted from its database, then B. */
  async restore(ctx) {
    const t = new Tally()
    const a = await ctx.node('restore')
    const per = Math.max(1, Math.ceil(N / 5))
    for (let cycle = 0; cycle < 5; cycle++) {
      const ps = []
      for (let i = 0; i < per; i++) {
        const r = recipient()
        const p = policyTx(ctx.chain, r)
        await a.submit(p, [POL])
        ps.push([r, p])
      }
      await a.restart()
      t.add('restarts')
      for (const [r, p] of ps) {
        const e = envelopeTx(ctx.chain, r, p)
        const res = await a.submit(e, [ENV])
        t.add(admittedIn(res.steak, ENV) ? 'admitted' : 'refused')
        t.seen('seen', await a.recordsFor(e.id('hex')))
      }
    }
    return t.n
  },

  /**
   * As restore, on a host that does NOT call a module lookup's restore()
   * (a stock host): engine backend only.
   */
  async restoreNoHook(ctx) {
    if (ctx.backend !== 'engine') return { unreachable: 'the reference host always calls restore()' }
    const t = new Tally()
    const a = await ctx.node('nohook', { noRestoreHook: true, path: 'p3' })
    for (let i = 0; i < N; i++) {
      const r = recipient()
      const p = policyTx(ctx.chain, r)
      await a.submit(p, [POL])
      await a.restart()
      const e = envelopeTx(ctx.chain, r, p)
      const res = await a.submit(e, [ENV])
      t.add(admittedIn(res.steak, ENV) ? 'admitted' : 'refused')
      t.seen('seen', await a.recordsFor(e.id('hex')))
    }
    return t.n
  },

  /** A spent (replaced by a scope "none" policy) before B; and the race of the two. */
  async spent(ctx) {
    const out = {}
    for (const retain of [false, true]) {
      const t = new Tally()
      const a = await ctx.node(`spent-${retain ? 'retain' : 'drop'}`, { retain })
      for (let i = 0; i < N; i++) {
        const r = recipient()
        const p = policyTx(ctx.chain, r)
        await a.submit(p, [POL])
        await a.submit(policySpend(ctx.chain, r, p, 'none'), [POL])
        const e = envelopeTx(ctx.chain, r, p)
        const res = await a.submit(e, [ENV])
        t.add(admittedIn(res.steak, ENV) ? 'after-spend-admitted' : 'after-spend-refused')
        t.seen('after-spend-seen', await a.recordsFor(e.id('hex')))
        // The race: the spend and an envelope naming the old policy at once.
        const r2 = recipient()
        const p2 = policyTx(ctx.chain, r2)
        await a.submit(p2, [POL])
        const e2 = envelopeTx(ctx.chain, r2, p2)
        const [, res2] = await Promise.all([a.submit(policySpend(ctx.chain, r2, p2, 'none'), [POL]), a.submit(e2, [ENV])])
        t.add(admittedIn(res2.steak, ENV) ? 'race-admitted' : 'race-refused')
      }
      out[retain ? 'A retains spent' : 'A drops spent'] = t.n
    }
    return out
  },

  /** A evicted after B was admitted against it (engine backend only). */
  async evict(ctx) {
    const t = new Tally()
    const a = await ctx.node('evict')
    for (let i = 0; i < N; i++) {
      const r = recipient()
      const p = policyTx(ctx.chain, r)
      await a.submit(p, [POL])
      const e1 = envelopeTx(ctx.chain, r, p)
      t.add(admittedIn((await a.submit(e1, [ENV])).steak, ENV) ? 'before-evict-admitted' : 'before-evict-refused')
      const ev = await a.evict(p.id('hex'))
      if (ev === 'unreachable') return { unreachable: 'the reference host has no eviction route' }
      const e2 = envelopeTx(ctx.chain, r, p)
      const res = await a.submit(e2, [ENV])
      t.add(admittedIn(res.steak, ENV) ? 'after-evict-admitted' : 'after-evict-refused')
      t.seen('after-evict-seen', await a.recordsFor(e2.id('hex')))
      t.add((await held(a.dbName, e1.id('hex'), ENV)) ? 'earlier-envelope-still-held' : 'earlier-envelope-gone')
      const back = await a.submit(p, [POL])
      t.add(admittedIn(back.steak, POL) ? 'policy-resubmit-readmitted' : 'policy-resubmit-not-readmitted')
    }
    return t.n
  },

  /**
   * GASP: node "src" admits envelopes against live policies, and some whose
   * policy is spent afterwards; a fresh node syncs from it with each topic
   * order, twice. Is B's admission reproduced on the syncing peer?
   */
  async gasp(ctx) {
    if (ctx.backend !== 'refhost') return { unreachable: 'GASP is measured on the reference host backend' }
    const out = {}
    const src = await ctx.node('gsrc')
    const live = []
    const spentAfter = []
    for (let i = 0; i < N; i++) {
      const r = recipient()
      const p = policyTx(ctx.chain, r)
      await src.submit(p, [POL])
      const e = envelopeTx(ctx.chain, r, p)
      if (!admittedIn((await src.submit(e, [ENV])).steak, ENV)) throw new Error('source did not admit a live envelope')
      live.push(e.id('hex'))
      const r2 = recipient()
      const p2 = policyTx(ctx.chain, r2)
      await src.submit(p2, [POL])
      const e2 = envelopeTx(ctx.chain, r2, p2)
      if (!admittedIn((await src.submit(e2, [ENV])).steak, ENV)) throw new Error('source did not admit an envelope')
      await src.submit(policySpend(ctx.chain, r2, p2, 'none'), [POL])
      spentAfter.push(e2.id('hex'))
    }
    const trials = 3
    for (const order of [[POL, ENV], [ENV, POL]]) {
      const t = new Tally()
      for (let k = 0; k < trials; k++) {
        const dst = await ctx.node(`gdst-${order[0] === POL ? 'AB' : 'BA'}-${k}`, { topics: order, peer: src.url })
        for (const pass of [1, 2]) {
          const s = await dst.sync()
          t.add(`pass${pass}-sync-${s.body.status}`)
          let liveHeld = 0
          let spentHeld = 0
          for (const txid of live) if (await held(dst.dbName, txid, ENV)) liveHeld++
          for (const txid of spentAfter) if (await held(dst.dbName, txid, ENV)) spentHeld++
          t.add(`pass${pass}-live-envelopes-held`, liveHeld)
          t.add(`pass${pass}-spent-policy-envelopes-held`, spentHeld)
        }
        t.add('source-envelopes-live', live.length)
        t.add('source-envelopes-spent-policy', spentAfter.length)
        // The syncing node's dry-run reads of the live envelopes.
        for (const txid of live.slice(0, 5)) {
          const dry = (await dst.recordsFor(txid)).find((x) => x.dryRun)
          if (dry !== undefined) for (const [p, v] of Object.entries(dry.seen)) t.add(`dry-seen.${p}=${v}`)
          else t.add('dry-seen.no-call')
        }
        await dst.stop()
      }
      out[`order ${order[0] === POL ? 'A,B' : 'B,A'} (x${trials} fresh peers)`] = t.n
    }
    return out
  },
}

// ---------------------------------------------------------------- main

async function main() {
  const { mkdirSync } = await import('node:fs')
  mkdirSync(work, { recursive: true })
  const chain = new Chain()
  const headers = await chain.serve()
  const nodes = []
  const ctx = {
    backend: args.backend,
    chain,
    async node(name, opts = {}) {
      const n = args.backend === 'refhost' ? new RefNode(name, chain, { headers: headers.url, ...opts }) : new EngineNode(name, chain, opts)
      await n.start()
      nodes.push(n)
      return n
    },
  }
  const results = { backend: args.backend, trials: N, run: RUN, started: new Date().toISOString(), engine: '@bsv/overlay 2.3.1', scenarios: {} }
  if (args.backend === 'refhost') results.refhost = JSON.parse(readFileSync(join(REFHOST, 'package.json'), 'utf8')).version
  const names = args.only !== undefined ? args.only.split(',') : Object.keys(scenarios)
  try {
    for (const s of names) {
      const t0 = Date.now()
      results.scenarios[s] = await scenarios[s](ctx)
      console.log(`== ${s} (${((Date.now() - t0) / 1000).toFixed(1)}s)`)
      console.log(JSON.stringify(results.scenarios[s], null, 1))
    }
  } finally {
    for (const n of nodes) await n.stop()
    headers.close()
    for (const n of nodes) await root.raw(`DROP DATABASE IF EXISTS \`${n.dbName}\``).catch(() => {})
    for (const d of dbs.values()) await d.destroy()
    await root.destroy()
  }
  results.finished = new Date().toISOString()
  results.work = work
  if (args.out !== undefined) writeFileSync(args.out, JSON.stringify(results, null, 1) + '\n')
}

await main()
