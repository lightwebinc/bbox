/**
 * Gap A13 probe module: two topics and one lookup, loadable by the reference
 * host by path (OVERLAY_MODULES) and constructible in process.
 *
 *   tm_a13pol_qzv  topic A, "policy": admits every policy output.
 *                  A policy output names a recipient and a scope.
 *   tm_a13env_qzv  topic B, "envelope": admits an envelope output only if
 *                  the recipient's policy, READ FROM TOPIC A AT ADMISSION,
 *                  exists, is unspent and has scope "everyone". This is the
 *                  bbox design section 4.2 policy-at-admission seam.
 *   ls_a13_qzv     a lookup over topic A's policy outputs, by recipient.
 *
 * Every call of B's manager reads topic A by every path a module can reach
 * and records what each path saw, then decides by one of them:
 *
 *   p1  storage.findOutput(policy outpoint, topic A): the engine's own
 *       Storage. A manager is handed no storage by the engine or by the
 *       reference host; the only way a module gets it on the reference host
 *       is to keep the object the host passes to a lookup's restore().
 *   p2  storage.findUTXOsForTopic(topic A), scanned for the recipient.
 *   p3  the module's own index, fed by the engine's lookup notifications
 *       (outputAdmittedByTopic, outputSpent, outputEvicted,
 *       outputNoLongerRetainedInHistory) and rebuilt by restore().
 *   p4  a lookup through the engine: engine.lookup() in process, or HTTP
 *       POST /lookup to the host itself (A13_SELF_URL) on the reference host.
 *
 * Configuration (environment, or the options of seamModule()):
 *   A13_PATH     which path decides: p1 (default), p2, p3 or p4
 *   A13_REFUSE   throw (default; the engine records nothing) or empty
 *                (return no outputs; the engine records the transaction as
 *                applied)
 *   A13_RETAIN   1: topic A retains a spent policy (row kept, spent=true);
 *                default 0: the spent policy is removed from storage
 *   A13_LOG      a file each decision is appended to as one JSON line
 *   A13_NODE     a name written into every record
 *   A13_SELF_URL this host's base URL, for p4 on the reference host
 */
import { appendFileSync } from 'node:fs'
import { LockingScript, Transaction, Utils } from '@bsv/sdk'

export const POL = 'tm_a13pol_qzv'
export const ENV = 'tm_a13env_qzv'
export const LS = 'ls_a13_qzv'

const utf8 = (b) => Utils.toUTF8(b ?? [])

/** Reads a probe output script: [tag, recipient, x, y] drop drop drop drop 1. */
export function parse(script) {
  const s = Array.isArray(script) ? LockingScript.fromBinary(script) : script
  const c = s.chunks
  if (c.length !== 7 || c[6].op !== 0x51) return undefined
  const tag = utf8(c[0].data)
  if (tag === 'a13pol') return { kind: 'pol', recipient: utf8(c[1].data), scope: utf8(c[2].data) }
  if (tag === 'a13env') {
    const ref = utf8(c[2].data)
    return { kind: 'env', recipient: utf8(c[1].data), polTxid: ref, polVout: Number(utf8(c[3].data)) }
  }
  return undefined
}

