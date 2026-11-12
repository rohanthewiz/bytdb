// Package tuple implements an order-preserving binary encoding for
// composite keys: for any two tuples, bytes.Compare on their encodings
// equals Compare on the tuples themselves. This is the property that
// lets a relational layer store rows and index entries in an ordered
// key-value store and answer range queries with key scans.
//
// Supported element types (after normalization): nil (NULL), bool,
// int64, float64, string, []byte, decimal.Decimal. Inputs of any Go integer width are
// normalized to int64, float32 to float64. Every NaN normalizes to one
// canonical NaN, which sorts above +Inf (Postgres's order: NaN is
// greater than every number and equal to itself). Negative zero
// normalizes to positive zero in keys (Append), so equal values encode
// identically; AppendValue keeps it, for stored values whose order does
// not matter but whose sign Postgres preserves.
//
// NaN was refused outright until deep-look D-36, so no existing key or
// value holds one; adding it extends the encoding without changing any
// byte an earlier version wrote.
//
// Elements of different types order by type tag: NULL < false < true <
// ints < floats < bytes < strings < decimals. Within a typed column only one tag
// ever appears, so cross-type order only matters for NULL, which sorts
// before everything. A tuple that is a prefix of another sorts first,
// matching byte order of the encodings.
package tuple

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"github.com/rohanthewiz/bytdb/decimal"
	"github.com/rohanthewiz/serr"
)

// Type tags. Never renumber: encodings are persistent.
const (
	tagNull   byte = 0x01
	tagFalse  byte = 0x02
	tagTrue   byte = 0x03
	tagInt    byte = 0x04
	tagFloat  byte = 0x05
	tagBytes  byte = 0x06
	tagString byte = 0x07
	// Decimals (N-048) have two forms sharing one body, the way float
	// keys collapse -0 while stored floats keep it. A key (Append)
	// encodes the value alone, so 1.0 and 1.00 encode identically and
	// a unique index treats them as the duplicates Postgres says they
	// are. A stored value (AppendValue) appends the display scale, so
	// the row reads back as written. Both decode to decimal.Decimal;
	// both rank as tagDecimal.
	tagDecimal       byte = 0x08
	tagDecimalScaled byte = 0x09
)

// Decimal body, after the tag:
//
//	class     1 byte   0x01 negative, 0x02 zero, 0x03 positive
//	exponent  4 bytes  int32 E, sign bit flipped        (absent for zero)
//	digits    n bytes  1 + each base-100 digit pair     (absent for zero)
//	end       1 byte   0x00                             (absent for zero)
//
// where |v| = 0.d1d2...dk × 10^E with no leading or trailing zero
// digits (decimal.Mantissa); an odd final digit is padded with 0 in its
// pair, which cannot collide with a longer mantissa because mantissas
// never end in 0. For a negative value, exponent, digit and end bytes
// are all inverted, so a larger magnitude sorts lower and a shorter
// mantissa (smaller magnitude) higher. Every part is fixed-width or
// terminated, so whole-element inversion (AppendDesc) still reverses the
// order. tagDecimalScaled then adds the display scale as a big-endian
// uint16.
const (
	decNeg  byte = 0x01
	decZero byte = 0x02
	decPos  byte = 0x03
	decEnd  byte = 0x00
)

// Byte-string escaping: 0x00 inside the data becomes 0x00 0xFF, and
// 0x00 0x01 terminates the element. The terminator sorts below any
// escaped or literal continuation byte, so "a" < "a\x00" < "a\x01" and
// "a" < "ab" hold in byte order.
const (
	escByte    byte = 0x00
	escaped00  byte = 0xFF
	terminator byte = 0x01
)

// Encode encodes vals as one order-preserving byte string.
func Encode(vals ...any) ([]byte, error) {
	return Append(nil, vals...)
}

// Append appends the encoding of vals to buf and returns the result.
func Append(buf []byte, vals ...any) ([]byte, error) {
	return appendVals(buf, 0x00, vals, false)
}

