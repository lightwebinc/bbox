/**
 * The BEEF shape the carrier's beef rule reads, the twin of the Go
 * boxrec/beef.go: the counts a BEEF declares on the wire, and
 * whether a proof holds only the hashes it needs. Also the shortest-form
 * rule of bcommon's Go guard for the counts inside a transaction, which the
 * payment check holds a BEEF to.
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
 * The first count inside a transaction of a BEEF that is written wider than
 * its shortest form, or undefined: each transaction's input and output
 * counts and every script length, which the Go guard reads as a node's
 * ReadCompactSize does. A count written wide gives one transaction two ids,
 * the hash of its bytes and the id of the transaction written again in its
 * shortest form. The BEEF's own counts and a BUMP's numbers are not held to
 * it, as the guard does not hold them. raw must be a BEEF bcommon's
 * checkBEEF passed, so every count and length the walk reads is present.
 */
export function wideCount(raw: Uint8Array): string | undefined {
  let i = 0
  const varInt = (): { v: number; wide: boolean } => {
    const h = raw[i++]!
    if (h < 0xfd) return { v: h, wide: false }
    const n = h === 0xfd ? 2 : h === 0xfe ? 4 : 8
    let v = 0
    for (let k = n - 1; k >= 0; k--) v = v * 256 + raw[i + k]!
    i += n
    return { v, wide: h === 0xfd ? v < 0xfd : h === 0xfe ? v <= 0xffff : v <= 0xffffffff }
  }
  const tx = (): string | undefined => {
    i += 4 // version
    const ins = varInt()
    if (ins.wide) return `the input count ${ins.v} is not written in its shortest form`
    for (let k = 0; k < ins.v; k++) {
      i += 32 + 4 // outpoint
      const l = varInt()
      if (l.wide) return `input ${k}'s script length ${l.v} is not written in its shortest form`
      i += l.v + 4 // script, sequence
    }
    const outs = varInt()
    if (outs.wide) return `the output count ${outs.v} is not written in its shortest form`
    for (let k = 0; k < outs.v; k++) {
      i += 8 // value
      const l = varInt()
      if (l.wide) return `output ${k}'s script length ${l.v} is not written in its shortest form`
      i += l.v
    }
    i += 4 // locktime
    return undefined
  }
  if (raw.length >= 4 && raw[0] === 0x01 && raw[1] === 0x01 && raw[2] === 0x01 && raw[3] === 0x01) i = 4 + 32
  const v2 = raw[i] === 0x02 // EF BE 00 02, little-endian
  i += 4
  const bumps = varInt().v
  for (let n = 0; n < bumps; n++) {
    varInt() // block height
    const levels = raw[i++]!
    for (let l = 0; l < levels; l++) {
      const leaves = varInt().v
      for (let k = 0; k < leaves; k++) {
        varInt() // offset
        if ((raw[i++]! & 1) === 0) i += 32 // not a duplicate: a hash follows
      }
    }
  }
  const txs = varInt().v
  for (let n = 0; n < txs; n++) {
    if (v2) {
      const format = raw[i++]!
      if (format === 2) {
        i += 32 // txid only
        continue
      }
      if (format === 1) varInt() // BUMP index
    }
    const why = tx()
    if (why !== undefined) return `transaction ${n}: ${why}`
    if (!v2 && raw[i++] === 1) varInt() // has a BUMP: its index
  }
  return undefined
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
