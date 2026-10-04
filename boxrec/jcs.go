package boxrec

import (
	"bytes"
	"errors"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// The JSON subset an envelope's content is written in (docs/spec.md section
// 4.1): I-JSON (RFC 7493) with integers only, nesting at most maxJSONDepth,
// serialized exactly as RFC 8785 (JCS) serializes it. With integers only, the
// JCS number form is the plain decimal, so no floating-point formatting is
// needed and Go and JavaScript reach the same bytes.

const maxJSONDepth = 16

// maxJSONInt is 2^53 - 1: every integer in the subset is exact as an IEEE 754
// double, which is how a JavaScript host parses it.
const maxJSONInt = MaxSafe

// Object is a JSON object whose member names are unique. Members keep the
// order they were parsed or added in; serialization sorts them.
type Object struct {
	Members []Member
}

// Member is one name and value.
type Member struct {
	Name  string
	Value any // *Object, []any, string, Int, bool, or nil
}

// Int is an integer in the subset, from -(2^53 - 1) to 2^53 - 1.
type Int int64

// Get returns a member's value and whether it is present.
func (o *Object) Get(name string) (any, bool) {
	for _, m := range o.Members {
		if m.Name == name {
			return m.Value, true
		}
	}
	return nil, false
}

// Set replaces a member's value, or appends the member.
func (o *Object) Set(name string, v any) {
	for i := range o.Members {
		if o.Members[i].Name == name {
			o.Members[i].Value = v
			return
		}
	}
	o.Members = append(o.Members, Member{Name: name, Value: v})
}

// Without returns a shallow copy without the named members.
func (o *Object) Without(names ...string) *Object {
	out := &Object{}
	for _, m := range o.Members {
		skip := false
		for _, n := range names {
			if m.Name == n {
				skip = true
			}
		}
		if !skip {
			out.Members = append(out.Members, m)
		}
	}
	return out
}

var errJSON = errors.New("not in the JSON subset")

type jparser struct {
	b   []byte
	pos int
}

// ParseJSON parses b as one value of the subset: valid UTF-8, no byte order
// mark, objects with unique member names, strings with no lone surrogate,
// integers only (no fraction, exponent, leading zero or minus zero, magnitude
// at most 2^53 - 1), nesting at most 16, and nothing but white space after
// the value.
func ParseJSON(b []byte) (any, error) {
	if !utf8.Valid(b) {
		return nil, errJSON
	}
	p := &jparser{b: b}
	p.ws()
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.pos != len(p.b) {
		return nil, errJSON
	}
	return v, nil
}

func (p *jparser) ws() {
	for p.pos < len(p.b) {
		switch p.b[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *jparser) lit(s string) bool {
	if bytes.HasPrefix(p.b[p.pos:], []byte(s)) {
		p.pos += len(s)
		return true
	}
	return false
}

func (p *jparser) value(depth int) (any, error) {
	if depth > maxJSONDepth || p.pos >= len(p.b) {
		return nil, errJSON
	}
	switch c := p.b[p.pos]; {
	case c == '{':
		return p.object(depth + 1)
	case c == '[':
		return p.array(depth + 1)
	case c == '"':
		return p.str()
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	case p.lit("true"):
		return true, nil
	case p.lit("false"):
		return false, nil
	case p.lit("null"):
		return nil, nil
	}
	return nil, errJSON
}

func (p *jparser) object(depth int) (any, error) {
	if depth > maxJSONDepth {
		return nil, errJSON
	}
	p.pos++ // {
	o := &Object{}
	seen := map[string]bool{}
	p.ws()
	if p.lit("}") {
		return o, nil
	}
	for {
		p.ws()
		if p.pos >= len(p.b) || p.b[p.pos] != '"' {
			return nil, errJSON
		}
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		if seen[k] {
			return nil, errJSON
		}
		seen[k] = true
		p.ws()
		if !p.lit(":") {
			return nil, errJSON
		}
		p.ws()
		v, err := p.value(depth)
		if err != nil {
			return nil, err
		}
		o.Members = append(o.Members, Member{Name: k, Value: v})
		p.ws()
		if p.lit("}") {
			return o, nil
		}
		if !p.lit(",") {
			return nil, errJSON
		}
	}
}

func (p *jparser) array(depth int) (any, error) {
	if depth > maxJSONDepth {
		return nil, errJSON
	}
	p.pos++ // [
	out := []any{}
	p.ws()
	if p.lit("]") {
		return out, nil
	}
	for {
		p.ws()
		v, err := p.value(depth)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.ws()
		if p.lit("]") {
			return out, nil
		}
		if !p.lit(",") {
			return nil, errJSON
		}
	}
}

func hex4(b []byte) (rune, bool) {
	if len(b) < 4 {
		return 0, false
	}
	n, err := strconv.ParseUint(string(b[:4]), 16, 16)
	return rune(n), err == nil
}

func (p *jparser) str() (string, error) {
	p.pos++ // "
	var out []byte
	for p.pos < len(p.b) {
		c := p.b[p.pos]
		switch {
		case c == '"':
			p.pos++
			return string(out), nil
		case c < 0x20:
			return "", errJSON
		case c == '\\':
			if p.pos+1 >= len(p.b) {
				return "", errJSON
			}
			e := p.b[p.pos+1]
			p.pos += 2
			switch e {
			case '"', '\\', '/':
				out = append(out, e)
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				r, ok := hex4(p.b[p.pos:])
				if !ok {
					return "", errJSON
				}
				p.pos += 4
				if utf16.IsSurrogate(r) {
					if r >= 0xdc00 || !bytes.HasPrefix(p.b[p.pos:], []byte(`\u`)) {
						return "", errJSON
					}
					r2, ok := hex4(p.b[p.pos+2:])
					if !ok || r2 < 0xdc00 || r2 > 0xdfff {
						return "", errJSON
					}
					p.pos += 6
					r = utf16.DecodeRune(r, r2)
				}
				out = utf8.AppendRune(out, r)
			default:
				return "", errJSON
			}
		default:
			out = append(out, c)
			p.pos++
		}
	}
	return "", errJSON
}

func (p *jparser) number() (any, error) {
	start := p.pos
	neg := p.lit("-")
	digits := p.pos
	for p.pos < len(p.b) && p.b[p.pos] >= '0' && p.b[p.pos] <= '9' {
		p.pos++
	}
	d := p.b[digits:p.pos]
	if len(d) == 0 || (len(d) > 1 && d[0] == '0') || (neg && string(d) == "0") {
		return nil, errJSON
	}
	if p.pos < len(p.b) {
		if c := p.b[p.pos]; c == '.' || c == 'e' || c == 'E' {
			return nil, errJSON
		}
	}
	n, err := strconv.ParseInt(string(p.b[start:p.pos]), 10, 64)
	if err != nil || n > maxJSONInt || n < -maxJSONInt {
		return nil, errJSON
	}
	return Int(n), nil
}

// Canonical writes v as RFC 8785 serializes it: no white space, object
// members sorted by the UTF-16 code units of their names, strings escaped as
// ECMAScript's JSON.stringify escapes them, integers in plain decimal.
func Canonical(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := canon(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func canon(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case Int:
		if x > maxJSONInt || x < -maxJSONInt {
			return errJSON
		}
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case string:
		if !utf8.ValidString(x) {
			return errJSON
		}
		canonString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := canon(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case *Object:
		ms := append([]Member(nil), x.Members...)
		sort.SliceStable(ms, func(i, j int) bool { return utf16Less(ms[i].Name, ms[j].Name) })
		b.WriteByte('{')
		for i, m := range ms {
			if i > 0 {
				if ms[i-1].Name == m.Name {
					return errJSON
				}
				b.WriteByte(',')
			}
			canonString(b, m.Name)
			b.WriteByte(':')
			if err := canon(b, m.Value); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return errJSON
	}
	return nil
}

func utf16Less(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

func canonString(b *bytes.Buffer, s string) {
	const hexd = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hexd[c>>4])
				b.WriteByte(hexd[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
}