// AppendValue is Append for a stored value rather than a key: the one
// difference is that a float's negative zero keeps its sign. Keys must
// collapse it (0 = -0, so both must encode alike), but a value is only
// ever decoded, and Postgres preserves -0 in a float8 column; collapsing
// it made the stored row disagree with the row InsertReturning reported
// (deep-look D-36).
func AppendValue(buf []byte, vals ...any) ([]byte, error) {
	return appendVals(buf, 0x00, vals, true)
}

// AppendDesc appends vals encoded for descending order: each element's
// ascending encoding, inverted byte-for-byte. Element encodings are
// prefix-free (fixed width, or escape-terminated), so whole-element
// inversion exactly reverses byte order and stays self-delimiting —
// ascending and descending elements mix freely within one key.
// Descending elements decode with DecodeOne(data, true); note that a
// descending NULL sorts after every value, the mirror of ascending.
func AppendDesc(buf []byte, vals ...any) ([]byte, error) {
	return appendVals(buf, 0xFF, vals, false)
}

// appendVals encodes vals with every byte XORed by mask (0x00 keeps
// ascending order; 0xFF reverses it). keepNegZero is AppendValue's
// mode.
func appendVals(buf []byte, mask byte, vals []any, keepNegZero bool) ([]byte, error) {
	for _, v := range vals {
		if f, ok := v.(float64); ok && keepNegZero && f == 0 {
			buf = appendValue(buf, mask, f) // ±0 as given
			continue
		}
		if d, ok := v.(decimal.Decimal); ok {
			// keepNegZero is AppendValue's mode: a stored value keeps
			// its display scale, a key does not (see tagDecimal).
			buf = appendDecimal(buf, mask, d, keepNegZero)
			continue
		}
		n, err := normalize(v)
		if err != nil {
			return nil, err
		}
		buf = appendValue(buf, mask, n)
	}
	return buf, nil
}

func appendValue(buf []byte, mask byte, v any) []byte {
	u64 := func(buf []byte, u uint64) []byte {
		if mask != 0 {
			u = ^u
		}
		return binary.BigEndian.AppendUint64(buf, u)
	}
	switch t := v.(type) {
	case nil:
		return append(buf, tagNull^mask)
	case bool:
		if t {
			return append(buf, tagTrue^mask)
		}
		return append(buf, tagFalse^mask)
	case int64:
		buf = append(buf, tagInt^mask)
		return u64(buf, uint64(t)^(1<<63))
	case float64:
		bits := math.Float64bits(t)
		if bits&(1<<63) != 0 {
			bits = ^bits // negative: flip everything so more-negative sorts lower
		} else {
			bits |= 1 << 63 // positive: set the top bit to sort above negatives
		}
		buf = append(buf, tagFloat^mask)
		return u64(buf, bits)
	case []byte:
		return appendEscaped(append(buf, tagBytes^mask), mask, t)
	case string:
		return appendEscaped(append(buf, tagString^mask), mask, []byte(t))
	}
	panic(fmt.Sprintf("tuple: unreachable type %T after normalize", v))
}

// appendDecimal appends one decimal element (see the body layout at
// tagDecimal); withScale selects the stored-value form.
func appendDecimal(buf []byte, mask byte, d decimal.Decimal, withScale bool) []byte {
	tag := tagDecimal
	if withScale {
		tag = tagDecimalScaled
	}
	buf = append(buf, tag^mask)
	neg, digits, exp := d.Mantissa()
	switch {
	case digits == "":
		buf = append(buf, decZero^mask)
	default:
		// inv flips the magnitude bytes of a negative value so that its
		// order reverses; it composes with mask (descending keys).
		inv := mask
		cls := decPos
		if neg {
			inv ^= 0xFF
			cls = decNeg
		}
		buf = append(buf, cls^mask)
		var eb [4]byte
		binary.BigEndian.PutUint32(eb[:], uint32(int32(exp))^(1<<31))
		for _, b := range eb {
			buf = append(buf, b^inv)
		}
		for i := 0; i < len(digits); i += 2 {
			pair := (digits[i] - '0') * 10
			if i+1 < len(digits) {
				pair += digits[i+1] - '0'
			}
			buf = append(buf, (1+pair)^inv)
		}
		buf = append(buf, decEnd^inv)
	}
	if withScale {
		var sb [2]byte
		binary.BigEndian.PutUint16(sb[:], uint16(d.Scale()))
		buf = append(buf, sb[0]^mask, sb[1]^mask)
	}
	return buf
}

