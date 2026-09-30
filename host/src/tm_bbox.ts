/**
 * `tm_bbox_<name>_<suffix>`: the topic manager of one office (spec section
 * 8.1), one instance per configured office. The rules are admit.ts's; this
 * file is the engine seam: it runs them on the submission and turns the
 * verdict into admittance instructions.
 *
 * The overlay engine records every transaction a topic manager returns
 * instructions for as applied, empty instructions included: a later
 * submission of the same txid is a duplicate no topic manager is shown
 * again, and every held output it spends is marked spent. A refusal
 * returned as empty instructions would therefore be permanent for that
 * txid; anyone could suppress a genuine carrier by submitting it first in a
 * different BEEF. So every refusal is raised, and the engine records nothing
 * and marks nothing spent: a refused transaction offered again is decided
 * again.
 */
import type { ChainTracker } from '@bsv/sdk'
import type { AdmittanceInstructions, ModuleHost, TopicManager } from '@lightwebinc/bcommon'
import { admit, type TxKind } from './admit.js'
import { DefaultMaxBEEF } from './beef.js'
import { Refusal, TopicPrefix, checkOffice, type Reason } from './boxrec.js'

/**
 * Every label a host counts a refused submission by (spec sections 3.1, 4.6,
 * 5 and 8.1). The record rule `magic` is not among them: an output claims a
 * record only by its magic, so admission never reaches it.
 */
export const Reasons: readonly Reason[] = [
  'too-large',
  'cbor',
  'key-type',
  'missing',
  'type',
  'range',
  'identity',
  'office',
  'box',
  'expires',
  'acks',
  'content-json',
  'content-shape',
  'content-cipher',
  'content-signature',
  'carrier-shape',
  'beef',
  'mineable',
  'unlock',
  'lock',
  'signature',
  'funding',
  'unmined',
  'not-bbox',
]

/** What an admitted transaction was. */
export const AdmitKinds: readonly TxKind[] = ['envelope', 'receipt', 'sweep', 'spend']

/**
 * Raised for every refusal. The engine counts it as a failed topic for this
 * submission and stores nothing.
 */
export class Refused extends Error {
  constructor(
    readonly reason: Reason,
    detail: string,
  ) {
    super(`bbox: refused, ${reason}: ${detail}`)
    this.name = 'Refused'
  }
}

/**
 * The engine has verified a submission's proofs against the host's headers
 * before it calls a topic manager (Transaction.verify on submit). What the
 * sweep rule still needs is that the proof is present and names the
 * transaction, which MerklePath.verify checks before it asks a tracker;
 * this tracker answers yes to what the engine already checked.
 */
export const engineVerified: ChainTracker = {
  isValidRootForHeight: async () => true,
  currentHeight: async () => 0,
}

export class BboxTopicManager implements TopicManager {
  readonly office: string

  constructor(
    readonly topic: string,
    private readonly host: ModuleHost,
    private readonly headers: ChainTracker = engineVerified,
    private readonly maxBEEF: number = DefaultMaxBEEF,
  ) {
    if (!topic.startsWith(TopicPrefix)) throw new Error(`bbox: ${topic} is not a tm_bbox_ topic`)
    this.office = topic.slice(TopicPrefix.length)
    checkOffice(this.office)
    if (!Number.isSafeInteger(maxBEEF) || maxBEEF < DefaultMaxBEEF) throw new Error(`bbox: a BEEF bound of ${maxBEEF} is under the ${DefaultMaxBEEF}-byte floor`)
  }

  async identifyAdmissibleOutputs(beef: number[], previousCoins: number[]): Promise<AdmittanceInstructions> {
    try {
      const a = await admit(beef, previousCoins, { office: this.office, headers: this.headers, maxBEEF: this.maxBEEF })
      this.host.metrics.inc('bbox_admitted_total', { kind: a.kind })
      return { outputsToAdmit: a.outputs, coinsToRetain: a.retain }
    } catch (e) {
      if (!(e instanceof Refusal)) throw e
      this.host.metrics.inc('bbox_refused_total', { reason: e.reason })
      this.host.log('tm_bbox refused', { topic: this.topic, reason: e.reason, detail: e.message })
      throw new Refused(e.reason, e.message.replace(/^boxrec: /, ''))
    }
  }

  /** Nothing is needed: a carrier carries its funding tree, and a sweep names what it spends. */
  async identifyNeededInputs(): Promise<Array<{ txid: string; outputIndex: number }>> {
    return []
  }

  async getDocumentation(): Promise<string> {
    return [
      `# ${this.topic}`,
      '',
      `The bbox office ${this.office}: envelope and receipt carriers, each spending one`,
      "output of its owner's mined funding tree, and the tombstones of mined sweeps,",
      'each admitted on its own validity (spec section 8.1). A transaction that spends',
      'held outputs and claims nothing is accepted for its inputs alone. Every refusal',
      'is raised, so nothing refused is recorded.',
    ].join('\n')
  }

  async getMetaData(): Promise<{ name: string; shortDescription: string }> {
    return { name: this.topic, shortDescription: `Admits the bbox office ${this.office}.` }
  }
}
