package edgeaudit

import (
	"bufio"
	"encoding/binary"
	"io"
	"math"
	"strconv"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// Out-of-tree replica of pgwire's DataRow path (conn.go sendDataRows +
// send, values.go encodeValue) for one 5-column row shape, next to an
// append-into-a-reused-buffer variant, to size the avoidable per-row cost.

var benchRow = []any{int64(123456), "name-of-row", int64(864192), 185184.0, int64(1700000000123456)}
var benchTypes = []bytdb.ColType{bytdb.TInt, bytdb.TString, bytdb.TInt, bytdb.TFloat, bytdb.TTimestamp}

// currentEncode mirrors encodeValue: a fresh []byte per cell.
func currentEncode(v any, text bool, t bytdb.ColType) []byte {
	if t == bytdb.TTimestamp {
		x := v.(int64)
		if text {
			return []byte(bytdb.FormatTimestamp(x))
		}
		return binary.BigEndian.AppendUint64(nil, uint64(x-946684800000000))
	}
	switch x := v.(type) {
	case int64:
		if text {
			return strconv.AppendInt(nil, x, 10)
		}
		return binary.BigEndian.AppendUint64(nil, uint64(x))
	case float64:
		if text {
			return strconv.AppendFloat(nil, x, 'g', -1, 64)
		}
		return binary.BigEndian.AppendUint64(nil, math.Float64bits(x))
	case string:
		return []byte(x)
	}
	return nil
}

func benchCurrent(b *testing.B, text bool) {
	w := bufio.NewWriter(io.Discard)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var body []byte // `var b wbuf` per row
		body = binary.BigEndian.AppendUint16(body, uint16(len(benchRow)))
		for c, v := range benchRow {
			enc := currentEncode(v, text, benchTypes[c])
			body = binary.BigEndian.AppendUint32(body, uint32(len(enc)))
			body = append(body, enc...)
		}
		w.WriteByte('D') // send(): three writes
		var lb [4]byte
		binary.BigEndian.PutUint32(lb[:], uint32(len(body)+4))
		w.Write(lb[:])
		w.Write(body)
	}
}

func benchReuse(b *testing.B, text bool) {
	w := bufio.NewWriter(io.Discard)
	var buf []byte // one buffer per connection, reused across rows
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf = append(buf[:0], 'D', 0, 0, 0, 0)
		buf = binary.BigEndian.AppendUint16(buf, uint16(len(benchRow)))
		for c, v := range benchRow {
			lenAt := len(buf)
			buf = append(buf, 0, 0, 0, 0)
			switch x := v.(type) {
			case int64:
				switch {
				case benchTypes[c] == bytdb.TTimestamp && text:
					buf = append(buf, bytdb.FormatTimestamp(x)...) // an AppendTimestamp would drop this alloc too
				case benchTypes[c] == bytdb.TTimestamp:
					buf = binary.BigEndian.AppendUint64(buf, uint64(x-946684800000000))
				case text:
					buf = strconv.AppendInt(buf, x, 10)
				default:
					buf = binary.BigEndian.AppendUint64(buf, uint64(x))
				}
			case float64:
				if text {
					buf = strconv.AppendFloat(buf, x, 'g', -1, 64)
				} else {
					buf = binary.BigEndian.AppendUint64(buf, math.Float64bits(x))
				}
			case string:
				buf = append(buf, x...)
			}
			binary.BigEndian.PutUint32(buf[lenAt:], uint32(len(buf)-lenAt-4))
		}
		binary.BigEndian.PutUint32(buf[1:], uint32(len(buf)-1))
		w.Write(buf)
	}
}

func BenchmarkDataRow_CurrentText(b *testing.B)   { benchCurrent(b, true) }
func BenchmarkDataRow_ReuseText(b *testing.B)     { benchReuse(b, true) }
func BenchmarkDataRow_CurrentBinary(b *testing.B) { benchCurrent(b, false) }
func BenchmarkDataRow_ReuseBinary(b *testing.B)   { benchReuse(b, false) }
