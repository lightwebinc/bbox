/**
 * Probe transactions on a local chain, and that chain's headers served in the
 * shape the reference host's chain tracker reads (/v1/root/:height, /v1/tip).
 * Every transaction is mined, so admission never depends on script checks.
 */
import { createServer } from 'node:http'
import { randomBytes } from 'node:crypto'
import { LockingScript, MerklePath, Transaction, UnlockingScript, Utils } from '@bsv/sdk'

export class Chain {
  constructor(first = 7000) {
    this.height = first
    this.roots = new Map()
  }

  mine(tx) {
    const height = this.height++
    tx.merklePath = new MerklePath(height, [[
      { offset: 0, hash: randomBytes(32).toString('hex') },
      { offset: 1, hash: tx.id('hex'), txid: true },
    ]])
    this.roots.set(height, tx.merklePath.computeRoot(tx.id('hex')))
    return tx
  }

  get tracker() {
    return {
      isValidRootForHeight: async (root, height) => this.roots.get(height) === root,
      currentHeight: async () => Math.max(...this.roots.keys()),
    }
  }

  /** Serves the headers; resolves to { url, close }. */
  async serve() {
    const srv = createServer((req, res) => {
      const m = /^\/v1\/root\/(\d+)$/.exec(req.url ?? '')
      if (m !== null) {
        const root = this.roots.get(Number(m[1]))
        if (root === undefined) return res.writeHead(404).end()
        return res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ height: Number(m[1]), merkleRoot: root }))
      }
      if (req.url === '/v1/tip') {
        return res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ height: Math.max(this.height, ...this.roots.keys()) }))
      }
      res.writeHead(404).end()
    })
    await new Promise((r) => srv.listen(0, '127.0.0.1', r))
    return { url: `http://127.0.0.1:${srv.address().port}`, close: () => srv.close() }
  }
}

const push = (s) => Utils.toArray(String(s), 'utf8')

function probeScript(tag, a, b, c) {
  const s = new LockingScript()
  s.writeBin(push(tag)).writeBin(push(a)).writeBin(push(b)).writeBin(push(c))
  s.writeOpCode(0x6d).writeOpCode(0x6d).writeOpCode(0x51) // 2DROP 2DROP 1
  return s
}

export const policyScript = (recipient, scope) => probeScript('a13pol', recipient, scope, randomBytes(4).toString('hex'))
export const envelopeScript = (recipient, polTxid, polVout) => probeScript('a13env', recipient, polTxid, String(polVout))

function nowhere() {
  return { sourceTXID: randomBytes(32).toString('hex'), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff }
}

export function policyTx(chain, recipient, scope = 'everyone') {
  return chain.mine(new Transaction(1, [nowhere()], [{ satoshis: 1, lockingScript: policyScript(recipient, scope) }], 0))
}

export function envelopeTx(chain, recipient, pol, vout = 0) {
  return chain.mine(new Transaction(1, [nowhere()], [{ satoshis: 1, lockingScript: envelopeScript(recipient, pol.id('hex'), vout) }], 0))
}

/** Spends policy output 0 of pol into a new policy for the same recipient. */
export function policySpend(chain, recipient, pol, scope) {
  return chain.mine(new Transaction(1, [{ sourceTransaction: pol, sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff }], [{ satoshis: 1, lockingScript: policyScript(recipient, scope) }], 0))
}

/** One transaction carrying a policy (output 0) and an envelope for the same recipient (output 1), the envelope naming no outpoint. */
export function combinedTx(chain, recipient) {
  return chain.mine(new Transaction(1, [nowhere()], [
    { satoshis: 1, lockingScript: policyScript(recipient, 'everyone') },
    { satoshis: 1, lockingScript: envelopeScript(recipient, '', 0) },
  ], 0))
}

export const beef = (tx) => tx.toBEEF()
