/**
 * `ls_bbox`: one lookup service for every office a host carries (spec
 * sections 7.2, 7.3 and 8.2 to 8.5).
 *
 * Two stores back it. The engine's storage holds the outputs the host
 * keeps: envelope and receipt carriers and sweep tombstones, which this
 * service indexes in memory by office and key and rebuilds from storage on
 * start. The journal (journal.ts) holds the outpoint rows the host keeps for
 * its life: every carrier any office admitted, with the outpoint it spent;
 * every drop; every outpoint an admitted sweep spent. Every status (spec
 * section 8.2) is a function of the rows, never of arrival order:
 *
 * - `retracted`: a sweep spends the carrier's funding outpoint;
 * - `superseded`: the carrier is not its outpoint's winner, or the winner
 *   was dropped;
 * - `acknowledged`: an envelope a receipt acknowledges, the receipt being
 *   by the envelope's `to`, not retracted, and its own outpoint's winner;
 * - `held`: otherwise.
 *
 * The winner of an outpoint is the lowest commitment among its envelopes a
 * receipt acknowledges, or else among all its carriers. A dropped receipt
 * keeps acknowledging (its row outlives it), so a drop can never hand an
 * outpoint a new winner.
 */
import { Transaction } from '@bsv/sdk'
import type {
  LookupFormula,
  LookupQuestion,
  LookupService,
  LookupServiceMetaData,
  ModuleHost,
  Output,
  OutputAdmittedByTopic,
  RestoreStorage,
} from '@lightwebinc/bcommon'
import { LookupService as ServiceName, Refusal, TopicPrefix, claimOf, decodeEnvelope, decodeReceipt, firstPush, topic } from './boxrec.js'
import type { CarrierLine, Journal, Line } from './journal.js'
import { parseQuery, type Question } from './query.js'
import { isFundingShape } from './script.js'
import { fromHex, toHex } from './util.js'

/** The answer window, spec section 8.3: 30 days before the host's time, one hour after. */
export const WindowBefore = 30 * 86400
export const WindowAfter = 3600
/** The retention floor for receipts and sweeps, from first sight (spec section 8.4). */
export const FloorDays = 31
/** Superseded carriers kept per outpoint as evidence, and when the winner is acknowledged. */
export const Evidence = 8
export const EvidenceAcknowledged = 64

export type Status = 'retracted' | 'superseded' | 'acknowledged' | 'held'

/** An envelope carrier the host holds. */
interface EnvEntry {
  txid: string
  c: string
  office: string
  to: string
  box: string
  from: string
  created: number
  expires: number
}

/** A receipt carrier the host holds. */
interface RcptEntry {
  txid: string
  c: string
  office: string
  by: string
  acks: Set<string>
  created: number
}

/** A sweep tombstone the host holds. */
interface SweepEntry {
  txid: string
  office: string
  spent: string[]
  height: number
}

/** One funding outpoint's row. */
interface Row {
  carriers: Map<string, CarrierLine>
  dropped: Set<string>
  /** Its winner was dropped: nothing on it is answered again. */
  closed: boolean
  /** Admitted sweeps that spend it. */
  sweeps: Set<string>
}

/** What restore and retention need from the engine's storage. */
export interface Storage extends RestoreStorage {
  findOutput: (txid: string, outputIndex: number, topic?: string, spent?: boolean, includeBEEF?: boolean) => Promise<Output | null>
  deleteOutput?: (txid: string, outputIndex: number, topic: string) => Promise<void>
}

/** The engine's lookup answer. */
export interface OutputList {
  type: 'output-list'
  outputs: Array<{ beef: number[]; outputIndex: number }>
}

export interface Options {
  /** The office identifiers the host carries. */
  offices: ReadonlySet<string>
  host: ModuleHost
  journal: Journal
  /** Priceable classes this host prices: asked through the engine, they are refused. */
  priced?: ReadonlySet<string>
  /** How long a host keeps what it no longer answers, in days from first sight; at least 31. */
  retentionDays?: number
  /** The host's clock, Unix seconds. */
  now?: () => number
}

