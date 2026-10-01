/**
 * The terms route's handshake budget. A BRC-104 handshake is unauthenticated
 * and costs the host a signature on its own process, so the route answers
 * only as many as this budget allows: token buckets, one for the route and
 * one for each remote address, checked before any signature work. A
 * handshake over budget is answered 429 with Retry-After.
 *
 * An address's bucket is checked first, so one address that floods spends
 * its own budget and not the route's. IPv6 addresses share a bucket per /64,
 * which is what one host is usually given; an IPv4-mapped address is its
 * IPv4 address.
 */
import { isIPv6 } from 'node:net'

export interface BudgetConfig {
  /** Handshakes a second the route answers, and the most it answers at once. */
  perSec: number
  burst: number
  /** The same for each remote address. */
  perAddressPerSec: number
  addressBurst: number
}

/**
 * The defaults: a fraction of the roughly 10 handshakes a second a test
 * host's process signs, so its own work keeps most of the process. An
 * honest client shakes hands once per session.
 */
export const DefaultBudget: BudgetConfig = { perSec: 4, burst: 8, perAddressPerSec: 1, addressBurst: 4 }

/** The most addresses with a bucket; past it the least recently seen is forgotten. */
export const MaxAddresses = 10_000

class Bucket {
  constructor(
    public tokens: number,
    public at: number,
  ) {}

  refill(rate: number, burst: number, now: number): void {
    if (now > this.at) this.tokens = Math.min(burst, this.tokens + ((now - this.at) / 1000) * rate)
    this.at = now
  }

  /** Seconds until one token is there, at least 1. */
  wait(rate: number): number {
    return Math.max(1, Math.ceil((1 - this.tokens) / rate))
  }
}

export type Verdict = { ok: true } | { ok: false; limit: 'address' | 'global'; retryAfter: number }

/** The key an address is budgeted under: IPv4 as is, IPv6 by its /64. */
export function addressKey(address: string): string {
  const a = address.trim().toLowerCase()
  const mapped = /^::ffff:([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)$/.exec(a)
  if (mapped !== null) return mapped[1]!
  const bare = a.replace(/%.*$/, '')
  if (!isIPv6(bare)) return a
  const [head, tail] = bare.includes('::') ? bare.split('::') : [bare, undefined]
  const left = head === '' ? [] : head!.split(':')
  const right = tail === undefined || tail === '' ? [] : tail.split(':')
  const groups = tail === undefined ? left : [...left, ...Array<string>(8 - left.length - right.length).fill('0'), ...right]
  return `${groups.slice(0, 4).map((g) => g.replace(/^0+(?=.)/, '')).join(':')}::/64`
}

export class HandshakeBudget {
  private readonly global: Bucket
  /** By address key, least recently seen first. */
  private readonly byAddress = new Map<string, Bucket>()

  constructor(
    readonly c: BudgetConfig,
    private readonly now: () => number = Date.now,
  ) {
    for (const [k, v] of Object.entries(c)) {
      if (!(Number.isFinite(v) && v > 0)) throw new Error(`HandshakeBudget: ${k} must be positive`)
    }
    if (c.burst < 1 || c.addressBurst < 1) throw new Error('HandshakeBudget: a burst must be at least 1')
    this.global = new Bucket(c.burst, now())
  }

  /** Addresses with a bucket. */
  get addresses(): number {
    return this.byAddress.size
  }

  /** Takes one handshake for address from both buckets, or says which refused and how long to wait. */
  take(address: string): Verdict {
    const now = this.now()
    const key = addressKey(address)
    let a = this.byAddress.get(key)
    if (a === undefined) a = new Bucket(this.c.addressBurst, now)
    else this.byAddress.delete(key)
    this.prune(now)
    this.byAddress.set(key, a)
    a.refill(this.c.perAddressPerSec, this.c.addressBurst, now)
    if (a.tokens < 1) return { ok: false, limit: 'address', retryAfter: a.wait(this.c.perAddressPerSec) }
    this.global.refill(this.c.perSec, this.c.burst, now)
    if (this.global.tokens < 1) return { ok: false, limit: 'global', retryAfter: this.global.wait(this.c.perSec) }
    a.tokens -= 1
    this.global.tokens -= 1
    return { ok: true }
  }

  /** Forgets addresses whose buckets refilled, oldest first, and any past the cap, leaving room for one. */
  private prune(now: number): void {
    for (const [key, b] of this.byAddress) {
      if (this.byAddress.size < MaxAddresses) {
        b.refill(this.c.perAddressPerSec, this.c.addressBurst, now)
        if (b.tokens < this.c.addressBurst) break
      }
      this.byAddress.delete(key)
    }
  }
}
