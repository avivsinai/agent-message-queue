// Package jcs writes the RFC 8785 JSON Canonicalization Scheme form of a JSON
// text. A signer and a verifier that canonicalize the same value get the same
// bytes, so a signature or a digest over those bytes means one thing on every
// side: the browser, the server and this machine.
//
// The number profile is integers only. The documents that carry signatures
// and digests (consent, fingerprints, request snapshots) hold no fractions,
// and ES6 number formatting is where JCS implementations disagree, so a
// fraction, an exponent or an integer outside +-(2^53-1) is refused rather
// than formatted.
//
// Input is parsed strictly: duplicate object keys, a lone UTF-16 surrogate
// (escaped or as invalid UTF-8) and trailing data are refused. The parser is
// our own because encoding/json keeps the last duplicate key and replaces a
// lone surrogate with U+FFFD, so two different texts would canonicalize to
// the same bytes.
package jcs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// maxDepth bounds nesting so a hostile document cannot exhaust the stack.
const maxDepth = 64

// maxSafeInteger is 2^53-1, the largest integer every JSON implementation
// that reads numbers as IEEE 754 doubles holds exactly.
const maxSafeInteger = 1<<53 - 1

// ErrInvalid wraps every refusal, so callers can test for it with errors.Is.
var ErrInvalid = errors.New("jcs: invalid input")

// Canonicalize returns the canonical form of one JSON text.
func Canonicalize(data []byte) ([]byte, error) {
	p := parser{data: data}
	p.skipSpace()
	var out bytes.Buffer
	if err := p.value(&out, 0); err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.data) {
		return nil, p.fail("trailing data")
	}
	return out.Bytes(), nil
}

// Marshal encodes v with encoding/json and returns its canonical form.
func Marshal(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return Canonicalize(raw)
}

type parser struct {
	data []byte
	pos  int
}

func (p *parser) fail(format string, args ...any) error {
	return fmt.Errorf("%w: %s at byte %d", ErrInvalid, fmt.Sprintf(format, args...), p.pos)
}

func (p *parser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) value(out *bytes.Buffer, depth int) error {
	if depth > maxDepth {
		return p.fail("nesting deeper than %d", maxDepth)
	}
	if p.pos >= len(p.data) {
		return p.fail("unexpected end of input")
	}
	switch c := p.data[p.pos]; {
	case c == '{':
		return p.object(out, depth)
	case c == '[':
		return p.array(out, depth)
	case c == '"':
		s, err := p.str()
		if err != nil {
			return err
		}
		writeString(out, s)
		return nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number(out)
	default:
		for _, lit := range []string{"true", "false", "null"} {
			if bytes.HasPrefix(p.data[p.pos:], []byte(lit)) {
				p.pos += len(lit)
				out.WriteString(lit)
				return nil
			}
		}
		return p.fail("unexpected character %q", c)
	}
}

type member struct {
	key   string
	value []byte
}

func (p *parser) object(out *bytes.Buffer, depth int) error {
	p.pos++ // {
	var members []member
	seen := map[string]bool{}
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		out.WriteString("{}")
		return nil
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return p.fail("expected an object key")
		}
		key, err := p.str()
		if err != nil {
			return err
		}
		if seen[key] {
			return p.fail("duplicate key %q", key)
		}
		seen[key] = true
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return p.fail("expected ':'")
		}
		p.pos++
		p.skipSpace()
		var v bytes.Buffer
		if err := p.value(&v, depth+1); err != nil {
			return err
		}
		members = append(members, member{key: key, value: v.Bytes()})
		p.skipSpace()
		if p.pos >= len(p.data) {
			return p.fail("unterminated object")
		}
		if p.data[p.pos] == '}' {
			p.pos++
			break
		}
		if p.data[p.pos] != ',' {
			return p.fail("expected ',' or '}'")
		}
		p.pos++
	}
	// RFC 8785 3.2.3: sort by the keys' UTF-16 code units.
	slices.SortFunc(members, func(a, b member) int {
		return slices.Compare(utf16.Encode([]rune(a.key)), utf16.Encode([]rune(b.key)))
	})
	out.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			out.WriteByte(',')
		}
		writeString(out, m.key)
		out.WriteByte(':')
		out.Write(m.value)
	}
	out.WriteByte('}')
	return nil
}

