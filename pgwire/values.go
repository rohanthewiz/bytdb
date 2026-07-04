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
	oidBool   = 16
	oidBytea  = 17
	oidInt8   = 20
	oidInt2   = 21
	oidInt4   = 23
	oidText   = 25
	oidFloat4 = 700
	oidFloat8 = 701
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
	}
	return oidText
}

// typeSize is pg_type.typlen for RowDescription: fixed width in
// bytes, or -1 for variable.
func typeSize(t bytdb.ColType) int {
	switch t {
	case bytdb.TBool:
		return 1
	case bytdb.TInt, bytdb.TFloat:
		return 8
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
func encodeValue(v any, format int) ([]byte, error) {
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
	}
	return nil, serr.New("unsupported binary parameter type", "oid", fmt.Sprint(oid))
}
