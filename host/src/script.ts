/**
 * Script reading and canonical script rebuilding, the twin of the Go
 * boxrec/script.go. The reader is this package's own rather than
 * the SDK's, so that the Go and TypeScript codecs read every byte string
 * the same way; every script a host checks is compared byte for byte with
 * the one rebuilt here.
 */
import { Hash } from '@bsv/sdk'
import { pushDropScript, strictPublicKey } from '@lightwebinc/bcommon'

export { minimalPushBytes, pushDropScript, pushFields, strictSignature } from '@lightwebinc/bcommon'
import { TagFunding } from './boxrec.js'
import { bytesEq, concat } from './util.js'


/** A funding-tree output locked to key: <key> OP_CHECKSIG <62 62 02> OP_DROP. */
export function fundingScript(key: Uint8Array): Uint8Array {
  return pushDropScript(key, [TagFunding])
}

/** A well-formed funding output under some canonical compressed key. */
export function isFundingShape(s: Uint8Array): boolean {
  if (s.length !== 1 + 33 + 1 + 1 + 3 + 1 || s[0] !== 33) return false
  const key = s.subarray(1, 34)
  if (strictPublicKey(key) === undefined) return false
  return bytesEq(s, fundingScript(key))
}

/** The pay-to-public-key-hash locking script of a compressed key. */
export function p2pkh(key: Uint8Array): Uint8Array {
  return concat([Uint8Array.of(0x76, 0xa9, 0x14), Uint8Array.from(Hash.hash160(Array.from(key))), Uint8Array.of(0x88, 0xac)])
}
