/**
 * The host's terms route (spec sections 7.3 and 7.4): one small HTTP server
 * beside the overlay host, whose base URL is what BRC-180's
 * `metanet.overlays` names for `ls_bbox`. It serves
 *
 *   GET  <base>/ls_bbox/terms     the host terms document (404 when the host prices nothing)
 *   POST <base>/.well-known/auth  the BRC-104 handshake
 *   POST <base>/lookup            BRC-24 questions to ls_bbox, free and priced
 *
 * A free question is answered with or without BRC-104 authentication. A
 * question of a class the host prices must be asked over BRC-104; without a
 * payment it is answered 402 with BRC-105's headers (the version, the
 * price, the derivation prefix; the payee is the server's identity key in
 * the authentication headers), and with one that pays the price to the key
 * BRC-29 derives for that prefix, suffix and asker, and that verifies
 * against the host's headers, it is answered. One payment buys one
 * question, which is one answer page.
 *
 * The overlay host's own /lookup refuses a class the host prices (ls_bbox
 * does), so this route is the only way to it.
 */
import { createServer, type IncomingHttpHeaders, type IncomingMessage, type Server, type ServerResponse } from 'node:http'
import { closeSync, fdatasyncSync, mkdirSync, openSync, readFileSync, writeSync } from 'node:fs'
import { join } from 'node:path'
import {
  Peer,
  Transaction,
  Utils,
  createNonce,
  normalizeBRC100ByteFields,
  stringifyBRC100,
  verifyNonce,
  type AuthMessage,
  type ChainTracker,
  type PeerSession,
  type Transport,
  type WalletInterface,
} from '@bsv/sdk'
import type { ModuleHost } from '@lightwebinc/bcommon'
import { DefaultBudget, HandshakeBudget, type BudgetConfig } from './budget.js'
import { LookupService as ServiceName } from './boxrec.js'
import type { BboxLookupService } from './ls_bbox.js'
import { PaymentProtocol } from './payment.js'
import { Classes } from './query.js'
import { p2pkh } from './script.js'
import { bytesEq, fromHex, strictBase64, toBase64 } from './util.js'

/** BRC-105's payment version. */
export const PaymentVersion = '1.0'
/** The terms route, below the base URL. */
export const TermsPath = `/${ServiceName}/terms`
/** A question and its authentication are small; a payment rides in a header. */
const MaxBody = 64 << 10
const MaxHeaders = 256 << 10
const AuthTimeout = 30_000

/**
 * Parses a host's prices, `history=5,history-after=5`: a comma list of
 * `<class>=<satoshis>`, each a priceable class of spec section 7.2 once,
 * priced 0 to 2^53 - 1. A free class is refused: it never has a price.
 */
export function parsePrices(raw: string | undefined): Map<string, number> {
  const out = new Map<string, number>()
  if (raw === undefined || raw.trim() === '') return out
  for (const entry of raw.split(',')) {
    const e = entry.trim()
    const eq = e.indexOf('=')
    const name = eq < 0 ? e : e.slice(0, eq)
    const cls = Classes.find((c) => c.name === name)
    if (eq < 0 || cls === undefined) throw new Error(`BBOX_PRICES: "${e}" is not <class>=<satoshis>`)
    if (cls.free) throw new Error(`BBOX_PRICES: ${name} is a free class and never has a price`)
    if (out.has(name)) throw new Error(`BBOX_PRICES names ${name} twice`)
    const v = e.slice(eq + 1)
    if (!/^(0|[1-9][0-9]*)$/.test(v) || !Number.isSafeInteger(Number(v))) throw new Error(`BBOX_PRICES: ${name}: "${v}" is not an integer from 0 to 2^53 - 1`)
    out.set(name, Number(v))
  }
  return out
}

/** The host terms document (spec section 7.4), classes in the order of section 7.2. */
export function termsDocument(prices: ReadonlyMap<string, number>): { service: string; terms: number; classes: Array<{ class: string; satoshis: number }> } {
  return {
    service: ServiceName,
    terms: 1,
    classes: Classes.filter((c) => prices.has(c.name)).map((c) => ({ class: c.name, satoshis: prices.get(c.name)! })),
  }
}