/** The module. `opts` overrides the environment; `state.engine` is set by an in-process driver for p4. */
export function seamModule(opts = {}) {
  const env = process.env
  const cfg = {
    path: opts.path ?? env.A13_PATH ?? 'p1',
    refuse: opts.refuse ?? env.A13_REFUSE ?? 'throw',
    retain: (opts.retain ?? env.A13_RETAIN ?? '0') === '1' || opts.retain === true,
    log: opts.log ?? env.A13_LOG,
    node: opts.node ?? env.A13_NODE ?? 'node',
    selfURL: opts.selfURL ?? env.A13_SELF_URL,
  }
  const state = {
    cfg,
    storage: opts.storage ?? null,
    engine: null,
    // outpoint -> { recipient, scope, spent }
    index: new Map(),
    records: [],
  }

  const record = (r) => {
    r.node = cfg.node
    r.at = Date.now()
    state.records.push(r)
    if (cfg.log !== undefined) appendFileSync(cfg.log, JSON.stringify(r) + '\n')
  }

  const p1 = async (e) => {
    if (state.storage === null) return 'unreachable'
    const o = await state.storage.findOutput(e.polTxid, e.polVout, POL)
    if (o === null || o === undefined) return 'absent'
    if (o.spent) return 'spent'
    const p = parse(o.outputScript)
    return p?.scope === 'everyone' ? 'admit' : 'scope'
  }

  const p2 = async (e) => {
    if (state.storage === null) return 'unreachable'
    const rows = await state.storage.findUTXOsForTopic(POL)
    const mine = rows.map((o) => parse(o.outputScript)).filter((p) => p?.recipient === e.recipient)
    if (mine.length === 0) return 'absent'
    return mine.some((p) => p.scope === 'everyone') ? 'admit' : 'scope'
  }

  const p3 = async (e) => {
    const mine = [...state.index.values()].filter((v) => v.recipient === e.recipient && !v.spent)
    if (mine.length === 0) return 'absent'
    return mine.some((v) => v.scope === 'everyone') ? 'admit' : 'scope'
  }

  const p4 = async (e) => {
    let answer
    const question = { service: LS, query: { recipient: e.recipient } }
    if (state.engine !== null) {
      answer = await state.engine.lookup(question)
    } else if (cfg.selfURL !== undefined) {
      const res = await fetch(`${cfg.selfURL}/lookup`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(question) })
      if (res.status !== 200) return `error-${res.status}`
      answer = await res.json()
    } else {
      return 'unreachable'
    }
    const outs = answer?.outputs ?? []
    if (outs.length === 0) return 'absent'
    const scopes = outs.map((o) => parse(Transaction.fromBEEF(o.beef).outputs[o.outputIndex].lockingScript)?.scope)
    return scopes.includes('everyone') ? 'admit' : 'scope'
  }

  const reads = { p1, p2, p3, p4 }

  const policyManager = {
    identifyAdmissibleOutputs: async (beef, previousCoins) => {
      const tx = Transaction.fromBEEF(beef)
      const outputsToAdmit = []
      tx.outputs.forEach((o, i) => {
        if (parse(o.lockingScript)?.kind === 'pol') outputsToAdmit.push(i)
      })
      return { outputsToAdmit, coinsToRetain: cfg.retain ? [...previousCoins] : [] }
    },
    getDocumentation: async () => 'A13 probe: policy topic',
    getMetaData: async () => ({ name: POL, shortDescription: 'A13 probe policy topic' }),
  }

  const envelopeManager = {
    identifyAdmissibleOutputs: async (beef, previousCoins, _offChain, mode, context) => {
      const tx = Transaction.fromBEEF(beef)
      const txid = tx.id('hex')
      const outputsToAdmit = []
      let refused = 0
      for (const [i, o] of tx.outputs.entries()) {
        const e = parse(o.lockingScript)
        if (e?.kind !== 'env') continue
        const seen = {}
        for (const [name, fn] of Object.entries(reads)) {
          try {
            seen[name] = await fn(e)
          } catch (err) {
            seen[name] = `error:${String(err?.message ?? err).slice(0, 80)}`
          }
        }
        const admit = seen[cfg.path] === 'admit'
        record({ txid, vout: i, recipient: e.recipient, mode: mode ?? 'current-tx', dryRun: context?.dryRun === true, path: cfg.path, seen, admit })
        if (admit) outputsToAdmit.push(i)
        else refused++
      }
      if (refused > 0 && outputsToAdmit.length === 0 && cfg.refuse === 'throw') {
        throw new Error('a13: envelope refused by recipient policy')
      }
      return { outputsToAdmit, coinsToRetain: [] }
    },
    getDocumentation: async () => 'A13 probe: envelope topic',
    getMetaData: async () => ({ name: ENV, shortDescription: 'A13 probe envelope topic' }),
  }

  const lookup = {
    admissionMode: 'locking-script',
    spendNotificationMode: 'none',
    outputAdmittedByTopic: async (p) => {
      if (p.topic !== POL || p.mode !== 'locking-script') return
      const v = parse(p.lockingScript)
      if (v?.kind === 'pol') state.index.set(`${p.txid}.${p.outputIndex}`, { recipient: v.recipient, scope: v.scope, spent: false })
    },
    outputSpent: async (p) => {
      if (p.topic !== POL) return
      const v = state.index.get(`${p.txid}.${p.outputIndex}`)
      if (v !== undefined) v.spent = true
    },
    outputEvicted: async (txid, outputIndex) => {
      state.index.delete(`${txid}.${outputIndex}`)
    },
    outputNoLongerRetainedInHistory: async (txid, outputIndex, topic) => {
      if (topic === POL) state.index.delete(`${txid}.${outputIndex}`)
    },
    lookup: async (q) => {
      const r = q?.query?.recipient
      return [...state.index.entries()]
        .filter(([, v]) => v.recipient === r && !v.spent)
        .map(([k]) => {
          const [txid, vout] = k.split('.')
          return { txid, outputIndex: Number(vout) }
        })
    },
    // The only door to the engine's storage the reference host opens to a
    // module: the object handed to restore() at startup, kept here.
    restore: async (outputs, storage) => {
      state.storage = storage
      let n = 0
      for (const o of outputs) {
        if (o.topic !== POL) continue
        const v = parse(o.outputScript)
        if (v?.kind === 'pol') {
          state.index.set(`${o.txid}.${o.outputIndex}`, { recipient: v.recipient, scope: v.scope, spent: o.spent })
          n++
        }
      }
      return n
    },
    getDocumentation: async () => 'A13 probe lookup',
    getMetaData: async () => ({ name: LS, shortDescription: 'A13 probe lookup' }),
  }

  return { module: { topics: { [POL]: policyManager, [ENV]: envelopeManager }, lookups: { [LS]: lookup } }, state }
}

/** The reference host's entry point: create(host). */
export default function create(host) {
  const { module, state } = seamModule()
  host.log('a13 probe module loaded', { path: state.cfg.path, refuse: state.cfg.refuse, retain: state.cfg.retain })
  return module
}
