/**
 * The outpoint rows ls_bbox keeps for the host's life (spec sections 8.2
 * and 8.4), as an append-only journal. The engine's storage holds the
 * outputs a host still keeps; it cannot hold what the rows must outlive: a
 * dropped carrier, a dropped sweep, and which carrier won an outpoint.
 * Without them a carrier made later on a swept or spent funding output
 * would be a fresh txid and would be answered.
 *
 * One JSON object a line:
 *
 *   {"k":"c","op":"<txid>.<vout>","c":"<C>","txid":"<txid>","kind":"e","office":"...","to":"<hex>","at":<s>}
 *   {"k":"c",...,"kind":"r","by":"<hex>","acks":["<C>",...],"at":<s>}
 *   {"k":"d","op":"...","c":"<C>","w":true}
 *   {"k":"s","op":"...","s":"<sweep txid>","office":"...","h":<height>,"at":<s>}
 *
 * `c` lines are every carrier any office admitted, with what the winner rule
 * reads (an envelope's `to`, a receipt's `by` and `acks`); `d` lines are
 * drops, `w` when the dropped carrier was its outpoint's winner; `s` lines
 * are the outpoints admitted sweeps spent. Commitments are in hash byte
 * order and txids in display order, both lowercase hex; `at` is the host's
 * time of first sight in Unix seconds.
 */
import { closeSync, fdatasyncSync, mkdirSync, openSync, readFileSync, truncateSync, writeSync } from 'node:fs'
import { join } from 'node:path'

export interface CarrierLine {
  k: 'c'
  op: string
  c: string
  txid: string
  kind: 'e' | 'r'
  office: string
  to?: string
  by?: string
  acks?: string[]
  at: number
}

export interface DropLine {
  k: 'd'
  op: string
  c: string
  w: boolean
}

export interface SweepLine {
  k: 's'
  op: string
  s: string
  office: string
  h: number
  at: number
}

export type Line = CarrierLine | DropLine | SweepLine

/** Where the rows live. `append` is durable when it returns. */
export interface Journal {
  load(): Line[]
  append(l: Line): void
}

/** A journal in memory, for tests and for a host that accepts losing it. */
export class MemoryJournal implements Journal {
  readonly lines: Line[] = []
  load(): Line[] {
    return this.lines.map((l) => ({ ...l }))
  }
  append(l: Line): void {
    this.lines.push({ ...l })
  }
}

function isLine(v: unknown): v is Line {
  if (typeof v !== 'object' || v === null) return false
  const o = v as Record<string, unknown>
  const s = (k: string): boolean => typeof o[k] === 'string'
  const n = (k: string): boolean => typeof o[k] === 'number' && Number.isSafeInteger(o[k])
  switch (o['k']) {
    case 'c':
      return s('op') && s('c') && s('txid') && (o['kind'] === 'e' || o['kind'] === 'r') && s('office') && n('at')
    case 'd':
      return s('op') && s('c') && typeof o['w'] === 'boolean'
    case 's':
      return s('op') && s('s') && s('office') && n('h') && n('at')
  }
  return false
}

/**
 * The journal as a file, `outpoints.jsonl` in dir. Each line is written and
 * flushed to disk before append returns. A last line cut short by a crash
 * is cut off on load; any other line that does not parse stops the load,
 * because a row silently skipped could let a funding output pay twice.
 */
export class FileJournal implements Journal {
  readonly path: string
  private fd: number | undefined

  constructor(dir: string) {
    mkdirSync(dir, { recursive: true })
    this.path = join(dir, 'outpoints.jsonl')
  }

  load(): Line[] {
    let text: string
    try {
      text = readFileSync(this.path, 'utf8')
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code === 'ENOENT') return []
      throw e
    }
    if (text.length > 0 && !text.endsWith('\n')) {
      // A last line cut short by a crash was never acknowledged: cut it off,
      // so the next line starts on its own.
      const keep = text.lastIndexOf('\n') + 1
      truncateSync(this.path, Buffer.byteLength(text.slice(0, keep), 'utf8'))
      text = text.slice(0, keep)
    }
    const raw = text.split('\n')
    const out: Line[] = []
    for (const [i, l] of raw.entries()) {
      if (l === '') continue
      let v: unknown
      try {
        v = JSON.parse(l)
      } catch {
        throw new Error(`${this.path}: line ${i + 1} is not JSON`)
      }
      if (!isLine(v)) throw new Error(`${this.path}: line ${i + 1} is not an outpoint row`)
      out.push(v)
    }
    return out
  }

  append(l: Line): void {
    this.fd ??= openSync(this.path, 'a')
    writeSync(this.fd, JSON.stringify(l) + '\n')
    fdatasyncSync(this.fd)
  }

  close(): void {
    if (this.fd !== undefined) closeSync(this.fd)
    this.fd = undefined
  }
}
