/**
 * The JSON subset an envelope's content (and the recipient's plaintext) is
 * written in, spec section 4.1: I-JSON (RFC 7493) with integers only,
 * nesting at most 16, serialized exactly as RFC 8785 (JCS) serializes it.
 * The twin of the Go internal/boxrec/jcs.go, step for step: its own parser
 * rather than JSON.parse, which takes the last of two equal member names and
 * cannot report the duplicate the subset refuses.
 */

const maxDepth = 16

/** A JSON value in the subset. Integers are safe integers. */
export type JValue = JObject | JValue[] | string | number | boolean | null

/** A JSON object whose member names are unique, in parse or insertion order. */
export class JObject {
  constructor(readonly members: Array<{ name: string; value: JValue }> = []) {}

  get(name: string): JValue | undefined {
    return this.members.find((m) => m.name === name)?.value
  }

  has(name: string): boolean {
    return this.members.some((m) => m.name === name)
  }

  set(name: string, value: JValue): this {
    const m = this.members.find((x) => x.name === name)
    if (m !== undefined) m.value = value
    else this.members.push({ name, value })
    return this
  }

  /** A shallow copy without the named members. */
  without(...names: string[]): JObject {
    return new JObject(this.members.filter((m) => !names.includes(m.name)))
  }
}

export class JSONSubsetError extends Error {
  constructor() {
    super('not in the JSON subset')
    this.name = 'JSONSubsetError'
  }
}

const utf8 = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true })
const hex4 = /^[0-9a-fA-F]{4}$/

class Parser {
  pos = 0
  constructor(readonly s: string) {}

  ws(): void {
    while (this.pos < this.s.length) {
      const c = this.s[this.pos]
      if (c !== ' ' && c !== '\t' && c !== '\n' && c !== '\r') return
      this.pos++
    }
  }

  lit(t: string): boolean {
    if (this.s.startsWith(t, this.pos)) {
      this.pos += t.length
      return true
    }
    return false
  }

  value(depth: number): JValue {
    if (depth > maxDepth || this.pos >= this.s.length) throw new JSONSubsetError()
    const c = this.s[this.pos]!
    if (c === '{') return this.object(depth + 1)
    if (c === '[') return this.array(depth + 1)
    if (c === '"') return this.str()
    if (c === '-' || (c >= '0' && c <= '9')) return this.number()
    if (this.lit('true')) return true
    if (this.lit('false')) return false
    if (this.lit('null')) return null
    throw new JSONSubsetError()
  }

  object(depth: number): JObject {
    if (depth > maxDepth) throw new JSONSubsetError()
    this.pos++
    const o = new JObject()
    const seen = new Set<string>()
    this.ws()
    if (this.lit('}')) return o
    for (;;) {
      this.ws()
      if (this.s[this.pos] !== '"') throw new JSONSubsetError()
      const k = this.str()
      if (seen.has(k)) throw new JSONSubsetError()
      seen.add(k)
      this.ws()
      if (!this.lit(':')) throw new JSONSubsetError()
      this.ws()
      o.members.push({ name: k, value: this.value(depth) })
      this.ws()
      if (this.lit('}')) return o
      if (!this.lit(',')) throw new JSONSubsetError()
    }
  }

  array(depth: number): JValue[] {
    if (depth > maxDepth) throw new JSONSubsetError()
    this.pos++
    const out: JValue[] = []
    this.ws()
    if (this.lit(']')) return out
    for (;;) {
      this.ws()
      out.push(this.value(depth))
      this.ws()
      if (this.lit(']')) return out
      if (!this.lit(',')) throw new JSONSubsetError()
    }
  }

  hex4(at: number): number | undefined {
    const h = this.s.slice(at, at + 4)
    return hex4.test(h) ? parseInt(h, 16) : undefined
  }

