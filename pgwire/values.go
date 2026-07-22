package pgwire

// values.go: mapping bytdb column types and Go values onto PostgreSQL
// type OIDs and the text and binary wire formats.

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// PostgreSQL type OIDs (pg_type.oid) for the types this server speaks.
const (
	oidBool        = 16
	oidBytea       = 17
	oidInt8        = 20
	oidInt2        = 21
	oidInt4        = 23
	oidText        = 25
	oidFloat4      = 700
	oidFloat8      = 701
	oidDate        = 1082
	oidTimestamp   = 1114
	oidTimestamptz = 1184
	oidUUID        = 2950
	oidTextArray   = 1009 // _text: one-dimensional text[]
	oidJSON        = 114  // accepted on input; columns present as jsonb
	oidJSONB       = 3802
)

// Postgres's date/time binary formats count from 2000-01-01 rather
// than the Unix epoch bytdb stores; these are the offsets between the
// two, in the respective units.
const (
	pgEpochMicros = 946684800000000
	pgEpochDays   = 10957
)

// Wire format codes.
const (
	fmtText   = 0
	fmtBinary = 1
)

// oidForType is the OID a bytdb column type presents as. Untyped
// placeholders ("" from Describe) present as text.
func oidForType(t bytdb.ColType) uint32 {
	switch t {
	case bytdb.TBool:
		return oidBool
	case bytdb.TInt:
		return oidInt8
	case bytdb.TFloat:
		return oidFloat8
	case bytdb.TBytes:
		return oidBytea
	case bytdb.TTimestamp:
		return oidTimestamptz // stored instants are UTC
	case bytdb.TDate:
		return oidDate
	case bytdb.TUUID:
		return oidUUID
	case bytdb.TTextArray:
		return oidTextArray
	case bytdb.TJSONB:
		return oidJSONB
	}
	return oidText
}

// typeSize is pg_type.typlen for RowDescription: fixed width in
// bytes, or -1 for variable.
func typeSize(t bytdb.ColType) int {
	switch t {
	case bytdb.TBool:
		return 1
	case bytdb.TInt, bytdb.TFloat, bytdb.TTimestamp:
		return 8
	case bytdb.TDate:
		return 4
	case bytdb.TUUID:
		return 16
	}
	return -1
}

// formatFor resolves one position's format code from a Bind message's
// format list: none means all text, one applies to all, otherwise one
// per position.
func formatFor(formats []int, i int) int {
	switch len(formats) {
	case 0:
		return fmtText
	case 1:
		return formats[0]
	}
	return formats[i]
}

