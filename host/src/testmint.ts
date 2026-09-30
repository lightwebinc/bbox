/**
 * Test helpers that make fresh bbox objects the golden vectors do not hold:
 * funding trees mined on a local chain, envelope and receipt carriers at any
 * time and on any funding output, and sweeps. Every key is drawn from a
 * fixed label, so a party is the same on every run. Nothing here is part of
 * the module the host loads.
 */
import { createCipheriv, randomBytes } from 'node:crypto'
import {
  CompletedProtoWallet,
  Hash,
  LockingScript,
  MerklePath,
  PrivateKey,
  ProtoWallet,
  P2PKH,
  PushDrop,
  Transaction,
  UnlockingScript,
  type ChainTracker,
  type CreateActionArgs,
  type CreateActionResult,
} from '@bsv/sdk'
import { LockTime, readerLockingKey } from '@lightwebinc/bcommon'
import { KeyEnvelope, KeySignature, Protocol, encodeEnvelope, encodeReceipt } from './boxrec.js'
import { BRC78Version, rfc3339 } from './content.js'
import { JObject, canonical } from './jcs.js'
import { fundingScript, pushDropScript } from './script.js'
import { concat, toBase64, toHex } from './util.js'

const text = new TextEncoder()
export const sha = (s: string): Uint8Array => Uint8Array.from(Hash.sha256(Array.from(text.encode(s))))

/** A local chain: blocks of two, each a minted transaction beside a fixed sibling. */
export class Chain {
  readonly roots = new Map<number, string>()
  private height: number

  constructor(
    first = 5000,
    extra: Array<{ height: number; merkleRoot: string }> = [],
  ) {
    this.height = first
    for (const h of extra) this.roots.set(h.height, h.merkleRoot)
  }

  /** Gives tx a proof in a new block and returns it. */
  mine(tx: Transaction): Transaction {
    const height = this.height++
    tx.merklePath = new MerklePath(height, [
      [
        { offset: 0, hash: '55'.repeat(32) },
        { offset: 1, hash: tx.id('hex'), txid: true },
      ],
    ])
    this.roots.set(height, tx.merklePath.computeRoot(tx.id('hex')))
    return tx
  }

  get tracker(): ChainTracker {
    return {
      isValidRootForHeight: async (root, height) => this.roots.get(height) === root,
      currentHeight: async () => Math.max(...this.roots.keys()),
    }
  }

  get headers(): Array<{ height: number; merkleRoot: string }> {
    return [...this.roots].map(([height, merkleRoot]) => ({ height, merkleRoot }))
  }
}

/** One identity: its key, its wallet, and its envelope key as any reader derives it. */
export class Party {
  readonly key: PrivateKey
  readonly id: Uint8Array
  readonly hex: string
  readonly wallet: ProtoWallet
  readonly envKey: Uint8Array

  constructor(readonly label: string, key?: PrivateKey) {
    this.key = key ?? PrivateKey.fromHex(toHex(sha(`bbox/test/party/${label}`)))
    this.id = Uint8Array.from(this.key.toPublicKey().encode(true) as number[])
    this.hex = toHex(this.id)
    this.wallet = new ProtoWallet(this.key)
    this.envKey = Uint8Array.from(readerLockingKey(Protocol, KeyEnvelope, this.hex).encode(true) as number[])
  }

  async sign(keyID: string, data: Uint8Array): Promise<Uint8Array> {
    const r = await this.wallet.createSignature({ data: Array.from(data), protocolID: Protocol, keyID, counterparty: 'anyone' })
    return Uint8Array.from(r.signature)
  }
}

let nowhere = 0

/** A transaction from nowhere: one unsigned input of an outpoint no one holds. */
export function fromNowhere(outs: Array<{ satoshis: number; lockingScript: LockingScript }>): Transaction {
  return new Transaction(
    1,
    [{ sourceTXID: toHex(sha(`bbox/test/nowhere/${nowhere++}/${Date.now()}`)), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff }],
    outs,
    0,
  )
}

/** A mined funding tree of n outputs of 1000 satoshis for owner. */
export function fundingTree(owner: Party, n: number, chain: Chain): Transaction {
  const lock = LockingScript.fromBinary(Array.from(fundingScript(owner.envKey)))
  return chain.mine(fromNowhere(Array.from({ length: n }, () => ({ satoshis: 1000, lockingScript: lock }))))
}

/** A BRC-78 message from one party to another, with a random key id and IV. */
function brc78(from: Party, to: Party, plaintext: Uint8Array): Uint8Array {
  const keyID = Uint8Array.from(randomBytes(32))
  const iv = Uint8Array.from(randomBytes(32))
  const invoice = `2-message encryption-${toBase64(keyID)}`
  const mine = from.key.deriveChild(to.key.toPublicKey(), invoice)
  const theirs = to.key.toPublicKey().deriveChild(from.key, invoice)
  const key = Uint8Array.from(mine.deriveSharedSecret(theirs).encode(true) as number[]).subarray(1)
  const c = createCipheriv('aes-256-gcm', key, iv)
  return concat([BRC78Version, from.id, to.id, keyID, iv, c.update(plaintext), c.final(), c.getAuthTag()])
}