// decScan is one decimal element's parts as scanDecimal validated them.
type decScan struct {
	neg   bool
	pairs []byte // the stored digit-pair bytes (still XOR inv); nil for zero
	inv   byte   // what the pair bytes are XORed with
	exp   int
	scale int32
}

// scanDecimal validates a decimal body (after its tag) whose stored
// bytes are XOR mask, without building the value; withScale is
// tagDecimalScaled. Decode and Skip both go through it, so they reject
// exactly the same input, and anything it accepts builds without error.
// Malformed input is an error, never a panic: keys and values come from
// disk.
func scanDecimal(data []byte, mask byte, withScale bool) (sc decScan, rest []byte, err error) {
	if len(data) < 1 {
		return sc, nil, serr.New("tuple: truncated decimal element")
	}
	cls, data := data[0]^mask, data[1:]
	switch cls {
	case decZero:
	case decPos, decNeg:
		sc.neg = cls == decNeg
		sc.inv = mask
		if sc.neg {
			sc.inv ^= 0xFF
		}
		if len(data) < 4 {
			return sc, nil, serr.New("tuple: truncated decimal exponent")
		}
		var eb [4]byte
		for i := range eb {
			eb[i] = data[i] ^ sc.inv
		}
		sc.exp = int(int32(binary.BigEndian.Uint32(eb[:]) ^ (1 << 31)))
		data = data[4:]
		end := bytes.IndexByte(data, decEnd^sc.inv)
		if end < 1 {
			return sc, nil, serr.New("tuple: malformed decimal digits")
		}
		sc.pairs = data[:end]
		for _, b := range sc.pairs {
			if p := b ^ sc.inv; p < 1 || p > 100 {
				return sc, nil, serr.New("tuple: malformed decimal digit pair")
			}
		}
		data = data[end+1:]
	default:
		return sc, nil, serr.New("tuple: unknown decimal class", "class", fmt.Sprintf("0x%02x", cls))
	}
	if withScale {
		if len(data) < 2 {
			return sc, nil, serr.New("tuple: truncated decimal scale")
		}
		sc.scale = int32(binary.BigEndian.Uint16([]byte{data[0] ^ mask, data[1] ^ mask}))
		data = data[2:]
	}
	// The limits decimal.FromMantissa enforces, checked here so the
	// build cannot fail: at most MaxIntDigits before the point, and a
	// scale (own or displayed) of at most MaxScale. 2·len(pairs) bounds
	// the digit count; the pad digit makes it at most one too high, which
	// only matters right at the limit and errs toward refusing.
	if sc.pairs != nil {
		if own := 2*len(sc.pairs) - sc.exp; sc.exp > decimal.MaxIntDigits || own > decimal.MaxScale {
			return sc, nil, serr.New("tuple: decimal element out of range")
		}
	}
	if sc.scale > decimal.MaxScale {
		return sc, nil, serr.New("tuple: decimal scale out of range")
	}
	return sc, data, nil
}

// decodeDecimal reads one decimal body (see scanDecimal).
func decodeDecimal(data []byte, mask byte, withScale bool) (decimal.Decimal, []byte, error) {
	sc, rest, err := scanDecimal(data, mask, withScale)
	if err != nil {
		return decimal.Decimal{}, nil, err
	}
	if len(sc.pairs) > 0 && len(sc.pairs) <= 9 {
		// Up to 18 digits: accumulate the mantissa as a number, no digit
		// string (most money-sized values; this runs per row per column).
		var mant uint64
		for _, b := range sc.pairs {
			mant = mant*100 + uint64(b^sc.inv-1)
		}
		n := 2 * len(sc.pairs)
		// Drop trailing zeros: the pad digit of an odd-length mantissa,
		// and any more a crafted pair carries.
		for mant != 0 && mant%10 == 0 {
			mant /= 10
			n--
		}
		if mant == 0 { // crafted all-zero pairs: the value is 0
			return decimal.Decimal{}, rest, nil
		}
		d, err := decimal.FromMantissaU64(sc.neg, mant, n, sc.exp, sc.scale)
		if err != nil {
			return decimal.Decimal{}, nil, serr.Wrap(err, "op", "decode decimal element")
		}
		return d, rest, nil
	}
	var digits []byte
	if sc.pairs != nil {
		digits = make([]byte, 0, 2*len(sc.pairs))
		for _, b := range sc.pairs {
			p := b ^ sc.inv - 1
			digits = append(digits, '0'+p/10, '0'+p%10)
		}
		// An odd-length mantissa's final pair carries a pad zero; a
		// mantissa never ends in 0, so dropping trailing zeros removes
		// exactly the pad (and normalizes crafted input that has more).
		digits = bytes.TrimRight(digits, "0")
		if len(digits) == 0 { // crafted all-zero pairs: the value is 0
			return decimal.Decimal{}, rest, nil
		}
	}
	d, err := decimal.FromMantissa(sc.neg, string(digits), sc.exp, sc.scale)
	if err != nil {
		return decimal.Decimal{}, nil, serr.Wrap(err, "op", "decode decimal element")
	}
	return d, rest, nil
}

