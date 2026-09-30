/**
 * The bbox host module: `tm_bbox_<name>_<suffix>` for each configured
 * office and `ls_bbox` for all of them, loaded into a reference overlay host
 * by path (`OVERLAY_MODULES=/abs/path/to/bbox-module.js`). The host carries
 * no bbox code; this file's default export is the factory it calls.
 *
 * Configuration is read from the environment when the host calls the
 * factory (docs/host.md):
 *
 *   BBOX_OFFICES          required: the office identifiers, a comma list
 *   BBOX_STATE_DIR        required: an absolute directory kept for the host's
 *                         life, for the outpoint rows and the payment ledger
 *   BBOX_RETENTION_DAYS   how long what is no longer answered is kept, from
 *                         first sight; at least and by default 31
 *   BBOX_MAX_BEEF         the host's BEEF bound; at least and by default 262144
 *   BBOX_LISTEN           host:port of the terms route (paid.ts)
 *   BBOX_PRICES           the classes priced, e.g. history=5,history-after=5
 *   BBOX_PAYEE_KEY        the payee's private key, 64 hex characters
 *   BBOX_HEADERS_URL      the header source payments verify against;
 *                         OVERLAY_CHAIN_TRACKER_URL when unset
 *   BBOX_SESSIONS         the most BRC-104 sessions the terms route keeps;
 *                         default 10000
 *   BBOX_SESSION_TTL      seconds an idle BRC-104 session is kept; default 600
 *
 * Every `tm_bbox_` topic in OVERLAY_TOPICS must be an office named here,
 * because a bbox topic left to the host's default manager would admit
 * anything. Every mistake throws, and the host refuses to start.
 */
import { isAbsolute } from 'node:path'
import type { Server } from 'node:http'
import { PrivateKey, ProtoWallet } from '@bsv/sdk'
import type { Module, ModuleHost } from '@lightwebinc/bcommon'
import { DefaultMaxBEEF } from './beef.js'
import { LookupService, TopicPrefix, checkOffice, topic } from './boxrec.js'
import { FileJournal, type Journal } from './journal.js'
import { BboxLookupService, FloorDays } from './ls_bbox.js'
import { DefaultMaxSessions, DefaultSessionTTL, HeaderTracker, LedgerReceiver, LookupFront, parsePrices } from './paid.js'
import { AdmitKinds, BboxTopicManager, Reasons } from './tm_bbox.js'

/**
 * The bundle's banner, which the bundle step defines (scripts/bundle.js):
 * the package and library versions the host is running. The tsc tree
 * leaves it undefined.
 */
declare const BBOX_BUILD: string | undefined

/** How often retention runs, seconds. */
const PruneEvery = 3600

export interface Config {
  offices: string[]
  stateDir: string
  retentionDays: number
  maxBEEF: number
  listen?: { host: string; port: number }
  prices: Map<string, number>
  payeeKey?: PrivateKey
  headersURL?: string
  sessions: { max: number; ttlSeconds: number }
}

/** Parses BBOX_OFFICES. Throws on anything it cannot take exactly. */
export function parseOffices(raw: string | undefined): string[] {
  if (raw === undefined || raw.trim() === '') throw new Error('BBOX_OFFICES is required: name each office this host carries')
  const out: string[] = []
  for (const entry of raw.split(',')) {
    const office = entry.trim()
    try {
      checkOffice(office)
    } catch {
      throw new Error(`BBOX_OFFICES: "${office}" is not an office identifier <name>_<suffix>`)
    }
    if (out.includes(office)) throw new Error(`BBOX_OFFICES names ${office} twice`)
    out.push(office)
  }
  return out
}

function integer(name: string, raw: string | undefined, def: number, min: number): number {
  if (raw === undefined || raw.trim() === '') return def
  const v = raw.trim()
  if (!/^[0-9]+$/.test(v) || !Number.isSafeInteger(Number(v)) || Number(v) < min) throw new Error(`${name}: "${raw}" is not an integer of at least ${min}`)
  return Number(v)
}

/** Reads the whole configuration from env. */
export function parseConfig(env: NodeJS.ProcessEnv): Config {
  const offices = parseOffices(env['BBOX_OFFICES'])
  const stateDir = env['BBOX_STATE_DIR']
  if (stateDir === undefined || !isAbsolute(stateDir)) {
    throw new Error('BBOX_STATE_DIR is required: an absolute directory kept for the host\'s life, where the outpoint rows live')
  }
  const c: Config = {
    offices,
    stateDir,
    retentionDays: integer('BBOX_RETENTION_DAYS', env['BBOX_RETENTION_DAYS'], FloorDays, FloorDays),
    maxBEEF: integer('BBOX_MAX_BEEF', env['BBOX_MAX_BEEF'], DefaultMaxBEEF, DefaultMaxBEEF),
    prices: parsePrices(env['BBOX_PRICES']),
    sessions: {
      max: integer('BBOX_SESSIONS', env['BBOX_SESSIONS'], DefaultMaxSessions, 1),
      ttlSeconds: integer('BBOX_SESSION_TTL', env['BBOX_SESSION_TTL'], DefaultSessionTTL, 1),
    },
  }
  const listen = env['BBOX_LISTEN']?.trim()
  if (listen !== undefined && listen !== '') {
    const m = /^(.+):([0-9]{1,5})$/.exec(listen)
    const port = m === null ? NaN : Number(m[2])
    if (m === null || port < 1 || port > 65535) throw new Error(`BBOX_LISTEN: "${listen}" is not host:port`)
    c.listen = { host: m[1]!.replace(/^\[(.*)\]$/, '$1'), port }
  }
  const key = env['BBOX_PAYEE_KEY']?.trim()
  if (key !== undefined && key !== '') {
    if (!/^[0-9a-f]{64}$/.test(key)) throw new Error('BBOX_PAYEE_KEY is not 64 lowercase hex characters')
    c.payeeKey = PrivateKey.fromHex(key)
  }
  const headers = (env['BBOX_HEADERS_URL'] ?? env['OVERLAY_CHAIN_TRACKER_URL'])?.trim()
  if (headers !== undefined && headers !== '') c.headersURL = headers
  const charged = [...c.prices.values()].some((p) => p > 0)
  if (charged && (c.listen === undefined || c.payeeKey === undefined || c.headersURL === undefined)) {
    throw new Error('BBOX_PRICES prices a class: BBOX_LISTEN, BBOX_PAYEE_KEY and a header source (BBOX_HEADERS_URL or OVERLAY_CHAIN_TRACKER_URL) are required')
  }
  if (c.prices.size > 0 && c.listen === undefined) throw new Error('BBOX_PRICES needs BBOX_LISTEN, where the terms document is served')
  return c
}

