package bytdb

// bits.go: Postgres's bit and bit varying types (N-053), riding on a
// string of '0' and '1' characters.
//
// One character per bit is wasteful (8x a packed form) and deliberate:
// the string's byte order is exactly Postgres's bit_cmp order — the
// first differing bit decides, and a string that is a prefix of another
// sorts first — so keys, comparison, hashing and DISTINCT need nothing
// new, and the text form is the value itself. A bit-heavy schema would
// want packing; nobody has asked for bit at all (N-053 is completeness).
//
//	TBit     bit(n): exactly n bits on every write (n from Column.MaxLen;
//	         a declared "bit" is bit(1))
//	TVarBit  bit varying(n) / varbit(n): at most n bits; varbit has no limit
//
// Text input is Postgres's bit_in: binary digits, or a leading 'x'/'X'
// for hexadecimal (four bits per digit) or 'b'/'B' for binary. The SQL
// literals B'0101' and X'1F' read the same way.

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rohanthewiz/serr"
)

// BitColumnLen is a bit column's n: Column.MaxLen, with a bare bit
// (MaxLen 0) as bit(1); 0 for an unlimited bit varying.
func BitColumnLen(c *Column) int {
	if c.Type == TBit && c.MaxLen <= 0 {
		return 1
	}
	return c.MaxLen
}

// ParseBits reads bit text ("0101", "x1F", "b101") to the one-character-
// per-bit form, as Postgres's bit_in does; a bad digit is Postgres's
// `"2" is not a valid binary digit` (22P02).
func ParseBits(s string) (string, error) {
	hex := false
	body := s
	if body != "" {
		switch body[0] {
		case 'x', 'X':
			hex, body = true, body[1:]
		case 'b', 'B':
			body = body[1:]
		}
	}
	if hex {
		return parseHexBits(body)
	}
	return parseBinaryBits(body)
}

func parseBinaryBits(s string) (string, error) {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' && s[i] != '1' {
			return "", serr.New(`"` + firstChar(s[i:]) + `" is not a valid binary digit`)
		}
	}
	return s, nil
}

func parseHexBits(s string) (string, error) {
	var b strings.Builder
	b.Grow(4 * len(s))
	for i := 0; i < len(s); i++ {
		v, ok := hexVal(s[i])
		if !ok {
			return "", serr.New(`"` + firstChar(s[i:]) + `" is not a valid hexadecimal digit`)
		}
		for k := 3; k >= 0; k-- {
			b.WriteByte('0' + byte(v>>k&1))
		}
	}
	return b.String(), nil
}

func hexVal(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// firstChar is s's first character (a whole UTF-8 sequence), as
// Postgres quotes the offending one.
func firstChar(s string) string {
	_, n := utf8.DecodeRuneInString(s)
	return s[:n]
}

// FitBits applies an explicit cast's length: bit(n) pads with zeros on
// the right or truncates to exactly n bits; bit varying(n) only
// truncates. n <= 0 means no limit (varbit).
func FitBits(bits string, n int, varying bool) string {
	switch {
	case n <= 0:
		return bits
	case len(bits) > n:
		return bits[:n]
	case len(bits) < n && !varying:
		return bits + strings.Repeat("0", n-len(bits))
	}
	return bits
}

// CheckBitsLen enforces a column's length on write (an assignment, not
// an explicit cast): bit(n) takes exactly n bits, bit varying(n) at
// most n — Postgres's 22026 otherwise.
func CheckBitsLen(bits string, n int, varying bool) error {
	switch {
	case n <= 0:
	case varying && len(bits) > n:
		return serr.New("bit string too long for type bit varying(" + strconv.Itoa(n) + ")")
	case !varying && len(bits) != n:
		return serr.New("bit string length " + strconv.Itoa(len(bits)) + " does not match type bit(" + strconv.Itoa(n) + ")")
	}
	return nil
}

// IntToBits is int::bit(n): the low n bits of the two's-complement
// value, sign-extended when n exceeds 64.
func IntToBits(v int64, n int) string {
	var b strings.Builder
	b.Grow(n)
	for i := n - 1; i >= 0; i-- {
		shift := min(i, 63) // past bit 63 every bit is the sign bit
		b.WriteByte('0' + byte(uint64(v)>>shift&1))
	}
	return b.String()
}

// BitsToInt is bit::int4 (width 32) or bit::int8 (width 64): the bits as
// an unsigned number, wrapped to the signed width, so 32 one-bits are
// -1 as an int4. More bits than the width is "integer out of range" or
// "bigint out of range".
func BitsToInt(bits string, width int) (int64, error) {
	if len(bits) > width {
		if width == 32 {
			return 0, serr.New("integer out of range")
		}
		return 0, serr.New("bigint out of range")
	}
	var u uint64
	for i := 0; i < len(bits); i++ {
		u = u<<1 | uint64(bits[i]-'0')
	}
	if width == 32 {
		return int64(int32(uint32(u))), nil
	}
	return int64(u), nil
}

// BitsOp is & | # on two bit strings, which must be the same length
// (Postgres's "cannot AND bit strings of different sizes", 22026).
func BitsOp(op string, a, b string) (string, error) {
	if len(a) != len(b) {
		name := map[string]string{"&": "AND", "|": "OR", "#": "XOR"}[op]
		return "", serr.New("cannot " + name + " bit strings of different sizes")
	}
	out := []byte(a)
	for i := range out {
		x, y := a[i] == '1', b[i] == '1'
		var r bool
		switch op {
		case "&":
			r = x && y
		case "|":
			r = x || y
		default: // "#"
			r = x != y
		}
		out[i] = '0'
		if r {
			out[i] = '1'
		}
	}
	return string(out), nil
}

// BitsNot is ~bits.
func BitsNot(a string) string {
	out := []byte(a)
	for i := range out {
		out[i] ^= 1 // '0' <-> '1'
	}
	return string(out)
}

// BitsShift is bits << n (left) or bits >> n: the length is kept, zeros
// shift in, and a negative count shifts the other way.
func BitsShift(a string, n int64, left bool) string {
	if n < 0 {
		left, n = !left, -n
	}
	l := int64(len(a))
	if n >= l {
		return strings.Repeat("0", len(a))
	}
	if left {
		return a[n:] + strings.Repeat("0", int(n))
	}
	return strings.Repeat("0", int(n)) + a[:l-n]
}

// BitsAppendBinary appends the wire's binary bit form (bit_send): the
// length in bits as an int32, then the bits packed eight to a byte,
// most significant first, the last byte padded with zero bits.
func BitsAppendBinary(dst []byte, bits string) []byte {
	n := len(bits)
	dst = append(dst, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	for i := 0; i < n; i += 8 {
		var c byte
		for k := 0; k < 8; k++ {
			c <<= 1
			if i+k < n && bits[i+k] == '1' {
				c |= 1
			}
		}
		dst = append(dst, c)
	}
	return dst
}

// BitsFromBinary reads bit_recv's form back.
func BitsFromBinary(raw []byte) (string, error) {
	if len(raw) < 4 {
		return "", serr.New("bad binary bit parameter")
	}
	n := int(int32(uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3])))
	if n < 0 || len(raw)-4 != (n+7)/8 {
		return "", serr.New("bad binary bit parameter")
	}
	var b strings.Builder
	b.Grow(n)
	for i := 0; i < n; i++ {
		b.WriteByte('0' + raw[4+i/8]>>(7-i%8)&1)
	}
	return b.String(), nil
}