// skipDecimal is decodeDecimal without building the value (ascending
// encoding only, as Skip).
func skipDecimal(data []byte, withScale bool) ([]byte, error) {
	_, rest, err := scanDecimal(data, 0x00, withScale)
	return rest, err
}

func appendEscaped(buf []byte, mask byte, s []byte) []byte {
	for _, c := range s {
		if c == escByte {
			buf = append(buf, escByte^mask, escaped00^mask)
		} else {
			buf = append(buf, c^mask)
		}
	}
	return append(buf, escByte^mask, terminator^mask)
}

// Decode decodes every element of an encoded tuple.
func Decode(data []byte) ([]any, error) {
	var out []any
	for len(data) > 0 {
		v, rest, err := decodeOne(data, 0x00)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		data = rest
	}
	return out, nil
}

// DecodeOne decodes the first element of data — encoded ascending, or
// descending (AppendDesc) when desc is true — returning the value and
// the remaining bytes. Callers that mix directions within one key
// decode element by element, passing each element's direction.
func DecodeOne(data []byte, desc bool) (v any, rest []byte, err error) {
	if len(data) == 0 {
		return nil, nil, serr.New("tuple: empty input")
	}
	var mask byte
	if desc {
		mask = 0xFF
	}
	return decodeOne(data, mask)
}

// decodeOne decodes one element whose stored bytes are the ascending
// encoding XOR mask.
func decodeOne(data []byte, mask byte) (v any, rest []byte, err error) {
	u64 := func(b []byte) uint64 {
		u := binary.BigEndian.Uint64(b)
		if mask != 0 {
			u = ^u
		}
		return u
	}
	tag, data := data[0]^mask, data[1:]
	switch tag {
	case tagNull:
		return nil, data, nil
	case tagFalse:
		return false, data, nil
	case tagTrue:
		return true, data, nil
	case tagInt:
		if len(data) < 8 {
			return nil, nil, serr.New("tuple: truncated int element")
		}
		u := u64(data) ^ (1 << 63)
		return int64(u), data[8:], nil
	case tagFloat:
		if len(data) < 8 {
			return nil, nil, serr.New("tuple: truncated float element")
		}
		bits := u64(data)
		if bits&(1<<63) != 0 {
			bits &^= 1 << 63
		} else {
			bits = ^bits
		}
		return math.Float64frombits(bits), data[8:], nil
	case tagString:
		str, rest, err := unescapeString(data, mask)
		if err != nil {
			return nil, nil, err
		}
		return str, rest, nil
	case tagBytes:
		raw, rest, err := unescape(data, mask)
		if err != nil {
			return nil, nil, err
		}
		return raw, rest, nil
	case tagDecimal, tagDecimalScaled:
		d, rest, err := decodeDecimal(data, mask, tag == tagDecimalScaled)
		if err != nil {
			return nil, nil, err
		}
		return d, rest, nil
	}
	return nil, nil, serr.New("tuple: unknown type tag", "tag", fmt.Sprintf("0x%02x", tag))
}