/** A payment the route accepted: what a wallet's internalizeAction needs to take it. */
export interface ReceivedPayment {
  txid: string
  /** The payment as Atomic BEEF. */
  beef: number[]
  outputIndex: 0
  satoshis: number
  derivationPrefix: string
  derivationSuffix: string
  senderIdentityKey: string
  class: string
}

/**
 * Where accepted payments go. claim records p and returns true, atomically,
 * unless its txid was claimed before: one payment buys one question.
 */
export interface PaymentReceiver {
  claim(p: ReceivedPayment): boolean
}

/** Payments in memory, for tests. */
export class MemoryReceiver implements PaymentReceiver {
  readonly payments: ReceivedPayment[] = []
  claim(p: ReceivedPayment): boolean {
    if (this.payments.some((x) => x.txid === p.txid)) return false
    this.payments.push(p)
    return true
  }
}

/**
 * Payments as a ledger file, `payments.jsonl` in dir, one line each, flushed
 * to disk before the question is answered: what the operator's wallet
 * internalizes (BRC-100 internalizeAction, protocol "wallet payment", output
 * 0, with the prefix, the suffix and the sender's identity key), which also
 * broadcasts it. The txids already in the file are claimed.
 */
export class LedgerReceiver implements PaymentReceiver {
  readonly path: string
  private readonly txids = new Set<string>()
  private fd: number | undefined

  constructor(dir: string) {
    mkdirSync(dir, { recursive: true })
    this.path = join(dir, 'payments.jsonl')
    let text = ''
    try {
      text = readFileSync(this.path, 'utf8')
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== 'ENOENT') throw e
    }
    for (const l of text.split('\n')) {
      try {
        const v = JSON.parse(l) as { txid?: unknown }
        if (typeof v.txid === 'string') this.txids.add(v.txid)
      } catch {
        // a line cut short by a crash: its payment was never answered
      }
    }
    if (text.length > 0 && !text.endsWith('\n')) this.write('\n')
  }

  private write(s: string): void {
    this.fd ??= openSync(this.path, 'a')
    writeSync(this.fd, s)
    fdatasyncSync(this.fd)
  }

  claim(p: ReceivedPayment): boolean {
    if (this.txids.has(p.txid)) return false
    this.txids.add(p.txid)
    this.write(JSON.stringify({ ...p, beef: toBase64(Uint8Array.from(p.beef)), at: Math.floor(Date.now() / 1000) }) + '\n')
    return true
  }

  close(): void {
    if (this.fd !== undefined) closeSync(this.fd)
    this.fd = undefined
  }
}

/**
 * A chain tracker over a header source's `/v1` routes (`/v1/root/<height>`,
 * `/v1/tip`), the shape the reference host's own tracker reads. A height the
 * source does not hold is not valid; any other failure is an error.
 */
export class HeaderTracker implements ChainTracker {
  constructor(
    private readonly base: string,
    private readonly timeoutMs = 10_000,
  ) {}

  private async get(path: string): Promise<{ status: number; body: unknown }> {
    const res = await fetch(`${this.base.replace(/\/+$/, '')}${path}`, { headers: { accept: 'application/json' }, signal: AbortSignal.timeout(this.timeoutMs) })
    return { status: res.status, body: res.status === 200 ? await res.json() : undefined }
  }

  async isValidRootForHeight(root: string, height: number): Promise<boolean> {
    const { status, body } = await this.get(`/v1/root/${height}`)
    if (status === 404) return false
    if (status !== 200) throw new Error(`headers: root for height ${height}: status ${status}`)
    return (body as { merkleRoot?: unknown }).merkleRoot === root
  }

