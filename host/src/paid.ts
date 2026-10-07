/**
 * The host's terms route (spec sections 7.3 and 7.4): bcommon's shared
 * route (`@lightwebinc/bcommon/host`, LookupFront) with bbox's names,
 * classes and payment protocol, beside the overlay host, whose base URL is
 * what BRC-180's `metanet.overlays` names for `ls_bbox`. It serves
 *
 *   GET  <base>/ls_bbox/terms     the host terms document (404 when the host prices nothing)
 *   POST <base>/.well-known/auth  the BRC-104 handshake
 *   POST <base>/lookup            BRC-24 questions to ls_bbox, free and priced
 *
 * Metric names (`bbox_*`), log lines, status codes, bodies and ledger lines
 * are the ones this file wrote when it carried the route itself: a
 * session's response budget is taken after the request's signature is
 * verified, and a ledger line puts the host's decision before `at`
 * (`spread`).
 *
 * The overlay host's own /lookup refuses a class the host prices (ls_bbox
 * does), so this route is the only way to it.
 */
import type { ChainTracker } from '@bsv/sdk'
import { parsePrices as sharedPrices, termsDocument as sharedTerms, termsPath, type FrontOptions, type LedgerOptions, type LookupAnswerer } from '@lightwebinc/bcommon/host'
import { LookupService as ServiceName } from './boxrec.js'
import type { BboxLookupService } from './ls_bbox.js'
import { PaymentProtocol } from './payment.js'
import { Classes } from './query.js'

/** The terms route, below the base URL. */
export const TermsPath = termsPath(ServiceName)

/**
 * Parses a host's prices, `history=5,history-after=5`: a comma list of
 * `<class>=<satoshis>`, each a priceable class of spec section 7.2 once,
 * priced 0 to 2^53 - 1. A free class is refused: it never has a price. A
 * class and its `-after` form are priced together or not at all.
 */
export function parsePrices(raw: string | undefined): Map<string, number> {
  return sharedPrices(raw, Classes, 'BBOX_PRICES')
}

/** The host terms document (spec section 7.4), classes in the order of section 7.2. */
export function termsDocument(prices: ReadonlyMap<string, number>): { service: string; terms: number; classes: Array<{ class: string; satoshis: number }> } {
  return sharedTerms(ServiceName, Classes, prices)
}

/** ls_bbox as the shared route asks its questions: a priced answer is hydrated as the engine hydrates one. */
export function answerer(ls: BboxLookupService): LookupAnswerer {
  return {
    get restored() {
      return ls.restored
    },
    classify: (q) => ls.classify(q).q.class.name,
    answer: async (q) => ls.hydrate(await ls.answer(q, true)),
  }
}

/** What the shared route takes of bbox: its names, its classes, its payment protocol and ls_bbox. */
export function bboxRoute(ls: BboxLookupService): Pick<FrontOptions, 'app' | 'service' | 'classes' | 'paymentProtocol' | 'ls' | 'responseBudget'> {
  return { app: 'bbox', service: ServiceName, classes: Classes, paymentProtocol: PaymentProtocol, ls: answerer(ls), responseBudget: 'after-verify' }
}

/** The ledger's layout: the decision before `at`, as bbox has always written it. */
export const Ledger: LedgerOptions = { layout: 'spread' }

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