// AppendInt appends one ascending int64 element: Append(buf, v) without
// boxing v into an interface (which allocates for values above 255).
// Key prefixes — table and index IDs — are built this way on every
// write (deep-look D-45).
func AppendInt(buf []byte, v int64) []byte {
	buf = append(buf, tagInt)
	return binary.BigEndian.AppendUint64(buf, uint64(v)^(1<<63))
}

// Skip returns data past its first element (ascending encoding)
// without materializing the value: no allocation for any type. Row
// decoding uses it for columns a query does not read (deep-look D-39).
func Skip(data []byte) (rest []byte, err error) {
	if len(data) == 0 {
		return nil, serr.New("tuple: empty input")
	}
	tag, data := data[0], data[1:]
	switch tag {
	case tagNull, tagFalse, tagTrue:
		return data, nil
	case tagInt, tagFloat:
		if len(data) < 8 {
			return nil, serr.New("tuple: truncated numeric element")
		}
		return data[8:], nil
	case tagBytes, tagString:
		_, _, end, err := scanEscaped(data, 0x00)
		if err != nil {
			return nil, err
		}
		return data[end+2:], nil
	case tagDecimal, tagDecimalScaled:
		return skipDecimal(data, tag == tagDecimalScaled)
	}
	return nil, serr.New("tuple: unknown type tag", "tag", fmt.Sprintf("0x%02x", tag))
}

// unescape reads an escaped byte string whose stored bytes are XOR
// mask, returning the logical bytes.
//
// Two passes: first locate the terminator and count escape pairs, then
// emit into one exact-size allocation. This runs for every string and
// bytes element of every row decoded, and the old byte-at-a-time append
// from a zero-capacity slice re-allocated geometrically (~10 growths
// for a 1 KB value) on each — pure GC tax on the scan path. The common
// no-escape case reduces to one allocation plus a copy (or an XOR loop
// when a descending column's mask is set).
func unescape(data []byte, mask byte) (raw, rest []byte, err error) {
	n, escapes, end, err := scanEscaped(data, mask)
	if err != nil {
		return nil, nil, err
	}
	raw = make([]byte, n)
	fillUnescaped(raw, data[:end], mask, escapes)
	return raw, data[end+2:], nil
}

// unescapeString is unescape producing a string in ONE allocation. A
// string element used to cost two — the raw bytes, then string(raw)'s
// copy — for every text column of every row decoded (deep-look D-39).
// The common case (no escapes, ascending) converts the stored bytes
// directly; otherwise the bytes are unescaped into a builder sized
// exactly, whose String() takes them without copying.
func unescapeString(data []byte, mask byte) (str string, rest []byte, err error) {
	n, escapes, end, err := scanEscaped(data, mask)
	if err != nil {
		return "", nil, err
	}
	rest = data[end+2:]
	if escapes == 0 && mask == 0 {
		return string(data[:end]), rest, nil
	}
	var b strings.Builder
	b.Grow(n)
	body := data[:end]
	for i := 0; i < len(body); i++ {
		c := body[i] ^ mask
		if c == escByte {
			i++ // skip the pair's second byte; the escape byte is the value
		}
		b.WriteByte(c)
	}
	return b.String(), rest, nil
}

// scanEscaped locates an escaped byte string's terminator. It returns
// the logical length n, the number of escape pairs, and end, the index
// of the terminator pair in data (the element continues at end+2).
func scanEscaped(data []byte, mask byte) (n, escapes, end int, err error) {
	esc := escByte ^ mask // the escape byte as it appears in stored form
	end = -1
scan:
	for i := 0; i < len(data); i++ {
		if data[i] != esc {
			n++
			continue
		}
		if i+1 >= len(data) {
			break // dangling escape byte: unterminated
		}
		switch data[i+1] ^ mask {
		case escaped00:
			n++
			escapes++
			i++
		case terminator:
			end = i
			break scan
		default:
			return 0, 0, 0, serr.New("tuple: invalid escape sequence")
		}
	}
	if end < 0 {
		return 0, 0, 0, serr.New("tuple: unterminated byte-string element")
	}
	return n, escapes, end, nil
}