const outpoint = (txid: string, vout: number): string => `${txid}.${vout}`
const hashOrder = (display: string): string => toHex(fromHex(display).reverse())
const cmp = (a: string, b: string): number => (a < b ? -1 : a > b ? 1 : 0)

function addTo(m: Map<string, Set<string>>, k: string, v: string): void {
  let s = m.get(k)
  if (s === undefined) m.set(k, (s = new Set()))
  s.add(v)
}

function removeFrom(m: Map<string, Set<string>>, k: string, v: string): void {
  const s = m.get(k)
  s?.delete(v)
  if (s?.size === 0) m.delete(k)
}

export class BboxLookupService implements LookupService {
  /** The whole transaction: a carrier's funding outpoint and a sweep's spends are its inputs. */
  readonly admissionMode = 'whole-tx' as const
  /** Spends are read from admitted transactions, never from the engine's notices. */
  readonly spendNotificationMode = 'none' as const

  readonly offices: ReadonlySet<string>
  private readonly host: ModuleHost
  private readonly journal: Journal
  private readonly priced: ReadonlySet<string>
  private readonly retention: number
  private readonly now: () => number

  // Outpoint rows, from the journal.
  private readonly rows = new Map<string, Row>()
  /** Commitment -> its carrier line. */
  private readonly lines = new Map<string, CarrierLine>()
  /** Envelope commitment -> receipt commitments listing it. */
  private readonly ackers = new Map<string, Set<string>>()
  /** Sweep txid -> first sight. */
  private readonly sweepSeen = new Map<string, number>()

  // What the host holds, from storage.
  private readonly envelopes = new Map<string, EnvEntry>()
  private readonly envByTo = new Map<string, Set<string>>()
  private readonly receipts = new Map<string, RcptEntry>()
  private readonly rcptByBy = new Map<string, Set<string>>()
  private readonly sweeps = new Map<string, SweepEntry>()
  private readonly sweepsByOp = new Map<string, Set<string>>()

  /** Outputs dropped from the index and still to be deleted from storage. */
  private readonly pending = new Map<string, { txid: string; topic: string }>()
  private storage: Storage | undefined

  // Memo of the status functions, cleared on any change.
  private winners = new Map<string, string>()
  private acked = new Map<string, boolean>()
  private computing = new Set<string>()

  constructor(o: Options) {
    this.offices = o.offices
    this.host = o.host
    this.journal = o.journal
    this.priced = o.priced ?? new Set()
    const days = o.retentionDays ?? FloorDays
    if (!Number.isSafeInteger(days) || days < FloorDays) throw new Error(`ls_bbox: retention of ${days} days is under the ${FloorDays}-day floor`)
    this.retention = days * 86400
    this.now = o.now ?? (() => Math.floor(Date.now() / 1000))
    for (const l of this.journal.load()) this.apply(l)
  }

  // ---- gauges

  get envelopeCount(): number {
    return this.envelopes.size
  }
  get receiptCount(): number {
    return this.receipts.size
  }
  get sweepCount(): number {
    return this.sweeps.size
  }
  get rowCount(): number {
    return this.rows.size
  }
  get pendingDeletes(): number {
    return this.pending.size
  }

  // ---- outpoint rows

  private row(op: string): Row {
    let r = this.rows.get(op)
    if (r === undefined) this.rows.set(op, (r = { carriers: new Map(), dropped: new Set(), closed: false, sweeps: new Set() }))
    return r
  }

  private apply(l: Line): void {
    const r = this.row(l.op)
    if (l.k === 'c') {
      r.carriers.set(l.c, l)
      this.lines.set(l.c, l)
      for (const a of l.acks ?? []) addTo(this.ackers, a, l.c)
    } else if (l.k === 'd') {
      r.dropped.add(l.c)
      if (l.w) r.closed = true
    } else {
      r.sweeps.add(l.s)
      if (!this.sweepSeen.has(l.s)) this.sweepSeen.set(l.s, l.at)
    }
    this.changed()
  }

