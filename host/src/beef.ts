/**
 * The BEEF shape the carrier's beef rule reads, the twin of the Go
 * internal/boxrec/beef.go: the counts a BEEF declares on the wire, and
 * whether a proof holds only the hashes it needs.
 */
import type { MerklePath } from '@bsv/sdk'

/** The BEEF bound admission applies when a host names none: spec section 12's floor. */
export const DefaultMaxBEEF = 256 << 10

/**
 * The BUMP and transaction counts a BEEF declares on the wire, before any
 * parser merges proofs of one block or collapses a transaction listed twice.
 */
export function beefCounts(b: Uint8Array): { bumps: number; txs: number } | undefined {
  let i = 0
  const byte = (): number | undefined => (i < b.length ? b[i++] : undefined)
  const varInt = (): number | undefined => {
    const h = byte()
    if (h === undefined) return undefined
    const n = h === 0xfd ? 2 : h === 0xfe ? 4 : h === 0xff ? 8 : 0
    if (n === 0) return h
    if (i + n > b.length) return undefined
    let v = 0
    for (let k = n - 1; k >= 0; k--) v = v * 256 + b[i + k]!
    i += n
    return v
  }
  if (b.length >= 4 && b[0] === 0x01 && b[1] === 0x01 && b[2] === 0x01 && b[3] === 0x01) i = 4 + 32
  i += 4
  const bumps = varInt()
  if (bumps === undefined) return undefined
  for (let n = 0; n < bumps; n++) {
    if (varInt() === undefined) return undefined
    const levels = byte()
    if (levels === undefined) return undefined
    for (let l = 0; l < levels; l++) {
      const leaves = varInt()
      if (leaves === undefined) return undefined
      for (let k = 0; k < leaves; k++) {
        if (varInt() === undefined) return undefined
        const flags = byte()
        if (flags === undefined) return undefined
        if ((flags & 1) === 0) i += 32
      }
    }
  }
  const txs = varInt()
  if (txs === undefined || i > b.length) return undefined
  return { bumps, txs }
}

/**
 * Whether mp proves txid (display hex) with only the hashes that needs: at
 * the lowest level the txid, flagged as one, and its sibling (a hash or a
 * duplicate), and above it exactly the one sibling each level needs.
 */
export function minimalPath(mp: MerklePath, txid: string): boolean {
  const level0 = mp.path[0]
  if (level0 === undefined || level0.length !== 2) return false
  const at = level0.find((e) => e.hash === txid && e.txid === true)
  if (at === undefined) return false
  for (const [level, leaves] of mp.path.entries()) {
    const node = Math.floor(at.offset / 2 ** level)
    const want = node % 2 === 0 ? node + 1 : node - 1
    let n = 0
    for (const e of leaves) {
      if (e === at) continue
      if (e.offset !== want || e.txid === true) return false
      n++
    }
    if (n !== 1) return false
  }
  return true
}
