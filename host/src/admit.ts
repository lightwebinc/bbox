/**
 * The topic manager's transaction rules of spec section 8.1, the twin of
 * the Go internal/boxrec/admit.go: classification, then the carrier's rules
 * 1 to 10, the sweep rule, or the spend rule. A transaction is refused for
 * the first rule it breaks, with the same reason label in both languages,
 * and every refusal is thrown: a topic manager raises it rather than answer
 * with empty instructions, so the engine records nothing for the txid.
 */
import { Beef, type ChainTracker, type Transaction } from '@bsv/sdk'
import { LockTime, MaxSequence, readerLockingKey, unlockingRefusal, verifyFieldSignature } from '@lightwebinc/bcommon'
import {
  KeyEnvelope,
  Protocol,
  Refusal,
  checkOffice,
  claimOf,
  decodeEnvelope,
  decodeReceipt,
  firstPush,
  type Envelope,
  type Kind,
  type Receipt,
} from './boxrec.js'
import { DefaultMaxBEEF, beefCounts, minimalPath } from './beef.js'
import { checkContent, type Content } from './content.js'
import { fundingScript, isFundingShape, pushDropScript, pushFields, strictSignature } from './script.js'
import { bytesEq, toHex } from './util.js'

export interface Host {
  /** The topic's office identifier. */
  office: string
  /** A chain tracker over the host's own block headers. */
  headers?: ChainTracker
  /** The host's BEEF bound; DefaultMaxBEEF when omitted. */
  maxBEEF?: number
}

export interface Carrier {
  envelope?: Envelope
  content?: Content
  receipt?: Receipt
  /** The record's from or by. */
  owner: Uint8Array
  /** The record exactly as carried. */
  record: Uint8Array
  /** C = txid(K), hash byte order. */
  commitment: Uint8Array
  /** The outpoint the carrier spends, txid in display order. */
  funding: { txid: string; vout: number }
}

export type TxKind = 'envelope' | 'receipt' | 'sweep' | 'spend'

export interface Admission {
  kind: TxKind
  /** The transaction's txid, hash byte order. */
  txid: Uint8Array
  /** Output indices admitted (outputsToAdmit). */
  outputs: number[]
  /** Input indices that spend a held output, each retained (coinsToRetain). */
  retain: number[]
  carrier?: Carrier
  /** For a sweep, every outpoint its inputs spend, txid in display order. */
  spent?: Array<{ txid: string; vout: number }>
}

const bin = (s: { toBinary(): number[] }): Uint8Array => Uint8Array.from(s.toBinary())

/** Whether tx carries a proof that verifies against the headers. */
export async function mined(tx: Transaction, headers?: ChainTracker): Promise<boolean> {
  if (tx.merklePath === undefined || headers === undefined) return false
  try {
    return await tx.merklePath.verify(tx.id('hex'), headers)
  } catch {
    return false
  }
}

/**
 * Parses the BEEF as bcommon's guard does before its parser runs: within the
 * bound, one BEEF with nothing after it, at least one transaction, every
 * transaction with at least one input and every proof with at least one
 * level. The subject is the Atomic BEEF's, or else the last transaction.
 */
function parseBEEF(raw: Uint8Array, bound: number): { beef: Beef; tx: Transaction } {
  if (raw.length > bound) throw new Refusal('beef', `${raw.length} bytes, max ${bound}`)
  let beef: Beef
  try {
    beef = Beef.fromBinaryView(Uint8Array.from(raw))
  } catch (e) {
    throw new Refusal('beef', (e as Error).message)
  }
  if (beef.txs.length === 0 || beef.bumps.some((b) => b.path.length === 0) || beef.txs.some((t) => t.tx !== undefined && t.tx.inputs.length === 0)) {
    throw new Refusal('beef', 'an empty BEEF, proof or transaction')
  }
  const target = beef.atomicTxid ?? beef.txs.at(-1)!.txid
  const tx = beef.findAtomicTransaction(target)
  if (tx === undefined) throw new Refusal('beef', 'no subject transaction')
  return { beef, tx }
}

function retained(tx: Transaction, held: number[]): number[] {
  for (const i of held) {
    if (!Number.isInteger(i) || i < 0 || i >= tx.inputs.length) throw new Error(`boxrec: previous coin names input ${i} of ${tx.inputs.length}`)
  }
  return [...new Set(held)].sort((a, b) => a - b)
}

/**
 * Applies spec section 8.1 to one submission: the BEEF as it arrived, and
 * held, the indices of its subject's inputs that spend an output the topic
 * holds (BRC-22's previous coins). SPV of the BEEF is the engine's and is
 * assumed to have run.
 */