  private write(l: Line): void {
    this.journal.append(l)
    this.apply(l)
  }

  private changed(): void {
    this.winners = new Map()
    this.acked = new Map()
  }

  // ---- statuses (spec section 8.2)

  /** The outpoint's winner: the lowest C among its acknowledged envelopes, else among all its carriers. */
  private winnerOf(op: string): string | undefined {
    const memo = this.winners.get(op)
    if (memo !== undefined) return memo
    const r = this.rows.get(op)
    if (r === undefined || r.carriers.size === 0) return undefined
    const all = [...r.carriers.keys()].sort(cmp)
    if (this.computing.has(op)) return all[0]
    this.computing.add(op)
    let w: string | undefined
    try {
      w = all.find((c) => this.isAcknowledged(c)) ?? all[0]
    } finally {
      this.computing.delete(op)
    }
    this.winners.set(op, w!)
    return w
  }

  /** Whether a receipt it is not retracted and is its outpoint's winner, so it acknowledges. */
  private acknowledges(l: CarrierLine): boolean {
    return this.rows.get(l.op)!.sweeps.size === 0 && this.winnerOf(l.op) === l.c
  }

  /** Whether an acknowledging receipt by the envelope's `to` lists envelope c. */
  private isAcknowledged(c: string): boolean {
    const memo = this.acked.get(c)
    if (memo !== undefined) return memo
    const e = this.lines.get(c)
    let yes = false
    if (e?.kind === 'e') {
      for (const rc of this.ackers.get(c) ?? []) {
        const r = this.lines.get(rc)!
        if (r.by === e.to && this.acknowledges(r)) {
          yes = true
          break
        }
      }
    }
    this.acked.set(c, yes)
    return yes
  }

  /** The status of the carrier with commitment c (hash byte order hex). */
  status(c: string): Status | undefined {
    const l = this.lines.get(c)
    if (l === undefined) return undefined
    const r = this.rows.get(l.op)!
    if (r.sweeps.size > 0) return 'retracted'
    if (r.closed || this.winnerOf(l.op) !== c) return 'superseded'
    if (l.kind === 'e' && this.isAcknowledged(c)) return 'acknowledged'
    return 'held'
  }

  /** Whether the carrier with commitment c was dropped. */
  dropped(c: string): boolean {
    const l = this.lines.get(c)
    return l !== undefined && this.rows.get(l.op)!.dropped.has(c)
  }

  private open(e: EnvEntry, now: number): boolean {
    return (
      this.status(e.c) === 'held' &&
      (e.expires === 0 || now < e.expires) &&
      e.created >= now - WindowBefore &&
      e.created <= now + WindowAfter
    )
  }

  // ---- admission

  outputAdmittedByTopic(payload: OutputAdmittedByTopic): void {
    if (payload.mode !== 'whole-tx') return
    const office = this.officeOf(payload.topic)
    if (office === undefined) return
    let tx: Transaction
    try {
      tx = Transaction.fromAtomicBEEF(payload.atomicBEEF)
    } catch {
      this.host.log('ls_bbox ignored an admission whose BEEF did not parse', { topic: payload.topic })
      return
    }
    this.index(office, tx, payload.outputIndex)
  }

  outputEvicted(txid: string, outputIndex: number): void {
    if (outputIndex === 0) this.forget(txid)
  }

  outputNoLongerRetainedInHistory(txid: string, outputIndex: number, t: string): void {
    if (outputIndex === 0 && this.officeOf(t) !== undefined) this.forget(txid)
  }

  private officeOf(t: string): string | undefined {
    if (!t.startsWith(TopicPrefix)) return undefined
    const office = t.slice(TopicPrefix.length)
    return this.offices.has(office) ? office : undefined
  }