export interface Letter {
  office: string
  from: Party
  to: Party
  box: string
  created: number
  expires?: number
  body?: string
}

/** An envelope record: the plaintext sealed to the recipient in a signed BRC-169 envelope. */
export async function envelopeRecord(l: Letter): Promise<Uint8Array> {
  const plain = canonical(new JObject().set('body', l.body ?? 'hello'))
  const doc = new JObject()
    .set('metanetHandles', '1.0')
    .set('recipient', new JObject().set('identityKey', l.to.hex))
    .set('sender', new JObject().set('identityKey', l.from.hex))
    .set('created', rfc3339(l.created))
    .set('payment', null)
    .set('content', toBase64(brc78(l.from, l.to, plain)))
  doc.set('signature', toHex(await l.from.sign(KeySignature, canonical(doc.without('content', 'signature')))))
  return encodeEnvelope({ office: l.office, to: l.to.id, box: l.box, from: l.from.id, created: l.created, expires: l.expires ?? 0, content: canonical(doc), extra: [] })
}

/** A receipt record acknowledging commitments (hash byte order). */
export function receiptRecord(office: string, by: Party, acks: Uint8Array[], created: number): Uint8Array {
  const sorted = [...acks].sort((a, b) => Buffer.compare(a, b))
  return encodeReceipt({ office, by: by.id, acks: sorted, created, extra: [] })
}

/**
 * A carrier of record spending output vout of owner's tree: one input
 * signed through the SDK's PushDrop unlocker, one output locked and signed
 * under owner's envelope key and carrying the whole value.
 */
export async function carrier(tree: Transaction, vout: number, owner: Party, record: Uint8Array, lockTime = LockTime): Promise<Transaction> {
  const sig = await owner.sign(KeyEnvelope, record)
  const unlock = new PushDrop(new CompletedProtoWallet(owner.key)).unlock(Protocol, KeyEnvelope, 'anyone', 'all', false)
  const tx = new Transaction(
    1,
    [{ sourceTransaction: tree, sourceOutputIndex: vout, unlockingScriptTemplate: unlock, sequence: 0 }],
    [{ satoshis: tree.outputs[vout]!.satoshis!, lockingScript: LockingScript.fromBinary(Array.from(pushDropScript(owner.envKey, [record], sig))) }],
    lockTime,
  )
  await tx.sign()
  return tx
}

/** A mined sweep of outputs vouts of owner's tree: its tombstone at output 0. */
export async function sweep(tree: Transaction, vouts: number[], owner: Party, chain: Chain): Promise<Transaction> {
  const unlock = new PushDrop(new CompletedProtoWallet(owner.key)).unlock(Protocol, KeyEnvelope, 'anyone', 'all', false)
  const value = vouts.reduce((n, v) => n + tree.outputs[v]!.satoshis!, 0)
  const tx = new Transaction(
    1,
    vouts.map((v) => ({ sourceTransaction: tree, sourceOutputIndex: v, unlockingScriptTemplate: unlock, sequence: 0xffffffff })),
    [{ satoshis: value, lockingScript: LockingScript.fromBinary(Array.from(fundingScript(owner.envKey))) }],
    0,
  )
  await tx.sign()
  return chain.mine(tx)
}

/**
 * A client wallet that can pay, as BRC-105's client asks of one: each
 * createAction spends a fresh coin mined on chain to the outputs asked for,
 * with change back to itself.
 */
export class PayingWallet extends ProtoWallet {
  constructor(
    private readonly key: PrivateKey,
    private readonly chain: Chain,
  ) {
    super(key)
  }

  coin(): Transaction {
    return this.chain.mine(fromNowhere([{ satoshis: 100000, lockingScript: new P2PKH().lock(this.key.toAddress()) }]))
  }

  async pay(outputs: Array<{ satoshis: number; lockingScript: string }>, coin = this.coin()): Promise<Transaction> {
    const tx = new Transaction(
      1,
      [{ sourceTransaction: coin, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(this.key), sequence: 0xffffffff }],
      [
        ...outputs.map((o) => ({ satoshis: o.satoshis, lockingScript: LockingScript.fromHex(o.lockingScript) })),
        { satoshis: 100000 - outputs.reduce((n, o) => n + o.satoshis, 0) - 100, lockingScript: new P2PKH().lock(this.key.toAddress()) },
      ],
      0,
    )
    await tx.sign()
    return tx
  }

  async createAction(args: CreateActionArgs): Promise<CreateActionResult> {
    const tx = await this.pay((args.outputs ?? []).map((o) => ({ satoshis: o.satoshis, lockingScript: o.lockingScript })))
    return { tx: tx.toAtomicBEEF(), txid: tx.id('hex') }
  }
}

/** A transaction's Atomic BEEF, as a publisher submits it. */
export const atomic = (tx: Transaction): number[] => tx.toAtomicBEEF()

/** A commitment C = txid(K), hash byte order. */
export const commitment = (tx: Transaction): Uint8Array => Uint8Array.from(tx.hash() as number[])