export interface Mounted {
  module: Module
  ls: BboxLookupService
  /** The terms route, when configured. */
  front?: LookupFront
  /** Starts the terms route (when configured) and retention; resolves when the route listens. */
  start: () => Promise<Server | undefined>
}

/**
 * The module for this configuration. `overlayTopics` is OVERLAY_TOPICS,
 * when known; `journal` replaces the state directory's, for tests.
 */
export function bboxModule(host: ModuleHost, c: Config, overlayTopics?: readonly string[], journal?: Journal): Mounted {
  const names = new Set(c.offices.map(topic))
  for (const t of overlayTopics ?? []) {
    if (t.startsWith(TopicPrefix) && !names.has(t)) {
      throw new Error(`OVERLAY_TOPICS names ${t} but BBOX_OFFICES does not configure it; it would be admitted by the default manager`)
    }
  }
  for (const kind of AdmitKinds) host.metrics.preset('bbox_admitted_total', { kind })
  for (const reason of Reasons) host.metrics.preset('bbox_refused_total', { reason })
  for (const why of ['evidence', 'retention']) host.metrics.preset('bbox_dropped_total', { why })
  host.metrics.preset('bbox_retractions_total')
  const priced = new Set([...c.prices].filter(([, p]) => p > 0).map(([name]) => name))
  const ls = new BboxLookupService({
    offices: new Set(c.offices),
    host,
    journal: journal ?? new FileJournal(c.stateDir),
    priced,
    retentionDays: c.retentionDays,
  })
  host.metrics.gauge('bbox_offices', () => c.offices.length)
  host.metrics.gauge('bbox_envelopes', () => ls.envelopeCount)
  host.metrics.gauge('bbox_receipts', () => ls.receiptCount)
  host.metrics.gauge('bbox_sweeps', () => ls.sweepCount)
  host.metrics.gauge('bbox_outpoint_rows', () => ls.rowCount)
  const topics: Record<string, BboxTopicManager> = {}
  for (const office of c.offices) topics[topic(office)] = new BboxTopicManager(topic(office), host, undefined, c.maxBEEF)
  let front: LookupFront | undefined
  if (c.listen !== undefined) {
    front = new LookupFront({
      ls,
      host,
      prices: c.prices,
      wallet: c.payeeKey === undefined ? undefined : new ProtoWallet(c.payeeKey),
      receiver: priced.size > 0 ? new LedgerReceiver(c.stateDir) : undefined,
      headers: c.headersURL === undefined ? undefined : new HeaderTracker(c.headersURL),
      sessions: c.sessions,
    })
  }
  const start = async (): Promise<Server | undefined> => {
    const timer = setInterval(() => {
      if (!ls.restored) return
      ls.prune().catch((e: unknown) => host.log('ls_bbox retention failed', { err: String(e) }))
    }, PruneEvery * 1000)
    timer.unref()
    if (front === undefined || c.listen === undefined) return undefined
    const server = await front.listen(c.listen.port, c.listen.host)
    server.unref()
    return server
  }
  const build = typeof BBOX_BUILD === 'string' ? BBOX_BUILD : 'unbundled'
  host.log('bbox module mounted', {
    topics: [...names].join(','),
    lookup: LookupService,
    terms: c.listen === undefined ? 'none' : `${c.listen.host}:${c.listen.port}`,
    priced: [...priced].join(',') || 'none',
    sessions: c.listen === undefined ? 'none' : `${c.sessions.max}, idle ${c.sessions.ttlSeconds}s`,
    build,
  })
  return { module: { topics, lookups: { [LookupService]: ls } }, ls, front, start }
}

/** The factory the host calls: the configuration from the environment, checked against OVERLAY_TOPICS. */
export default async function create(host: ModuleHost): Promise<Module> {
  const overlay = process.env['OVERLAY_TOPICS']
  const topics = overlay === undefined ? undefined : overlay.split(',').map((t) => t.trim()).filter((t) => t !== '')
  const m = bboxModule(host, parseConfig(process.env), topics)
  await m.start()
  return m.module
}

export { BboxTopicManager, Refused } from './tm_bbox.js'
export { BboxLookupService } from './ls_bbox.js'
export { LookupFront, termsDocument } from './paid.js'