  str(): string {
    this.pos++
    let out = ''
    while (this.pos < this.s.length) {
      const c = this.s[this.pos]!
      if (c === '"') {
        this.pos++
        return out
      }
      if (c.charCodeAt(0) < 0x20) throw new JSONSubsetError()
      if (c !== '\\') {
        out += c
        this.pos++
        continue
      }
      if (this.pos + 1 >= this.s.length) throw new JSONSubsetError()
      const e = this.s[this.pos + 1]!
      this.pos += 2
      switch (e) {
        case '"':
        case '\\':
        case '/':
          out += e
          break
        case 'b':
          out += '\b'
          break
        case 'f':
          out += '\f'
          break
        case 'n':
          out += '\n'
          break
        case 'r':
          out += '\r'
          break
        case 't':
          out += '\t'
          break
        case 'u': {
          const r = this.hex4(this.pos)
          if (r === undefined) throw new JSONSubsetError()
          this.pos += 4
          if (r >= 0xd800 && r <= 0xdfff) {
            if (r >= 0xdc00 || !this.s.startsWith('\\u', this.pos)) throw new JSONSubsetError()
            const r2 = this.hex4(this.pos + 2)
            if (r2 === undefined || r2 < 0xdc00 || r2 > 0xdfff) throw new JSONSubsetError()
            this.pos += 6
            out += String.fromCharCode(r, r2)
          } else {
            out += String.fromCharCode(r)
          }
          break
        }
        default:
          throw new JSONSubsetError()
      }
    }
    throw new JSONSubsetError()
  }

  number(): number {
    const start = this.pos
    const neg = this.lit('-')
    const digits = this.pos
    while (this.pos < this.s.length && this.s[this.pos]! >= '0' && this.s[this.pos]! <= '9') this.pos++
    const d = this.s.slice(digits, this.pos)
    if (d.length === 0 || (d.length > 1 && d[0] === '0') || (neg && d === '0')) throw new JSONSubsetError()
    const next = this.s[this.pos]
    if (next === '.' || next === 'e' || next === 'E') throw new JSONSubsetError()
    const n = BigInt(this.s.slice(start, this.pos))
    if (n > BigInt(Number.MAX_SAFE_INTEGER) || n < -BigInt(Number.MAX_SAFE_INTEGER)) throw new JSONSubsetError()
    return Number(n)
  }
}

/**
 * Parses b as one value of the subset: valid UTF-8, no byte order mark,
 * objects with unique member names, strings with no lone surrogate,
 * integers only (no fraction, exponent, leading zero or minus zero,
 * magnitude at most 2^53 - 1), nesting at most 16, and nothing but white
 * space after the value.
 */
export function parseJSON(b: Uint8Array): JValue {
  let s: string
  try {
    s = utf8.decode(b)
  } catch {
    throw new JSONSubsetError()
  }
  const p = new Parser(s)
  p.ws()
  const v = p.value(0)
  p.ws()
  if (p.pos !== s.length) throw new JSONSubsetError()
  return v
}

const loneSurrogate = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/

function canonString(s: string): string {
  if (loneSurrogate.test(s)) throw new JSONSubsetError()
  let out = '"'
  for (const ch of s) {
    const c = ch.charCodeAt(0)
    if (ch === '"') out += '\\"'
    else if (ch === '\\') out += '\\\\'
    else if (ch === '\b') out += '\\b'
    else if (ch === '\f') out += '\\f'
    else if (ch === '\n') out += '\\n'
    else if (ch === '\r') out += '\\r'
    else if (ch === '\t') out += '\\t'
    else if (c < 0x20) out += '\\u00' + c.toString(16).padStart(2, '0')
    else out += ch
  }
  return out + '"'
}

function canon(v: JValue): string {
  if (v === null) return 'null'
  if (v === true) return 'true'
  if (v === false) return 'false'
  if (typeof v === 'number') {
    if (!Number.isSafeInteger(v)) throw new JSONSubsetError()
    return String(v)
  }
  if (typeof v === 'string') return canonString(v)
  if (Array.isArray(v)) return '[' + v.map(canon).join(',') + ']'
  // Member names sort by their UTF-16 code units, which is how JavaScript
  // compares strings.
  const ms = [...v.members].sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0))
  for (let i = 1; i < ms.length; i++) if (ms[i - 1]!.name === ms[i]!.name) throw new JSONSubsetError()
  return '{' + ms.map((m) => canonString(m.name) + ':' + canon(m.value)).join(',') + '}'
}

const enc = new TextEncoder()

/**
 * v as RFC 8785 serializes it: no white space, object members sorted by the
 * UTF-16 code units of their names, strings escaped as JSON.stringify
 * escapes them, integers in plain decimal.
 */
export function canonical(v: JValue): Uint8Array {
  return enc.encode(canon(v))
}