  /**
   * Indexes output outputIndex of an admitted transaction on office, writing
   * its rows the first time it is seen. A carrier the rows say was dropped
   * is not indexed again, and its output is deleted.
   */
  private index(office: string, tx: Transaction, outputIndex: number): boolean {
    const out = tx.outputs[outputIndex]
    if (out === undefined || outputIndex !== 0) return false
    const script = Uint8Array.from(out.lockingScript.toBinary())
    const txid = tx.id('hex')
    const kind = claimOf(script)
    const now = this.now()
    if (kind !== 'none') {
      const input = tx.inputs[0]
      const src = input?.sourceTXID ?? input?.sourceTransaction?.id('hex')
      if (input === undefined || src === undefined) return false
      const op = outpoint(src, input.sourceOutputIndex)
      const c = toHex(Uint8Array.from(tx.hash() as number[]))
      const record = firstPush(script)!
      let line: CarrierLine
      let entry: { e?: EnvEntry; r?: RcptEntry }
      if (kind === 'envelope') {
        const e = decodeEnvelope(record)
        const to = toHex(e.to)
        line = { k: 'c', op, c, txid, kind: 'e', office, to, at: now }
        entry = { e: { txid, c, office, to, box: e.box, from: toHex(e.from), created: e.created, expires: e.expires } }
      } else {
        const r = decodeReceipt(record)
        const by = toHex(r.by)
        const acks = r.acks.map(toHex)
        line = { k: 'c', op, c, txid, kind: 'r', office, by, acks, at: now }
        entry = { r: { txid, c, office, by, acks: new Set(acks), created: r.created } }
      }
      if (!this.lines.has(c)) this.write(line)
      if (this.rows.get(op)!.dropped.has(c)) {
        this.pending.set(txid, { txid, topic: topic(office) })
        return false
      }
      if (entry.e !== undefined) {
        this.envelopes.set(txid, entry.e)
        addTo(this.envByTo, `${office}|${entry.e.to}`, txid)
      } else {
        this.receipts.set(txid, entry.r!)
        addTo(this.rcptByBy, `${office}|${entry.r!.by}`, txid)
      }
      this.changed()
      this.enforceEvidence(op)
      return true
    }
    if (!isFundingShape(script)) {
      this.host.log('ls_bbox indexed an output of no known kind', { office, txid })
      return false
    }
    // A sweep, or a published funding tree: every input's outpoint is swept.
    const spent: string[] = []
    for (const input of tx.inputs) {
      const src = input.sourceTXID ?? input.sourceTransaction?.id('hex')
      if (src === undefined) continue
      const op = outpoint(src, input.sourceOutputIndex)
      spent.push(op)
      if (!this.rows.get(op)?.sweeps.has(txid)) {
        this.write({ k: 's', op, s: txid, office, h: tx.merklePath?.blockHeight ?? 0, at: this.sweepSeen.get(txid) ?? now })
        this.host.metrics.inc('bbox_retractions_total')
      }
    }
    this.sweeps.set(txid, { txid, office, spent, height: tx.merklePath?.blockHeight ?? 0 })
    for (const op of spent) addTo(this.sweepsByOp, `${office}|${op}`, txid)
    return true
  }

  /** Removes a held output from the index; its rows stay. */
  private forget(txid: string): void {
    const e = this.envelopes.get(txid)
    if (e !== undefined) {
      this.envelopes.delete(txid)
      removeFrom(this.envByTo, `${e.office}|${e.to}`, txid)
    }
    const r = this.receipts.get(txid)
    if (r !== undefined) {
      this.receipts.delete(txid)
      removeFrom(this.rcptByBy, `${r.office}|${r.by}`, txid)
    }
    const s = this.sweeps.get(txid)
    if (s !== undefined) {
      this.sweeps.delete(txid)
      for (const op of s.spent) removeFrom(this.sweepsByOp, `${s.office}|${op}`, txid)
    }
  }