export async function admit(raw: Uint8Array | number[], held: number[], h: Host): Promise<Admission> {
  try {
    checkOffice(h.office)
  } catch {
    throw new Error(`boxrec: host office ${JSON.stringify(h.office)} breaks the grammar`)
  }
  const bytes = Uint8Array.from(raw)
  const { beef, tx } = parseBEEF(bytes, h.maxBEEF ?? DefaultMaxBEEF)
  const retain = retained(tx, held)
  const a: Admission = { kind: 'spend', txid: Uint8Array.from(tx.hash() as number[]), outputs: [], retain }

  // Classify: the one output that claims a record.
  let claim: Kind = 'none'
  let claims = 0
  for (const o of tx.outputs) {
    const k = claimOf(bin(o.lockingScript))
    if (k !== 'none') {
      claim = k
      claims++
    }
  }
  if (claims > 1) throw new Refusal('carrier-shape', `${claims} outputs claim a record`)
  if (claims === 1) {
    a.carrier = checkCarrier(bytes, beef, tx, claim, h)
    a.kind = claim === 'envelope' ? 'envelope' : 'receipt'
    a.outputs = [0]
    return a
  }

  // Sweep: output 0 funding-shaped under any key, and mined.
  const first = tx.outputs[0]
  if (first !== undefined && isFundingShape(bin(first.lockingScript))) {
    if (!(await mined(tx, h.headers))) throw new Refusal('unmined')
    a.kind = 'sweep'
    a.outputs = [0]
    a.spent = tx.inputs.map((i) => ({ txid: i.sourceTXID ?? i.sourceTransaction!.id('hex'), vout: i.sourceOutputIndex }))
    return a
  }

  // Spend: it retains what it spends, and admits nothing.
  if (retain.length > 0) return a
  throw new Refusal('not-bbox')
}

function derived(owner: Uint8Array): Uint8Array {
  return Uint8Array.from(readerLockingKey(Protocol, KeyEnvelope, toHex(owner)).encode(true) as number[])
}

/** The carrier's rules 1 to 10; 5 to 8 in bcommon carrier.Validate's order. */
function checkCarrier(raw: Uint8Array, beef: Beef, tx: Transaction, claim: Kind, h: Host): Carrier {
  // 1. carrier-shape.
  if (tx.inputs.length !== 1 || tx.outputs.length !== 1) throw new Refusal('carrier-shape', `${tx.inputs.length} inputs, ${tx.outputs.length} outputs`)
  const input = tx.inputs[0]!
  const out = tx.outputs[0]!
  const src = input.sourceTransaction?.outputs[input.sourceOutputIndex]
  if (src === undefined) throw new Refusal('beef', 'the spent output is not in the BEEF')
  if ((src.satoshis ?? 0) !== (out.satoshis ?? 0)) throw new Refusal('carrier-shape', 'value')
  // 2. beef.
  checkCarrierBEEF(raw, beef, tx)
  // 3. the record's own rules.
  const lock = bin(out.lockingScript)
  const record = firstPush(lock, 35)!
  let c: Carrier
  let office: string
  const parent = input.sourceTXID ?? input.sourceTransaction!.id('hex')
  const base = { record, commitment: Uint8Array.from(tx.hash() as number[]), funding: { txid: parent, vout: input.sourceOutputIndex } }
  if (claim === 'envelope') {
    const e = decodeEnvelope(record)
    c = { ...base, envelope: e, owner: e.from }
    office = e.office
  } else {
    const r = decodeReceipt(record)
    c = { ...base, receipt: r, owner: r.by }
    office = r.office
  }
  // 4. office.
  if (office !== h.office) throw new Refusal('office', "not this topic's office")
  // 5. mineable.
  if (tx.lockTime < LockTime || (input.sequence ?? MaxSequence) === MaxSequence) throw new Refusal('mineable')
  // 6. unlock, bcommon's check.
  if (unlockingRefusal(tx) !== undefined) throw new Refusal('unlock')
  // 7. lock.
  const key = derived(c.owner)
  const fields = pushFields(lock)
  if (fields?.length !== 2) throw new Refusal('lock', 'want the record and a signature')
  const sig = fields[1]!
  if (!bytesEq(lock, pushDropScript(key, [record], sig))) throw new Refusal('lock')
  // 8. signature.
  if (!strictSignature(sig) || !verifyFieldSignature(readerLockingKey(Protocol, KeyEnvelope, toHex(c.owner)), Array.from(record), Array.from(sig))) {
    throw new Refusal('signature')
  }
  // 9. funding.
  if (!bytesEq(bin(src.lockingScript), fundingScript(key))) throw new Refusal('funding')
  // 10. the content rules.
  if (c.envelope !== undefined) c.content = checkContent(c.envelope)
  return c
}

/**
 * Rule 2: the BEEF declares exactly one BUMP and two transactions, which are
 * the carrier (unproven) and the transaction its input spends (proven by
 * that BUMP, minimally).
 */
function checkCarrierBEEF(raw: Uint8Array, beef: Beef, tx: Transaction): void {
  const n = beefCounts(raw)
  if (n === undefined || n.bumps !== 1 || n.txs !== 2 || beef.bumps.length !== 1 || beef.txs.length !== 2) {
    throw new Refusal('beef', `${n?.bumps} BUMPs and ${n?.txs} transactions`)
  }
  const input = tx.inputs[0]!
  const parent = input.sourceTXID ?? input.sourceTransaction!.id('hex')
  const self = beef.findTxid(tx.id('hex'))
  const fund = beef.findTxid(parent)
  if (self?.tx === undefined || fund?.tx === undefined) throw new Refusal('beef', 'not the carrier and its funding tree')
  if (self.bumpIndex !== undefined || fund.bumpIndex !== 0) throw new Refusal('beef', "the proof is not the funding tree's alone")
  if (!minimalPath(beef.bumps[0]!, parent)) throw new Refusal('beef', "the proof holds more than the funding tree's path")
}