  async currentHeight(): Promise<number> {
    const { status, body } = await this.get('/v1/tip')
    const height = (body as { height?: unknown } | undefined)?.height
    if (status !== 200 || typeof height !== 'number') throw new Error(`headers: tip: status ${status}`)
    return height
  }
}

/** The default bound on BRC-104 sessions the terms route keeps, and their idle life. */
export const DefaultMaxSessions = 10_000
export const DefaultSessionTTL = 600

/**
 * The BRC-104 sessions the terms route keeps, bounded. The SDK's own store
 * keeps every session for the life of the process, and a handshake is
 * unauthenticated, so anyone could grow it without end. This one holds at
 * most `max` sessions and forgets one idle for `ttlMs` (idle: not added,
 * updated or looked up); past the cap it forgets the least recently used.
 * A client whose session was forgotten is refused (401) and shakes hands
 * again. It is the SessionManager contract the SDK's Peer uses.
 */
export class BoundedSessions {
  /** By session nonce, least recently used first. */
  private readonly byNonce = new Map<string, { s: PeerSession; at: number }>()
  private readonly byKey = new Map<string, Set<string>>()

  constructor(
    readonly max: number,
    readonly ttlMs: number,
    private readonly now: () => number = Date.now,
    private readonly evicted: (why: 'cap' | 'idle') => void = () => {},
  ) {
    if (!Number.isSafeInteger(max) || max < 1) throw new Error('BoundedSessions: max must be at least 1')
    if (!(ttlMs > 0)) throw new Error('BoundedSessions: ttlMs must be positive')
  }

  get size(): number {
    return this.byNonce.size
  }

  addSession(session: PeerSession): void {
    const nonce = session.sessionNonce
    if (typeof nonce !== 'string') throw new TypeError('Invalid session: sessionNonce is required to add a session.')
    this.forget(nonce)
    this.byNonce.set(nonce, { s: session, at: this.now() })
    if (typeof session.peerIdentityKey === 'string') {
      let set = this.byKey.get(session.peerIdentityKey)
      if (set === undefined) this.byKey.set(session.peerIdentityKey, (set = new Set()))
      set.add(nonce)
    }
    this.prune()
  }

  updateSession(session: PeerSession): void {
    this.addSession(session)
  }

  getSession(identifier: string): PeerSession | undefined {
    this.expire()
    const direct = this.byNonce.get(identifier)
    if (direct !== undefined) return this.touch(identifier, direct.s)
    let best: PeerSession | undefined
    for (const nonce of this.byKey.get(identifier) ?? []) {
      const e = this.byNonce.get(nonce)
      if (e !== undefined && (best === undefined || (e.s.lastUpdate ?? 0) > (best.lastUpdate ?? 0))) best = e.s
    }
    return best === undefined ? undefined : this.touch(best.sessionNonce!, best)
  }

  removeSession(session: PeerSession): void {
    if (typeof session.sessionNonce === 'string') this.forget(session.sessionNonce)
  }

  hasSession(identifier: string): boolean {
    return this.getSession(identifier) !== undefined
  }

  private touch(nonce: string, s: PeerSession): PeerSession {
    this.byNonce.delete(nonce)
    this.byNonce.set(nonce, { s, at: this.now() })
    return s
  }

  private forget(nonce: string): void {
    const e = this.byNonce.get(nonce)
    if (e === undefined) return
    this.byNonce.delete(nonce)
    const key = e.s.peerIdentityKey
    if (typeof key === 'string') {
      const set = this.byKey.get(key)
      set?.delete(nonce)
      if (set?.size === 0) this.byKey.delete(key)
    }
  }

  /** Forgets every session idle for ttlMs: the oldest come first. */
  private expire(): void {
    const cutoff = this.now() - this.ttlMs
    for (const [nonce, e] of this.byNonce) {
      if (e.at > cutoff) break
      this.forget(nonce)
      this.evicted('idle')
    }
  }

  private prune(): void {
    this.expire()
    for (const nonce of this.byNonce.keys()) {
      if (this.byNonce.size <= this.max) break
      this.forget(nonce)
      this.evicted('cap')
    }
  }
}

