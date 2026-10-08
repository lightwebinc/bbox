/**
 * The sender's builders (send.ts) against the Go-generated vectors: with
 * the vector's sender key in a ProtoWallet (the surface a BRC-100 wallet
 * offers) and its fixed BRC-78 key id, every envelope's sealed content and
 * record come out byte for byte as Go wrote them (envelope-v1.json); every
 * envelope and receipt carrier as Go minted it from the same tree
 * (transaction-v1.json); and the payment's destinations and member as the
 * Go sender built them (payment-v1.json). The BRC-78 ciphertext is the one
 * place a wallet draws randomness (its IV), so the envelope check hands the
 * sealer the vector's ciphertext after asking the real wallet to encrypt;
 * the recipient's wallet opens the real ciphertext too.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { Beef, PrivateKey, ProtoWallet, Transaction, Utils } from '@bsv/sdk'
import { decodeEnvelope } from './boxrec.js'
import { parseJSON, canonical, JObject } from './jcs.js'
import { parsePlaintext } from './payment.js'
import { checkContent, verifyContentSignature } from './content.js'
import { encodePlaintext, envelopeCarrier, envelopeRecord, paymentDestination, paymentMember, seal, sealEnvelope, type SealWallet } from './send.js'
import { fromHex, toHex } from './util.js'

const vec = <T>(n: string): T => JSON.parse(readFileSync(new URL(`../../testdata/vectors/${n}`, import.meta.url), 'utf8')) as T
const sender = PrivateKey.fromHex('42'.repeat(32))
const recipient = PrivateKey.fromHex('43'.repeat(32))
const sw = new ProtoWallet(sender)
const rw = new ProtoWallet(recipient)

interface Env {
  name: string
  office: string
  to: string
  box: string
  from: string
  created: string
  expires: string
  plaintext: string
  brc78KeyId: string
  brc78: string
  signedJson: string
  content: string
  record: string
}

test('envelope-v1: every sealed envelope and its record, byte for byte, through the wallet', async () => {
  const v = vec<{ envelopes: Env[] }>('envelope-v1.json')
  assert.ok(v.envelopes.length > 0)
  for (const e of v.envelopes) {
    const brc78 = fromHex(e.brc78)
    const ciphertext = brc78.subarray(4 + 33 + 33 + 32)
    const plaintext = new TextEncoder().encode(e.plaintext)
    let asked: unknown
    const w: SealWallet = {
      createSignature: (a) => sw.createSignature(a),
      encrypt: async (a, o) => {
        asked = a
        // The real wallet encrypts, and the recipient opens it; the vector's IV is fixed, so its ciphertext is what is sealed.
        void o
        const real = await sw.encrypt(a)
        const back = await rw.decrypt({ ciphertext: real.ciphertext, protocolID: a.protocolID, keyID: a.keyID, counterparty: sender.toPublicKey().toString() })
        assert.deepEqual(back.plaintext, Array.from(plaintext), `${e.name}: the recipient opens what the wallet encrypted`)
        return { ciphertext: Array.from(ciphertext) }
      },
    }
    // The vector's envelope names the parties' handles too: seal is held to it over the same document.
    const doc = parseJSON(new TextEncoder().encode(e.signedJson)) as JObject
    const content = await seal(sw, 'vectors.example.com', doc, brc78)
    assert.equal(new TextDecoder().decode(content), e.content, `${e.name}: content`)
    // sealEnvelope through the wallet: the BRC-78 message as Go builds it, and an envelope the recipient's checks take.
    const mine = await sealEnvelope(w, 'vectors.example.com', fromHex(e.from), fromHex(e.to), plaintext, Number(e.created), fromHex(e.brc78KeyId))
    assert.deepEqual(asked, { plaintext: Array.from(plaintext), protocolID: [2, 'message encryption'], keyID: Utils.toBase64(Array.from(fromHex(e.brc78KeyId))), counterparty: e.to })
    const c = checkContent({ office: e.office, to: fromHex(e.to), box: e.box, from: fromHex(e.from), created: Number(e.created), expires: Number(e.expires), content: mine, extra: [] })
    verifyContentSignature(c, fromHex(e.from))
    assert.equal(new TextDecoder().decode(mine).includes(Utils.toBase64(Array.from(brc78))), true, `${e.name}: the BRC-78 message is Go's`)
    const back = decodeEnvelope(fromHex(e.record))
    const record = envelopeRecord({ office: e.office, to: fromHex(e.to), box: e.box, from: fromHex(e.from), created: Number(e.created), expires: Number(e.expires), content, extra: back.extra })
    assert.equal(toHex(record), e.record, `${e.name}: record`)
    // The recipient's own wallet opens the vector's ciphertext.
    const opened = await rw.decrypt({ ciphertext: Array.from(ciphertext), protocolID: [2, 'message encryption'], keyID: Utils.toBase64(Array.from(fromHex(e.brc78KeyId))), counterparty: e.from })
    assert.deepEqual(opened.plaintext, Array.from(plaintext), `${e.name}: opens`)
  }
})

test('transaction-v1: every envelope and receipt carrier, byte for byte, minted through the wallet from the same tree', async () => {
  const v = vec<{ transactions: Array<{ name: string; rawTx: string; beef: string; verdict: string }> }>('transaction-v1.json')
  const cases = v.transactions.filter((t) => /^carrier-(envelope|receipt)/.test(t.name) && t.verdict === 'admit')
  assert.ok(cases.length >= 3)
  for (const t of cases) {
    const want = Transaction.fromHex(t.rawTx)
    const beef = Beef.fromBinary(Array.from(fromHex(t.beef)))
    const treeId = want.inputs[0]!.sourceTXID ?? want.inputs[0]!.sourceTransaction!.id('hex')
    const tree = beef.findAtomicTransaction(treeId)!
    const out = want.outputs[0]!.lockingScript.chunks
    const record = Uint8Array.from(out.find((c) => (c.data?.length ?? 0) > 100)!.data!)
    // A receipt is the recipient's; an envelope the sender's.
    const got = await envelopeCarrier(t.name === 'carrier-receipt' ? rw : sw, 'vectors.example.com', record, tree, want.inputs[0]!.sourceOutputIndex)
    assert.equal(got.toHex(), t.rawTx, t.name)
  }
})

test('payment-v1: the destinations the sender derives and the payment member as Go wrote them', async () => {
  const v = vec<{ derivationPrefix: string; derivationSuffixes: string[]; paymentAtomicBeef: string; recipientIdentityKey: string; payments: Array<{ name: string; verdict: string; plaintext: string; outputs: Array<{ outputIndex: number; derivationSuffix: string; satoshis: number; lockingScript: string }> }> }>('payment-v1.json')
  const tx = Transaction.fromAtomicBEEF(Array.from(fromHex(v.paymentAtomicBeef)))
  for (const [i, s] of v.derivationSuffixes.entries()) {
    const d = await paymentDestination(sw, 'vectors.example.com', fromHex(v.recipientIdentityKey), v.derivationPrefix, s)
    assert.equal(d.toHex(), tx.outputs[i]!.lockingScript.toHex(), `destination ${i}`)
  }
  const one = v.payments.find((p) => p.name === 'payment-one-of-two')!
  const o = one.outputs[0]!
  const member = paymentMember(fromHex(v.paymentAtomicBeef), v.derivationPrefix, o.derivationSuffix, o.satoshis, o.outputIndex)
  const doc = parseJSON(new TextEncoder().encode(one.plaintext)) as JObject
  assert.equal(new TextDecoder().decode(canonical(member)), new TextDecoder().decode(canonical(doc.get('payment')!)))
  // The plaintext as the sender encodes it is the one the recipient parses.
  const plain = encodePlaintext(new JObject().set('body', 'Paid for the report.').set('payment', member))
  assert.ok(parsePlaintext(plain))
  assert.throws(() => paymentMember(fromHex(v.paymentAtomicBeef), v.derivationPrefix, o.derivationSuffix, 0))
  assert.throws(() => encodePlaintext(new JObject().set('payment', 'not an object')))
})