// encodeValue renders one result value (the Go kinds bytdb produces:
// int64, float64, string, bool, []byte) in the requested format. NULL
// is the caller's job (a DataRow length of -1).
//
// t is the column's declared type: the date/time and uuid types share
// runtime representations with int64 and []byte, so only the declared
// type can say whether an int64 is a count or an instant. "" (an
// untyped expression) falls through to representation-driven encoding.
func encodeValue(v any, format int, t bytdb.ColType) ([]byte, error) {
	switch t {
	case bytdb.TTimestamp:
		if x, ok := v.(int64); ok {
			if format == fmtText {
				return []byte(bytdb.FormatTimestamp(x)), nil
			}
			return binary.BigEndian.AppendUint64(nil, uint64(x-pgEpochMicros)), nil
		}
	case bytdb.TDate:
		if x, ok := v.(int64); ok {
			if format == fmtText {
				return []byte(bytdb.FormatDate(x)), nil
			}
			return binary.BigEndian.AppendUint32(nil, uint32(int32(x-pgEpochDays))), nil
		}
	case bytdb.TUUID:
		if x, ok := v.([]byte); ok && len(x) == 16 {
			if format == fmtText {
				return []byte(bytdb.FormatUUID(x)), nil
			}
			return x, nil
		}
	case bytdb.TTextArray:
		// The stored value IS the text wire form (the canonical array
		// literal), so text format passes through; binary re-encodes as
		// Postgres's array format, which pgx requests for text[].
		if x, ok := v.(string); ok {
			if format == fmtText {
				return []byte(x), nil
			}
			return encodeTextArrayBinary(x)
		}
	case bytdb.TJSONB:
		// The stored value IS the JSON text; jsonb's binary format is
		// just a 1-byte version prefix (always 1) ahead of that text.
		if x, ok := v.(string); ok {
			if format == fmtText {
				return []byte(x), nil
			}
			return append([]byte{1}, x...), nil
		}
	}
	if format == fmtText {
		switch x := v.(type) {
		case bool:
			if x {
				return []byte("t"), nil
			}
			return []byte("f"), nil
		case int64:
			return strconv.AppendInt(nil, x, 10), nil
		case float64:
			return strconv.AppendFloat(nil, x, 'g', -1, 64), nil
		case string:
			return []byte(x), nil
		case []byte:
			return []byte(`\x` + hex.EncodeToString(x)), nil
		}
		return nil, serr.New("unencodable result value", "type", fmt.Sprintf("%T", v))
	}
	switch x := v.(type) {
	case bool:
		if x {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case int64:
		return binary.BigEndian.AppendUint64(nil, uint64(x)), nil
	case float64:
		return binary.BigEndian.AppendUint64(nil, math.Float64bits(x)), nil
	case string:
		return []byte(x), nil
	case []byte:
		return x, nil
	}
	return nil, serr.New("unencodable result value", "type", fmt.Sprintf("%T", v))
}

// decodeParam converts one Bind parameter value to the Go value kinds
// sql.Exec binds. oid is the parameter's declared type: the client's
// when it sent one in Parse, otherwise derived from the inferred
// column type. Unrecognized OIDs pass through as text strings in text
// format and are refused in binary.
func decodeParam(raw []byte, format int, oid uint32) (any, error) {
	if format == fmtText {
		s := string(raw)
		switch oid {
		case oidBool:
			v, err := strconv.ParseBool(s)
			if err != nil {
				return nil, serr.New("bad boolean parameter", "value", s)
			}
			return v, nil
		case oidInt2, oidInt4, oidInt8:
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return nil, serr.New("bad integer parameter", "value", s)
			}
			return v, nil
		case oidFloat4, oidFloat8:
			v, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, serr.New("bad float parameter", "value", s)
			}
			return v, nil
		case oidBytea:
			if strings.HasPrefix(s, `\x`) {
				v, err := hex.DecodeString(s[2:])
				if err != nil {
					return nil, serr.New("bad bytea parameter", "value", s)
				}
				return v, nil
			}
			return raw, nil
		case oidTimestamp, oidTimestamptz:
			return bytdb.ParseTimestamp(s)
		case oidDate:
			return bytdb.ParseDate(s)
		case oidUUID:
			return bytdb.ParseUUID(s)
		case oidJSON, oidJSONB:
			// Pass the text through; the engine canonicalizes on write
			// and coerceLit canonicalizes comparisons, so the wire text
			// need not be canonical here.
			return s, nil
		}
		return s, nil
	}
	if format != fmtBinary {
		return nil, serr.New("bad parameter format code", "format", fmt.Sprint(format))
	}
	switch oid {
	case oidBool:
		if len(raw) != 1 {
			return nil, serr.New("bad binary boolean parameter")
		}
		return raw[0] != 0, nil
	case oidInt2:
		if len(raw) != 2 {
			return nil, serr.New("bad binary int2 parameter")
		}
		return int64(int16(binary.BigEndian.Uint16(raw))), nil
	case oidInt4:
		if len(raw) != 4 {
			return nil, serr.New("bad binary int4 parameter")
		}
		return int64(int32(binary.BigEndian.Uint32(raw))), nil
	case oidInt8:
		if len(raw) != 8 {
			return nil, serr.New("bad binary int8 parameter")
		}
		return int64(binary.BigEndian.Uint64(raw)), nil
	case oidFloat4:
		if len(raw) != 4 {
			return nil, serr.New("bad binary float4 parameter")
		}
		return float64(math.Float32frombits(binary.BigEndian.Uint32(raw))), nil
	case oidFloat8:
		if len(raw) != 8 {
			return nil, serr.New("bad binary float8 parameter")
		}
		return math.Float64frombits(binary.BigEndian.Uint64(raw)), nil
	case oidText:
		return string(raw), nil
	case oidBytea:
		return raw, nil
	case oidTimestamp, oidTimestamptz:
		if len(raw) != 8 {
			return nil, serr.New("bad binary timestamp parameter")
		}
		return int64(binary.BigEndian.Uint64(raw)) + pgEpochMicros, nil
	case oidDate:
		if len(raw) != 4 {
			return nil, serr.New("bad binary date parameter")
		}
		return int64(int32(binary.BigEndian.Uint32(raw))) + pgEpochDays, nil
	case oidUUID:
		if len(raw) != 16 {
			return nil, serr.New("bad binary uuid parameter")
		}
		return raw, nil
	case oidTextArray:
		return decodeTextArrayBinary(raw)
	case oidJSON:
		// json has no binary framing: the bytes are the text.
		return string(raw), nil
	case oidJSONB:
		// jsonb binary is a version byte (1) ahead of the text.
		if len(raw) < 1 || raw[0] != 1 {
			return nil, serr.New("bad binary jsonb parameter")
		}
		return string(raw[1:]), nil
	}
	return nil, serr.New("unsupported binary parameter type", "oid", fmt.Sprint(oid))
}