/** A reply before it is written, and signed when the request was authenticated. */
interface Reply {
  status: number
  headers: Record<string, string>
  body: Uint8Array
}

const text = new TextEncoder()
const json = (status: number, v: unknown, headers: Record<string, string> = {}): Reply => ({
  status,
  headers: { 'content-type': 'application/json', ...headers },
  body: text.encode(JSON.stringify(v)),
})
const failure = (status: number, code: string, description: string): Reply => json(status, { status: 'error', code, description })

/** BRC-104 over HTTP, server side: hands requests to a Peer and catches what it sends back. */
class ServerTransport implements Transport {
  private callback: ((m: AuthMessage) => Promise<void>) | undefined
  private readonly waiting = new Map<string, (m: AuthMessage) => void>()

  async send(m: AuthMessage): Promise<void> {
    const key = m.messageType === 'general' ? `g:${Utils.toBase64(m.payload!.slice(0, 32))}` : `h:${m.yourNonce}`
    const w = this.waiting.get(key)
    if (w === undefined) throw new Error('no open request for this message')
    this.waiting.delete(key)
    w(m)
  }

  async onData(callback: (m: AuthMessage) => Promise<void>): Promise<void> {
    this.callback = callback
  }

  /** What the peer will send for key, once it sends it. */
  expect(key: string): Promise<AuthMessage> {
    return new Promise((resolve, reject) => {
      const t = setTimeout(() => {
        this.waiting.delete(key)
        reject(new Error('authentication timed out'))
      }, AuthTimeout)
      t.unref()
      this.waiting.set(key, (m) => {
        clearTimeout(t)
        resolve(m)
      })
    })
  }

  forget(key: string): void {
    this.waiting.delete(key)
  }

  async deliver(m: AuthMessage): Promise<void> {
    if (this.callback === undefined) throw new Error('no peer')
    await this.callback(m)
  }
}

function header(h: IncomingHttpHeaders, name: string): string | undefined {
  const v = h[name]
  return typeof v === 'string' ? v : undefined
}

function optionalText(w: InstanceType<typeof Utils.Writer>, s: string): void {
  if (s.length === 0) {
    w.writeVarIntNum(-1)
    return
  }
  const b = Utils.toArray(s, 'utf8')
  w.writeVarIntNum(b.length)
  w.write(b)
}

function writePairs(w: InstanceType<typeof Utils.Writer>, pairs: Array<[string, string]>): void {
  pairs.sort(([a], [b]) => a.localeCompare(b))
  w.writeVarIntNum(pairs.length)
  for (const [k, v] of pairs) {
    const kb = Utils.toArray(k, 'utf8')
    const vb = Utils.toArray(v, 'utf8')
    w.writeVarIntNum(kb.length)
    w.write(kb)
    w.writeVarIntNum(vb.length)
    w.write(vb)
  }
}

const signedHeader = (k: string): boolean => (k.startsWith('x-bsv-') && !k.startsWith('x-bsv-auth')) || k === 'authorization'

/** The bytes a BRC-104 client signed for its request, rebuilt from what arrived. */
function requestPayload(requestId: number[], method: string, url: URL, headers: IncomingHttpHeaders, body: Uint8Array): number[] {
  const w = new Utils.Writer()
  w.write(requestId)
  w.writeVarIntNum(method.length)
  w.write(Utils.toArray(method))
  optionalText(w, url.pathname)
  optionalText(w, url.search)
  const pairs: Array<[string, string]> = []
  for (const [k, v] of Object.entries(headers)) {
    if (typeof v !== 'string') continue
    if (signedHeader(k)) pairs.push([k, v])
    else if (k === 'content-type') pairs.push([k, v.split(';')[0]!.trim()])
  }
  writePairs(w, pairs)
  if (body.length === 0) {
    w.writeVarIntNum(-1)
  } else {
    w.writeVarIntNum(body.length)
    w.write(Array.from(body))
  }
  return w.toArray()
}

