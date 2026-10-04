/**
 * The ls_bbox question classes and their validation, spec section 7.2: the
 * twin of the Go boxrec/query.go. A question is validated as
 * parsed (JSON.parse's object, where a member given twice took its last
 * value) and must carry exactly the members of one class.
 */
import { MaxTime, Refusal, checkBox, checkIdentity, checkOffice } from './boxrec.js'
import { fromHex, isLowerHex } from './util.js'

export interface Class {
  name: string
  /** Sorted. */
  members: string[]
  free: boolean
  page: number
}

export const PageEnvelopes = 64
export const PageReceipts = 8
export const PageSweeps = 8

export const Classes: Class[] = [
  { name: 'inbox', members: ['office', 'to'], free: true, page: PageEnvelopes },
  { name: 'inbox-after', members: ['after', 'office', 'to'], free: true, page: PageEnvelopes },
  { name: 'box', members: ['box', 'office', 'to'], free: true, page: PageEnvelopes },
  { name: 'box-after', members: ['after', 'box', 'office', 'to'], free: true, page: PageEnvelopes },
  { name: 'sender', members: ['from', 'office', 'to'], free: true, page: PageEnvelopes },
  { name: 'sender-after', members: ['after', 'from', 'office', 'to'], free: true, page: PageEnvelopes },
  { name: 'receipt', members: ['by', 'office', 'receiptFor'], free: true, page: PageReceipts },
  { name: 'sweep', members: ['office', 'spent'], free: true, page: PageSweeps },
  { name: 'history', members: ['history', 'office'], free: false, page: PageEnvelopes },
  { name: 'history-after', members: ['after', 'history', 'office'], free: false, page: PageEnvelopes },
]

export interface Question {
  class: Class
  office: string
  /** to, by or history. */
  key?: Uint8Array
  from?: Uint8Array
  box?: string
  afterCreated?: number
  /** Display order. */
  afterTxid?: string
  receiptFor?: string
  spentTxid?: string
  spentVout?: number
}

function keyHex(s: string): Uint8Array | undefined {
  if (!isLowerHex(s, 66)) return undefined
  const b = fromHex(s)
  try {
    checkIdentity(b)
  } catch {
    return undefined
  }
  return b
}

const digits = /^[0-9]+$/

/** A page cursor: decimal created (1 to MaxTime, no leading zero), a colon, a display txid. */
export function parseAfter(s: string): { created: number; txid: string } | undefined {
  const at = s.indexOf(':')
  if (at < 0) return undefined
  const c = s.slice(0, at)
  const t = s.slice(at + 1)
  if (c.length === 0 || c.length > 12 || c[0] === '0' || !digits.test(c) || !isLowerHex(t, 64)) return undefined
  const n = Number(c)
  return n > MaxTime ? undefined : { created: n, txid: t }
}

/** An outpoint as BRC-100's OutpointString writes one: a display txid, a dot, a decimal index. */
export function parseOutpoint(s: string): { txid: string; vout: number } | undefined {
  const at = s.indexOf('.')
  if (at < 0) return undefined
  const t = s.slice(0, at)
  const v = s.slice(at + 1)
  if (!isLowerHex(t, 64) || v.length === 0 || v.length > 10 || (v.length > 1 && v[0] === '0') || !digits.test(v)) return undefined
  const n = Number(v)
  return n > 0xffffffff ? undefined : { txid: t, vout: n }
}

/** Validates a BRC-24 query object; anything else is refused, never answered empty. */
export function parseQuery(q: Record<string, unknown>): Question {
  const names = Object.keys(q).sort()
  const cls = Classes.find((c) => c.members.join(',') === names.join(','))
  if (cls === undefined) throw new Refusal('query', `members ${names.join(',')}`)
  const out: Question = { class: cls, office: '' }
  for (const n of names) {
    const s = q[n]
    if (typeof s !== 'string') throw new Refusal('query', `${n} is not a string`)
    const fail = (): never => {
      throw new Refusal('query', n)
    }
    switch (n) {
      case 'office':
        try {
          checkOffice(s)
        } catch {
          fail()
        }
        out.office = s
        break
      case 'to':
      case 'by':
      case 'history':
        out.key = keyHex(s) ?? fail()
        break
      case 'from':
        out.from = keyHex(s) ?? fail()
        break
      case 'spent': {
        const o = parseOutpoint(s) ?? fail()
        out.spentTxid = o.txid
        out.spentVout = o.vout
        break
      }
      case 'box':
        try {
          checkBox(s)
        } catch {
          fail()
        }
        out.box = s
        break
      case 'after': {
        const a = parseAfter(s) ?? fail()
        out.afterCreated = a.created
        out.afterTxid = a.txid
        break
      }
      case 'receiptFor':
        if (!isLowerHex(s, 64)) fail()
        out.receiptFor = s
        break
    }
  }
  return out
}