  // ---- drops (spec section 8.4)

  /**
   * Drops the carrier with commitment c: marks its row (and closes the row
   * when c is its winner), takes it out of every answer, and queues its
   * output for deletion from storage.
   */
  private drop(c: string, why: string): void {
    const l = this.lines.get(c)!
    const r = this.rows.get(l.op)!
    if (r.dropped.has(c)) return
    this.write({ k: 'd', op: l.op, c, w: this.winnerOf(l.op) === c })
    this.forget(l.txid)
    this.pending.set(l.txid, { txid: l.txid, topic: topic(l.office) })
    this.host.metrics.inc('bbox_dropped_total', { why })
    this.host.log('ls_bbox dropped a carrier', { txid: l.txid, outpoint: l.op, why })
  }

  /**
   * Keeps at most 8 superseded carriers on an outpoint (64 when its winner
   * is acknowledged) and drops the rest, highest commitment first.
   */
  private enforceEvidence(op: string): void {
    const r = this.rows.get(op)!
    const w = this.winnerOf(op)
    const cap = w !== undefined && this.isAcknowledged(w) ? EvidenceAcknowledged : Evidence
    const kept = [...r.carriers.keys()].filter((c) => !r.dropped.has(c) && this.status(c) === 'superseded').sort(cmp)
    for (const c of kept.slice(cap).reverse()) this.drop(c, 'evidence')
  }

  /**
   * Applies retention at the host's time: drops every carrier and sweep
   * past its keep, then deletes what was dropped from storage. What is kept:
   * an open envelope, and one dated ahead of the host's clock, always; any
   * other carrier or sweep for the retention period from first sight, at
   * least 31 days. Returns how many were dropped.
   */
  async prune(): Promise<number> {
    const now = this.now()
    let n = 0
    const old = (at: number | undefined): boolean => at !== undefined && now - at >= this.retention
    for (const e of [...this.envelopes.values()]) {
      if (this.open(e, now) || (e.created > now + WindowAfter && this.status(e.c) !== 'retracted')) continue
      if (old(this.lines.get(e.c)?.at)) {
        this.drop(e.c, 'retention')
        n++
      }
    }
    for (const r of [...this.receipts.values()]) {
      if (old(this.lines.get(r.c)?.at)) {
        this.drop(r.c, 'retention')
        n++
      }
    }
    for (const s of [...this.sweeps.values()]) {
      if (old(this.sweepSeen.get(s.txid))) {
        this.forget(s.txid)
        this.pending.set(s.txid, { txid: s.txid, topic: topic(s.office) })
        this.host.metrics.inc('bbox_dropped_total', { why: 'retention' })
        n++
      }
    }
    await this.flush()
    return n
  }

  /** Deletes dropped outputs from storage, when the host's storage can. */
  async flush(): Promise<void> {
    const del = this.storage?.deleteOutput?.bind(this.storage)
    if (del === undefined) return
    for (const [k, p] of [...this.pending]) {
      try {
        await del(p.txid, 0, p.topic)
        this.pending.delete(k)
      } catch (e) {
        this.host.log('ls_bbox could not delete a dropped output', { txid: p.txid, err: String(e) })
      }
    }
  }

  // ---- questions (spec sections 7.2, 7.3 and 8.3)

  /** Asked through the engine: a class the host prices is refused here. */
  async lookup(question: LookupQuestion): Promise<LookupFormula> {
    return this.answer(question, false)
  }