// fillUnescaped writes body's logical bytes into raw (len(raw) is the
// logical length scanEscaped counted).
func fillUnescaped(raw, body []byte, mask byte, escapes int) {
	if escapes == 0 {
		if mask == 0 {
			copy(raw, body)
		} else {
			for i, b := range body {
				raw[i] = b ^ mask
			}
		}
		return
	}
	j := 0
	for i := 0; i < len(body); i++ {
		b := body[i] ^ mask
		if b == escByte {
			i++ // skip the pair's second byte; the escape byte is the value
		}
		raw[j] = b
		j++
	}
}

// Compare orders two tuples element-wise with the same semantics the
// encoding preserves; a tuple that is a prefix of the other sorts
// first. It panics on values Encode would reject — compare only what
// you could encode.
func Compare(a, b []any) int {
	for i := range min(len(a), len(b)) {
		if c := compareValues(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}

func compareValues(av, bv any) int {
	a, err := normalize(av)
	if err != nil {
		panic(err)
	}
	b, err := normalize(bv)
	if err != nil {
		panic(err)
	}
	if c := cmp.Compare(rank(a), rank(b)); c != 0 {
		return c
	}
	switch t := a.(type) {
	case int64:
		return cmp.Compare(t, b.(int64))
	case float64:
		return compareFloat(t, b.(float64))
	case []byte:
		return bytes.Compare(t, b.([]byte))
	case string:
		return cmp.Compare(t, b.(string))
	case decimal.Decimal:
		return t.Cmp(b.(decimal.Decimal))
	}
	return 0 // nil, false, true carry their value in the rank
}

func rank(v any) int {
	switch v := v.(type) {
	case nil:
		return int(tagNull)
	case bool:
		if v {
			return int(tagTrue)
		}
		return int(tagFalse)
	case int64:
		return int(tagInt)
	case float64:
		return int(tagFloat)
	case []byte:
		return int(tagBytes)
	case string:
		return int(tagString)
	case decimal.Decimal:
		return int(tagDecimal)
	}
	panic(fmt.Sprintf("tuple: unrankable type %T", v))
}

// normalize maps supported inputs onto the canonical element types.
func normalize(v any) (any, error) {
	switch t := v.(type) {
	case nil, bool, int64, string, []byte, decimal.Decimal:
		return t, nil
	case int:
		return int64(t), nil
	case int8:
		return int64(t), nil
	case int16:
		return int64(t), nil
	case int32:
		return int64(t), nil
	case uint:
		if uint64(t) > math.MaxInt64 {
			return nil, serr.New("tuple: uint value overflows int64")
		}
		return int64(t), nil
	case uint8:
		return int64(t), nil
	case uint16:
		return int64(t), nil
	case uint32:
		return int64(t), nil
	case uint64:
		if t > math.MaxInt64 {
			return nil, serr.New("tuple: uint64 value overflows int64")
		}
		return int64(t), nil
	case float32:
		return normalizeFloat(float64(t))
	case float64:
		return normalizeFloat(t)
	}
	return nil, serr.New("tuple: unsupported element type", "type", fmt.Sprintf("%T", v))
}

// canonicalNaN is the one NaN the encoding stores: positive, quiet.
// Under the sign transform appendValue applies, its bits order just
// above +Inf, which is where Postgres sorts NaN.
var canonicalNaN = math.Float64frombits(0x7FF8000000000000)

func normalizeFloat(f float64) (any, error) {
	if math.IsNaN(f) {
		// Every NaN encodes as the canonical one, so they all compare
		// equal (and a negative-signed NaN cannot sort below -Inf).
		return canonicalNaN, nil
	}
	if f == 0 {
		return 0.0, nil // collapse -0 so equal values encode identically
	}
	return f, nil
}

// compareFloat orders floats as the encoding does: NaN above every
// number and equal to itself. (cmp.Compare puts NaN below everything,
// which would contradict the byte order.)
func compareFloat(a, b float64) int {
	an, bn := math.IsNaN(a), math.IsNaN(b)
	switch {
	case an && bn:
		return 0
	case an:
		return 1
	case bn:
		return -1
	}
	return cmp.Compare(a, b)
}

// PrefixEnd returns the smallest byte string greater than every string
// having prefix as a prefix — the exclusive upper bound for a prefix
// scan. It returns nil when no such bound exists (all bytes 0xFF).
func PrefixEnd(prefix []byte) []byte {
	end := bytes.Clone(prefix)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xFF {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}
