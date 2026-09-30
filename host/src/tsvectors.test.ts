import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'
import { file, generate } from './tsvectors.js'

test('the TypeScript-written vectors are what the codec writes, byte for byte', async () => {
  assert.equal(readFileSync(file, 'utf8'), await generate(), 'run: node dist/tsvectors.js')
  assert.equal(await generate(), await generate())
  assert.deepEqual(readdirSync(new URL('.', file)), ['ts-v1.json'])
})

test('the TypeScript side admits its own carrier and accepts its own payment', async () => {
  const { ProtoWallet, PrivateKey } = await import('@bsv/sdk')
  const { admit } = await import('./admit.js')
  const { decodeEnvelope } = await import('./boxrec.js')
  const { checkContent } = await import('./content.js')
  const { checkPayment, open, parsePlaintext } = await import('./payment.js')
  const { fromHex, toHex } = await import('./util.js')
  const v = JSON.parse(readFileSync(file, 'utf8')) as {
    office: string
    headers: Array<{ height: number; merkleRoot: string }>
    carrier: { commitment: string; beef: string }
    envelopes: Array<{ record: string }>
    payment: { now: number; paymentTxid: string }
  }
  const roots = new Map(v.headers.map((h) => [h.height, h.merkleRoot]))
  const headers = { isValidRootForHeight: async (r: string, h: number) => roots.get(h) === r, currentHeight: async () => 0 }
  const a = await admit(fromHex(v.carrier.beef), [], { office: v.office, headers })
  assert.equal(a.kind, 'envelope')
  assert.equal(toHex(a.carrier!.commitment), v.carrier.commitment)
  const w = new ProtoWallet(PrivateKey.fromHex('43'.repeat(32)))
  const e = decodeEnvelope(fromHex(v.envelopes[1]!.record))
  const pt = parsePlaintext(await open(w, checkContent(e), e.from))
  const tx = await checkPayment(w, e, pt.payment!, headers, v.payment.now)
  assert.equal(tx.id('hex'), v.payment.paymentTxid)
})