// --- text[] binary wire format ---
//
// Postgres's array binary format is a header — dimension count, a
// has-nulls flag, the element type OID, then (length, lower bound) per
// dimension — followed by each element as an int32 byte length (-1 for
// NULL) and its bytes. bytdb speaks only one-dimensional text arrays,
// so encoding is a straight walk over the parsed literal and decoding
// re-renders the canonical literal string the engine stores.

func encodeTextArrayBinary(literal string) ([]byte, error) {
	elems, err := bytdb.ParseTextArray(literal)
	if err != nil {
		return nil, err
	}
	i32 := func(buf []byte, n int32) []byte {
		return binary.BigEndian.AppendUint32(buf, uint32(n))
	}
	hasNull := int32(0)
	for _, e := range elems {
		if e == nil {
			hasNull = 1
		}
	}
	// Postgres encodes an empty array with zero dimensions (no dim
	// header at all), and clients expect exactly that.
	if len(elems) == 0 {
		buf := i32(nil, 0)
		buf = i32(buf, 0)
		return i32(buf, oidText), nil
	}
	buf := i32(nil, 1) // ndim
	buf = i32(buf, hasNull)
	buf = i32(buf, oidText)
	buf = i32(buf, int32(len(elems))) // dimension length
	buf = i32(buf, 1)                 // lower bound: Postgres arrays are 1-based
	for _, e := range elems {
		if e == nil {
			buf = i32(buf, -1)
			continue
		}
		s := e.(string)
		buf = i32(buf, int32(len(s)))
		buf = append(buf, s...)
	}
	return buf, nil
}

func decodeTextArrayBinary(raw []byte) (string, error) {
	bad := func() (string, error) { return "", serr.New("bad binary array parameter") }
	if len(raw) < 12 {
		return bad()
	}
	ndim := int32(binary.BigEndian.Uint32(raw[0:4]))
	// raw[4:8] is the has-nulls flag; the element lengths already say
	// which elements are NULL, so it needs no checking.
	elemOID := binary.BigEndian.Uint32(raw[8:12])
	if ndim == 0 {
		return "{}", nil
	}
	if ndim != 1 {
		return "", serr.New("multidimensional arrays are not supported")
	}
	// varchar (1043) elements are text on the wire too; anything else
	// (int arrays, say) has no bytdb type to land in.
	if elemOID != oidText && elemOID != 1043 {
		return "", serr.New("unsupported array element type", "oid", fmt.Sprint(elemOID))
	}
	if len(raw) < 20 {
		return bad()
	}
	n := int32(binary.BigEndian.Uint32(raw[12:16]))
	if n < 0 {
		return bad()
	}
	// n is a wire-controlled element count (up to ~2.1e9). A well-formed
	// body spends at least a 4-byte length prefix per element, so the
	// real element count cannot exceed the bytes that remain. Clamp the
	// preallocation to that ceiling: sizing make() by n alone lets a
	// 20-byte parameter request tens of GB, and that allocation OOMs the
	// process — a runtime throw that is fatal and bypasses run()'s
	// recover fence, so one hostile Bind would take down the whole
	// server, not just its connection. The loop below still validates
	// every element against len(raw) and errors out the moment the body
	// is exhausted.
	capHint := int(n)
	if maxElems := (len(raw) - 20) / 4; capHint > maxElems {
		capHint = maxElems
	}
	elems := make([]any, 0, capHint)
	p := 20 // past the dimension's (length, lower bound)
	for range n {
		if p+4 > len(raw) {
			return bad()
		}
		l := int32(binary.BigEndian.Uint32(raw[p : p+4]))
		p += 4
		if l == -1 {
			elems = append(elems, nil)
			continue
		}
		if l < 0 || p+int(l) > len(raw) {
			return bad()
		}
		elems = append(elems, string(raw[p:p+int(l)]))
		p += int(l)
	}
	return bytdb.FormatTextArray(elems), nil
}
