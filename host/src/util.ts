/** Byte helpers shared by the codec. */
import { Utils } from '@bsv/sdk'

export const fromHex = (s: string): Uint8Array => Uint8Array.from(s.match(/../g) ?? [], (b) => parseInt(b, 16))
export const toHex = (b: Uint8Array | number[]): string => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('')

export function bytesEq(a: Uint8Array, b: Uint8Array): boolean {
  return a.length === b.length && a.every((x, i) => x === b[i])
}

export function concat(parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0))
  let at = 0
  for (const p of parts) {
    out.set(p, at)
    at += p.length
  }
  return out
}

/** Whether s is n lowercase hex digits. */
export const isLowerHex = (s: string, n: number): boolean => s.length === n && /^[0-9a-f]*$/.test(s)

/** The display txid of a hash-order commitment: its bytes reversed, in hex. */
export const displayTxid = (c: Uint8Array): string => toHex(Uint8Array.from(c).reverse())

const b64 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/

/**
 * Decodes s when it is non-empty standard base64 with padding and zero pad
 * bits, the one spelling of its bytes; undefined otherwise.
 */
export function strictBase64(s: string): Uint8Array | undefined {
  if (s.length === 0 || !b64.test(s)) return undefined
  const b = Uint8Array.from(Utils.toArray(s, 'base64'))
  return Utils.toBase64(Array.from(b)) === s ? b : undefined
}

export const toBase64 = (b: Uint8Array): string => Utils.toBase64(Array.from(b))