  /** The class of a question, validated, and whether this host prices it. */
  classify(question: LookupQuestion): { q: Question; priced: boolean } {
    if (question.service !== ServiceName) throw new Error(`ls_bbox: this service is ${ServiceName}, not ${String(question.service)}`)
    const raw = question.query
    if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) throw new Error('ls_bbox: the query is not an object')
    let q: Question
    try {
      q = parseQuery(raw as Record<string, unknown>)
    } catch (e) {
      if (e instanceof Refusal) throw new Error(`ls_bbox: refused: ${e.message.replace(/^boxrec: /, '')}`)
      throw e
    }
    if (!this.offices.has(q.office)) throw new Error(`ls_bbox: this host does not carry office ${q.office}`)
    return { q, priced: !q.class.free && this.priced.has(q.class.name) }
  }

  /**
   * Answers a question. paid says the question was paid for (or asked where
   * no payment is due); a class this host prices is refused otherwise.
   */
  async answer(question: LookupQuestion, paid: boolean): Promise<LookupFormula> {
    const { q, priced } = this.classify(question)
    if (priced && !paid) {
      throw new Error(`ls_bbox: ${q.class.name} is priced on this host; ask its terms route's /lookup over BRC-104`)
    }
    this.host.metrics.inc('bbox_lookups_total', { class: q.class.name })
    const now = this.now()
    const key = q.key === undefined ? '' : toHex(q.key)
    const f = (txid: string): { txid: string; outputIndex: number } => ({ txid, outputIndex: 0 })
    const envs = (): EnvEntry[] => [...(this.envByTo.get(`${q.office}|${key}`) ?? [])].map((t) => this.envelopes.get(t)!)
    const page = (list: EnvEntry[]): LookupFormula => {
      list.sort((a, b) => a.created - b.created || cmp(a.txid, b.txid))
      if (q.afterCreated !== undefined) {
        const ac = q.afterCreated
        const at = q.afterTxid!
        list = list.filter((e) => e.created > ac || (e.created === ac && e.txid > at))
      }
      return list.slice(0, q.class.page).map((e) => f(e.txid))
    }
    switch (q.class.name) {
      case 'inbox':
      case 'inbox-after':
        return page(envs().filter((e) => this.open(e, now)))
      case 'box':
      case 'box-after':
        return page(envs().filter((e) => e.box === q.box && this.open(e, now)))
      case 'sender':
      case 'sender-after': {
        const from = toHex(q.from!)
        return page(envs().filter((e) => e.from === from && this.open(e, now)))
      }
      case 'history':
      case 'history-after':
        return page(
          envs().filter((e) => {
            const s = this.status(e.c)
            return (s === 'held' || s === 'acknowledged') && !this.open(e, now) && e.created <= now + WindowAfter
          }),
        )
      case 'receipt': {
        const t = hashOrder(q.receiptFor!)
        const list = [...(this.rcptByBy.get(`${q.office}|${key}`) ?? [])]
          .map((x) => this.receipts.get(x)!)
          .filter((r) => r.acks.has(t) && this.status(r.c) === 'held')
          .sort((a, b) => a.created - b.created || cmp(a.txid, b.txid))
        return list.slice(0, q.class.page).map((r) => f(r.txid))
      }
      case 'sweep': {
        const op = outpoint(q.spentTxid!, q.spentVout!)
        const list = [...(this.sweepsByOp.get(`${q.office}|${op}`) ?? [])]
          .map((x) => this.sweeps.get(x)!)
          .sort((a, b) => a.height - b.height || cmp(a.txid, b.txid))
        return list.slice(0, q.class.page).map((s) => f(s.txid))
      }
    }
    throw new Error(`ls_bbox: no answer for class ${q.class.name}`)
  }

  /**
   * An answer with each output's BEEF, as the engine hydrates one, read from
   * the host's storage: for the terms route, which answers outside the
   * engine.
   */
  async hydrate(formula: LookupFormula): Promise<OutputList> {
    const storage = this.storage
    if (storage === undefined) throw new Error('ls_bbox: storage is not restored yet')
    const outputs: OutputList['outputs'] = []
    for (const { txid, outputIndex } of formula) {
      const o = await storage.findOutput(txid, outputIndex, undefined, undefined, true)
      if (o?.beef !== undefined) outputs.push({ beef: o.beef, outputIndex })
    }
    return { type: 'output-list', outputs }
  }

  get restored(): boolean {
    return this.storage !== undefined
  }

  // ---- restore (spec section 8.5)

  /**
   * Rebuilds the index: the rows from the journal, then every output of
   * this module's topics the host hands over (a row handed without its BEEF
   * is read again from storage with it), then every carrier and sweep the
   * rows name that the unspent rows did not carry (a spent tombstone, say),
   * read from storage. A carrier the rows say was dropped is not indexed,
   * and its output is deleted. Statuses are functions of the set, so the
   * order storage returns rows in changes nothing. A storage failure
   * propagates: a restore that skipped a sweep would serve retracted
   * envelopes.
   */
  async restore(outputs: Output[], storage: RestoreStorage): Promise<number> {
    for (const m of [this.rows, this.lines, this.ackers, this.sweepSeen, this.envelopes, this.envByTo, this.receipts, this.rcptByBy, this.sweeps, this.sweepsByOp, this.pending]) {
      m.clear()
    }
    for (const l of this.journal.load()) this.apply(l)
    const full = storage as Storage
    this.storage = full
    const seen = new Set<string>()
    const queue: Output[] = []
    for (const o of outputs) {
      if (this.officeOf(o.topic) === undefined || o.outputIndex !== 0 || seen.has(`${o.topic}|${o.txid}`)) continue
      seen.add(`${o.topic}|${o.txid}`)
      queue.push(o)
    }
    const named: Array<{ topic: string; txid: string }> = []
    for (const l of this.lines.values()) if (this.offices.has(l.office)) named.push({ topic: topic(l.office), txid: l.txid })
    const sweepOffice = new Map<string, string>()
    for (const l of this.journal.load()) if (l.k === 's') sweepOffice.set(l.s, l.office)
    for (const [txid, office] of sweepOffice) if (this.offices.has(office)) named.push({ topic: topic(office), txid })
    let indexed = 0
    const take = async (o: Output): Promise<void> => {
      let row: Output | null = o
      if (row.beef === undefined) row = (await full.findOutput(o.txid, 0, o.topic, undefined, true)) ?? row
      let tx: Transaction | undefined
      try {
        tx = row.beef === undefined ? undefined : Transaction.fromBEEF(row.beef, row.txid)
      } catch {
        tx = undefined
      }
      if (tx === undefined) {
        this.host.log('ls_bbox could not restore an output without its transaction', { topic: o.topic, txid: o.txid })
        return
      }
      try {
        if (this.index(this.officeOf(o.topic)!, tx, 0)) indexed++
      } catch (e) {
        this.host.log('ls_bbox could not restore an output', { topic: o.topic, txid: o.txid, err: String(e) })
      }
    }
    for (const o of queue) await take(o)
    for (const n of named) {
      if (seen.has(`${n.topic}|${n.txid}`)) continue
      seen.add(`${n.topic}|${n.txid}`)
      const o = await full.findOutput(n.txid, 0, n.topic, undefined, true)
      if (o !== null) await take(o)
    }
    await this.flush()
    return indexed
  }

  async getDocumentation(): Promise<string> {
    return [
      '# ls_bbox',
      '',
      'Answers every office this host carries. Free classes: `inbox`, `inbox-after`,',
      '`box`, `box-after`, `sender`, `sender-after` (open envelopes to a key, 64 a page,',
      'by created then txid), `receipt` (answered receipts by a key listing a txid, at',
      'most 8) and `sweep` (sweeps spending an outpoint, at most 8). Priceable:',
      '`history`, `history-after` (envelopes to a key no longer open). A question must',
      'carry exactly the members of one class; anything else is refused with an error.',
      'An empty answer means only that this host holds nothing that answers.',
    ].join('\n')
  }

  async getMetaData(): Promise<LookupServiceMetaData> {
    return { name: ServiceName, shortDescription: 'Answers bbox envelopes, receipts and sweeps by office and key.' }
  }
}