/** The bytes the server signs for its response, as the client rebuilds them. */
function responsePayload(requestId: number[], r: Reply): number[] {
  const w = new Utils.Writer()
  w.write(requestId)
  w.writeVarIntNum(r.status)
  writePairs(
    w,
    Object.entries(r.headers)
      .map(([k, v]): [string, string] => [k.toLowerCase(), v])
      .filter(([k]) => signedHeader(k)),
  )
  w.writeVarIntNum(r.body.length)
  if (r.body.length > 0) w.write(Array.from(r.body))
  return w.toArray()
}

async function readBody(req: IncomingMessage): Promise<Uint8Array> {
  const chunks: Buffer[] = []
  let n = 0
  for await (const c of req) {
    n += (c as Buffer).length
    if (n > MaxBody) throw new Error('the request body is too large')
    chunks.push(c as Buffer)
  }
  return new Uint8Array(Buffer.concat(chunks))
}

type PayeeWallet = Pick<WalletInterface, 'getPublicKey' | 'createSignature' | 'verifySignature' | 'createHmac' | 'verifyHmac'>

export interface FrontOptions {
  ls: BboxLookupService
  host: ModuleHost
  /** Prices by priceable class; empty when the host prices nothing. */
  prices: ReadonlyMap<string, number>
  /** The payee's wallet, whose identity key is the server's: needed for BRC-104 and priced classes. */
  wallet?: PayeeWallet
  /** Where accepted payments go. */
  receiver?: PaymentReceiver
  /** The host's block headers, which a payment verifies against. */
  headers?: ChainTracker
  /** The bound on BRC-104 sessions: the most kept, and how long one idle is kept. */
  sessions?: { max: number; ttlSeconds: number }
  /** The handshake budget (budget.ts); DefaultBudget when unset. */
  budget?: BudgetConfig
}

/** bbox_handshakes_total's results. */
export const HandshakeResults = ['accepted', 'failed', 'limited_address', 'limited_global'] as const

export class LookupFront {
  private readonly peer: Peer | undefined
  private readonly transport = new ServerTransport()
  /** The BRC-104 sessions the route keeps. */
  readonly sessions: BoundedSessions
  /** What handshakes the route answers. */
  readonly budget: HandshakeBudget

  constructor(private readonly o: FrontOptions) {
    for (const [name, price] of o.prices) {
      if (price > 0 && (o.wallet === undefined || o.receiver === undefined || o.headers === undefined)) {
        throw new Error(`bbox: ${name} has a price, which needs a payee wallet, a payment receiver and a header source`)
      }
    }
    const bound = o.sessions ?? { max: DefaultMaxSessions, ttlSeconds: DefaultSessionTTL }
    this.sessions = new BoundedSessions(bound.max, bound.ttlSeconds * 1000, Date.now, (why) => o.host.metrics.inc('bbox_sessions_evicted_total', { why }))
    o.host.metrics.gauge('bbox_sessions', () => this.sessions.size)
    this.budget = new HandshakeBudget(o.budget ?? DefaultBudget)
    for (const result of HandshakeResults) o.host.metrics.preset('bbox_handshakes_total', { result })
    if (o.wallet !== undefined) this.peer = new Peer(o.wallet as WalletInterface, this.transport, undefined, this.sessions as never, false)
  }

  /** The node:http request handler. */
  readonly handler = (req: IncomingMessage, res: ServerResponse): void => {
    this.handle(req, res).catch((e: unknown) => {
      this.o.host.log('bbox terms route failed', { err: String(e) })
      if (!res.headersSent) send(res, failure(500, 'ERR_INTERNAL', 'the request could not be served'))
      else res.destroy()
    })
  }

  /** Listens on host:port; resolves with the server once it listens. */
  async listen(port: number, hostname: string): Promise<Server> {
    const server = createServer({ maxHeaderSize: MaxHeaders }, this.handler)
    await new Promise<void>((resolve, reject) => {
      server.once('error', reject)
      server.listen(port, hostname, () => {
        server.off('error', reject)
        resolve()
      })
    })
    return server
  }