func (p *parser) array(out *bytes.Buffer, depth int) error {
	p.pos++ // [
	out.WriteByte('[')
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		out.WriteByte(']')
		return nil
	}
	for i := 0; ; i++ {
		if i > 0 {
			out.WriteByte(',')
		}
		p.skipSpace()
		if err := p.value(out, depth+1); err != nil {
			return err
		}
		p.skipSpace()
		if p.pos >= len(p.data) {
			return p.fail("unterminated array")
		}
		if p.data[p.pos] == ']' {
			p.pos++
			out.WriteByte(']')
			return nil
		}
		if p.data[p.pos] != ',' {
			return p.fail("expected ',' or ']'")
		}
		p.pos++
	}
}

// number accepts only the integer profile: an optional minus and digits with
// no leading zero, within +-(2^53-1). -0 canonicalizes to 0, as ES6 does.
func (p *parser) number(out *bytes.Buffer) error {
	start := p.pos
	if p.data[p.pos] == '-' {
		p.pos++
	}
	digits := p.pos
	for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
		p.pos++
	}
	if p.pos == digits {
		return p.fail("expected a digit")
	}
	if p.data[digits] == '0' && p.pos-digits > 1 {
		return p.fail("leading zero")
	}
	if p.pos < len(p.data) && (p.data[p.pos] == '.' || p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		return p.fail("only integers are allowed")
	}
	n, err := strconv.ParseInt(string(p.data[start:p.pos]), 10, 64)
	if err != nil || n > maxSafeInteger || n < -maxSafeInteger {
		return p.fail("integer outside +-(2^53-1)")
	}
	out.WriteString(strconv.FormatInt(n, 10))
	return nil
}

// str reads one string literal and returns its value. Invalid UTF-8 and lone
// surrogates are refused: they have no well-defined UTF-16 form to sort by.
func (p *parser) str() (string, error) {
	p.pos++ // opening quote
	var b []rune
	for {
		if p.pos >= len(p.data) {
			return "", p.fail("unterminated string")
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			return string(b), nil
		case c < 0x20:
			return "", p.fail("raw control character in string")
		case c == '\\':
			r, err := p.escape()
			if err != nil {
				return "", err
			}
			b = append(b, r)
		default:
			r, size := utf8.DecodeRune(p.data[p.pos:])
			if r == utf8.RuneError && size <= 1 {
				return "", p.fail("invalid UTF-8")
			}
			b = append(b, r)
			p.pos += size
		}
	}
}

func (p *parser) escape() (rune, error) {
	if p.pos+1 >= len(p.data) {
		return 0, p.fail("unterminated escape")
	}
	c := p.data[p.pos+1]
	p.pos += 2
	switch c {
	case '"', '\\', '/':
		return rune(c), nil
	case 'b':
		return '\b', nil
	case 'f':
		return '\f', nil
	case 'n':
		return '\n', nil
	case 'r':
		return '\r', nil
	case 't':
		return '\t', nil
	case 'u':
		r, err := p.hex4()
		if err != nil {
			return 0, err
		}
		if !utf16.IsSurrogate(r) {
			return r, nil
		}
		if r >= 0xDC00 || !bytes.HasPrefix(p.data[p.pos:], []byte(`\u`)) {
			return 0, p.fail("lone surrogate")
		}
		p.pos += 2
		low, err := p.hex4()
		if err != nil {
			return 0, err
		}
		if low < 0xDC00 || low > 0xDFFF {
			return 0, p.fail("lone surrogate")
		}
		return utf16.DecodeRune(r, low), nil
	default:
		return 0, p.fail("invalid escape \\%c", c)
	}
}

func (p *parser) hex4() (rune, error) {
	if p.pos+4 > len(p.data) {
		return 0, p.fail("short \\u escape")
	}
	n, err := strconv.ParseUint(string(p.data[p.pos:p.pos+4]), 16, 32)
	if err != nil {
		return 0, p.fail("invalid \\u escape")
	}
	p.pos += 4
	return rune(n), nil
}

// writeString writes s the way ES6 JSON.stringify does (RFC 8785 3.2.2.2):
// only '"', '\\' and the C0 controls are escaped; '<', '&', U+2028 and every
// non-ASCII character are written as they are.
func writeString(out *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	out.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte(hex[r>>4])
				out.WriteByte(hex[r&0xF])
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}
