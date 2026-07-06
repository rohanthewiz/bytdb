// Package tuple implements an order-preserving binary encoding for
// composite keys: for any two tuples, bytes.Compare on their encodings
// equals Compare on the tuples themselves. This is the property that
// lets a relational layer store rows and index entries in an ordered
// key-value store and answer range queries with key scans.
//
// Supported element types (after normalization): nil (NULL), bool,
// int64, float64, string, []byte. Inputs of any Go integer width are
// normalized to int64, float32 to float64. NaN is rejected; negative
// zero normalizes to positive zero.
//
// Elements of different types order by type tag: NULL < false < true <
// ints < floats < bytes < strings. Within a typed column only one tag
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
	return appendVals(buf, 0x00, vals)
}

// AppendDesc appends vals encoded for descending order: each element's
// ascending encoding, inverted byte-for-byte. Element encodings are
// prefix-free (fixed width, or escape-terminated), so whole-element
// inversion exactly reverses byte order and stays self-delimiting —
// ascending and descending elements mix freely within one key.
// Descending elements decode with DecodeOne(data, true); note that a
// descending NULL sorts after every value, the mirror of ascending.
func AppendDesc(buf []byte, vals ...any) ([]byte, error) {
	return appendVals(buf, 0xFF, vals)
}

// appendVals encodes vals with every byte XORed by mask (0x00 keeps
// ascending order; 0xFF reverses it).
func appendVals(buf []byte, mask byte, vals []any) ([]byte, error) {
	for _, v := range vals {
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
	case tagBytes, tagString:
		raw, rest, err := unescape(data, mask)
		if err != nil {
			return nil, nil, err
		}
		if tag == tagString {
			return string(raw), rest, nil
		}
		return raw, rest, nil
	}
	return nil, nil, serr.New("tuple: unknown type tag", "tag", fmt.Sprintf("0x%02x", tag))
}

// unescape reads an escaped byte string whose stored bytes are XOR
// mask, returning the logical bytes.
func unescape(data []byte, mask byte) (raw, rest []byte, err error) {
	raw = []byte{}
	for i := 0; i < len(data); i++ {
		if data[i]^mask != escByte {
			raw = append(raw, data[i]^mask)
			continue
		}
		if i+1 >= len(data) {
			break
		}
		switch data[i+1] ^ mask {
		case escaped00:
			raw = append(raw, escByte)
			i++
		case terminator:
			return raw, data[i+2:], nil
		default:
			return nil, nil, serr.New("tuple: invalid escape sequence")
		}
	}
	return nil, nil, serr.New("tuple: unterminated byte-string element")
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
		return cmp.Compare(t, b.(float64))
	case []byte:
		return bytes.Compare(t, b.([]byte))
	case string:
		return cmp.Compare(t, b.(string))
	}
	return 0 // nil, false, true carry their value in the rank
}

func rank(v any) int {
	switch v.(type) {
	case nil:
		return int(tagNull)
	case bool:
		if v.(bool) {
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
	}
	panic(fmt.Sprintf("tuple: unrankable type %T", v))
}

// normalize maps supported inputs onto the canonical element types.
func normalize(v any) (any, error) {
	switch t := v.(type) {
	case nil, bool, int64, string, []byte:
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

func normalizeFloat(f float64) (any, error) {
	if math.IsNaN(f) {
		return nil, serr.New("tuple: NaN is not encodable")
	}
	if f == 0 {
		return 0.0, nil // collapse -0 so equal values encode identically
	}
	return f, nil
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