  private async handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const handshake = req.method === 'POST' && new URL(req.url ?? '/', 'http://front').pathname === '/.well-known/auth'
    if (handshake && this.peer !== undefined) {
      // Before the body is read and before any signature work.
      const v = this.budget.take(req.socket.remoteAddress ?? '')
      if (!v.ok) {
        this.o.host.metrics.inc('bbox_handshakes_total', { result: `limited_${v.limit}` })
        send(res, json(429, { status: 'error', code: 'ERR_RATE_LIMITED', description: 'too many handshakes; try again later' }, { 'retry-after': String(v.retryAfter) }))
        return
      }
    }
    let body: Uint8Array
    try {
      body = await readBody(req)
    } catch {
      send(res, failure(413, 'ERR_TOO_LARGE', 'the request body is too large'))
      return
    }
    const url = new URL(req.url ?? '/', 'http://front')
    const method = req.method ?? 'GET'
    if (handshake) {
      const status = await this.handshake(res, body)
      if (this.peer !== undefined) this.o.host.metrics.inc('bbox_handshakes_total', { result: status === 200 ? 'accepted' : 'failed' })
      return
    }
    const requestId = header(req.headers, 'x-bsv-auth-request-id')
    if (requestId === undefined) {
      if (Object.keys(req.headers).some((k) => k.startsWith('x-bsv-auth-'))) {
        send(res, failure(400, 'ERR_AUTH_PARTIAL', 'partial BRC-104 authentication headers'))
        return
      }
      send(res, await this.route(method, url, req.headers, body, undefined))
      return
    }
    await this.authenticated(req, res, method, url, body, requestId)
  }

  /** Answers a handshake; resolves with the status answered. */
  private async handshake(res: ServerResponse, body: Uint8Array): Promise<number> {
    if (this.peer === undefined) {
      send(res, failure(404, 'ERR_NOT_FOUND', 'this host offers no BRC-104 authentication'))
      return 404
    }
    let m: AuthMessage
    try {
      m = normalizeBRC100ByteFields(JSON.parse(new TextDecoder().decode(body)), ['payload', 'signature']) as AuthMessage
    } catch {
      send(res, failure(400, 'ERR_AUTH_MALFORMED', 'the handshake is not JSON'))
      return 400
    }
    if (m.messageType !== 'initialRequest' || typeof m.initialNonce !== 'string') {
      send(res, failure(400, 'ERR_AUTH_MALFORMED', 'only an initial request is accepted here'))
      return 400
    }
    const key = `h:${m.initialNonce}`
    const reply = this.transport.expect(key)
    try {
      await this.transport.deliver(m)
    } catch (e) {
      this.transport.forget(key)
      reply.catch(() => {})
      send(res, failure(401, 'ERR_AUTH_FAILED', String((e as Error).message)))
      return 401
    }
    const r = await reply
    const headers: Record<string, string> = {
      'content-type': 'application/json',
      'x-bsv-auth-version': r.version,
      'x-bsv-auth-message-type': r.messageType,
      'x-bsv-auth-identity-key': r.identityKey,
    }
    if (r.yourNonce !== undefined) headers['x-bsv-auth-your-nonce'] = r.yourNonce
    if (r.signature !== undefined) headers['x-bsv-auth-signature'] = Utils.toHex(r.signature)
    send(res, { status: 200, headers, body: text.encode(stringifyBRC100(r)) })
    return 200
  }

  private async authenticated(req: IncomingMessage, res: ServerResponse, method: string, url: URL, body: Uint8Array, requestId: string): Promise<void> {
    const h = (n: string): string | undefined => header(req.headers, n)
    const version = h('x-bsv-auth-version')
    const identityKey = h('x-bsv-auth-identity-key')
    const nonce = h('x-bsv-auth-nonce')
    const yourNonce = h('x-bsv-auth-your-nonce')
    const signature = h('x-bsv-auth-signature')
    const id = strictBase64(requestId)
    if (this.peer === undefined || version === undefined || identityKey === undefined || nonce === undefined || yourNonce === undefined || signature === undefined || id?.length !== 32 || !/^([0-9a-f]{2})+$/.test(signature)) {
      send(res, failure(401, 'ERR_AUTH_FAILED', 'the BRC-104 authentication headers are incomplete'))
      return
    }
    const idBytes = Array.from(id)
    try {
      await this.transport.deliver({
        version,
        messageType: 'general',
        identityKey,
        nonce,
        yourNonce,
        payload: requestPayload(idBytes, method, url, req.headers, body),
        signature: Array.from(fromHex(signature)),
      })
    } catch {
      send(res, failure(401, 'ERR_AUTH_FAILED', 'the request does not verify'))
      return
    }
    const reply = await this.route(method, url, req.headers, body, identityKey)
    const key = `g:${requestId}`
    const signed = this.transport.expect(key)
    try {
      await this.peer.toPeer(responsePayload(idBytes, reply), yourNonce)
    } catch (e) {
      this.transport.forget(key)
      signed.catch(() => {})
      throw e
    }
    const m = await signed
    send(res, {
      ...reply,
      headers: {
        ...reply.headers,
        'x-bsv-auth-version': m.version,
        'x-bsv-auth-identity-key': m.identityKey,
        'x-bsv-auth-nonce': m.nonce!,
        'x-bsv-auth-your-nonce': m.yourNonce!,
        'x-bsv-auth-signature': Utils.toHex(m.signature!),
        'x-bsv-auth-request-id': requestId,
      },
    })
  }

  /** The application: terms and lookups. identity is the asker's key when authenticated. */
  private async route(method: string, url: URL, headers: IncomingHttpHeaders, body: Uint8Array, identity: string | undefined): Promise<Reply> {
    const { ls, prices } = this.o
    if (url.pathname === TermsPath && method === 'GET') {
      if (prices.size === 0) return failure(404, 'ERR_NO_TERMS', 'this host prices nothing')
      return json(200, termsDocument(prices))
    }
    if (url.pathname !== '/lookup' || method !== 'POST') return failure(404, 'ERR_NOT_FOUND', 'no such route')
    let question: { service: string; query: unknown }
    try {
      question = JSON.parse(new TextDecoder().decode(body)) as { service: string; query: unknown }
      if (typeof question !== 'object' || question === null) throw new Error('not an object')
    } catch {
      return json(400, { error: 'the question is not a JSON object' })
    }
    let cls: string
    try {
      cls = ls.classify(question).q.class.name
    } catch (e) {
      return json(400, { error: (e as Error).message })
    }
    if (!ls.restored) return failure(503, 'ERR_NOT_READY', 'the lookup service is not restored yet')
    const price = prices.get(cls)
    const extra: Record<string, string> = {}
    if (price !== undefined && price > 0) {
      if (identity === undefined) return failure(401, 'ERR_AUTH_REQUIRED', `${cls} is priced; ask it over BRC-104 authentication`)
      const raw = header(headers, 'x-bsv-payment')
      if (raw === undefined) {
        this.o.host.metrics.inc('bbox_payments_total', { result: 'requested' })
        const prefix = await createNonce(this.o.wallet as WalletInterface)
        return json(
          402,
          { status: 'error', code: 'ERR_PAYMENT_REQUIRED', satoshisRequired: price, description: `${cls} costs ${price} satoshis a question` },
          { 'x-bsv-payment-version': PaymentVersion, 'x-bsv-payment-satoshis-required': String(price), 'x-bsv-payment-derivation-prefix': prefix },
        )
      }
      const paid = await this.pay(raw, identity, price, cls)
      this.o.host.metrics.inc('bbox_payments_total', { result: paid.ok ? 'accepted' : paid.reply.status === 409 ? 'replayed' : 'refused' })
      if (!paid.ok) return paid.reply
      extra['x-bsv-payment-satoshis-paid'] = String(paid.satoshis)
    }
    try {
      return json(200, await ls.hydrate(await ls.answer(question, true)), extra)
    } catch (e) {
      return json(400, { error: (e as Error).message })
    }
  }

  /**
   * Checks a BRC-105 payment of price for one question of cls from identity:
   * a prefix this server issued, an Atomic BEEF whose output 0 holds at least
   * the price and pays the key BRC-29 derives for the prefix, the suffix and
   * the asker, a transaction that verifies against the host's headers (full
   * SPV, no fee check), and a txid never claimed before.
   */
  private async pay(raw: string, identity: string, price: number, cls: string): Promise<{ ok: true; satoshis: number } | { ok: false; reply: Reply }> {
    const bad = (status: number, code: string, d: string) => ({ ok: false as const, reply: failure(status, code, d) })
    const wallet = this.o.wallet!
    let p: { derivationPrefix?: unknown; derivationSuffix?: unknown; transaction?: unknown }
    try {
      p = JSON.parse(raw) as typeof p
    } catch {
      return bad(400, 'ERR_MALFORMED_PAYMENT', 'the payment header is not JSON')
    }
    const { derivationPrefix: prefix, derivationSuffix: suffix, transaction } = p ?? {}
    if (typeof prefix !== 'string' || typeof suffix !== 'string' || typeof transaction !== 'string' || strictBase64(prefix) === undefined || strictBase64(suffix) === undefined) {
      return bad(400, 'ERR_MALFORMED_PAYMENT', 'the payment header is malformed')
    }
    const beef = strictBase64(transaction)
    if (beef === undefined) return bad(400, 'ERR_MALFORMED_PAYMENT', 'the payment transaction is not standard base64')
    let issued = false
    try {
      issued = await verifyNonce(prefix, wallet as WalletInterface)
    } catch {
      issued = false
    }
    if (!issued) return bad(400, 'ERR_INVALID_DERIVATION_PREFIX', 'this server did not issue that derivation prefix')
    let tx: Transaction
    try {
      tx = Transaction.fromAtomicBEEF(Array.from(beef))
    } catch {
      return bad(400, 'ERR_INVALID_PAYMENT', 'the payment is not Atomic BEEF')
    }
    const out = tx.outputs[0]
    if (out === undefined || (out.satoshis ?? 0) < price) return bad(400, 'ERR_INVALID_PAYMENT', `output 0 does not hold ${price} satoshis`)
    const { publicKey } = await wallet.getPublicKey({ protocolID: PaymentProtocol, keyID: `${prefix} ${suffix}`, counterparty: identity, forSelf: true })
    if (!bytesEq(Uint8Array.from(out.lockingScript.toBinary()), p2pkh(fromHex(publicKey)))) {
      return bad(400, 'ERR_INVALID_PAYMENT', "output 0 does not pay the key derived for this prefix, suffix and asker")
    }
    let verified = false
    try {
      verified = await tx.verify(this.o.headers!)
    } catch {
      verified = false
    }
    if (!verified) return bad(400, 'ERR_PAYMENT_SPV', "the payment does not verify against this host's headers")
    const accepted: ReceivedPayment = {
      txid: tx.id('hex'),
      beef: Array.from(beef),
      outputIndex: 0,
      satoshis: out.satoshis ?? 0,
      derivationPrefix: prefix,
      derivationSuffix: suffix,
      senderIdentityKey: identity,
      class: cls,
    }
    if (!this.o.receiver!.claim(accepted)) return bad(409, 'ERR_PAYMENT_REPLAYED', 'this payment was already used')
    this.o.host.log('bbox payment accepted', { txid: accepted.txid, satoshis: accepted.satoshis, class: cls })
    return { ok: true, satoshis: accepted.satoshis }
  }
}

function send(res: ServerResponse, r: Reply): void {
  res.writeHead(r.status, { ...r.headers, 'content-length': String(r.body.length) })
  res.end(Buffer.from(r.body))
}
