package pgwire

// Deep-look D-41: DataRows are encoded into one reused buffer with
// back-patched lengths. The bytes must be exactly what building each
// message from per-value encodings produced.

import (
	"bufio"
	"bytes"
	"math"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestDataRowsMatchPerValueEncoding(t *testing.T) {
	types := []bytdb.ColType{bytdb.TInt, bytdb.TString, bytdb.TFloat, bytdb.TTimestamp, bytdb.TDate,
		bytdb.TUUID, bytdb.TBytes, bytdb.TBool, bytdb.TJSONB, bytdb.TTextArray, ""}
	uuid := []byte{0xa0, 0xee, 0xbc, 0x99, 0x9c, 0x0b, 0x4e, 0xf8, 0xbb, 0x6d, 0x6b, 0xb9, 0xbd, 0x38, 0x0a, 0x11}
	rows := [][]any{
		{int64(1), "a", 1.5, int64(1704067200123456), int64(19723), uuid, []byte{0, 1}, true, `{"k": 1}`, "{a,b}", int64(7)},
		{nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil},
		{int64(-9), string(make([]byte, 70000)), math.Copysign(0, -1), int64(-1), int64(-1), uuid, make([]byte, 3), false, `[]`, "{}", "x"},
		{int64(2), "", 1e300, int64(0), int64(0), uuid, []byte{}, true, `null`, `{"a b",NULL}`, 2.5},
	}
	for _, format := range []int{fmtText, fmtBinary} {
		formats := []int{format}
		var want bytes.Buffer
		for _, row := range rows {
			var b wbuf
			b.int16(len(row))
			for i, v := range row {
				if v == nil {
					b.int32(-1)
					continue
				}
				enc, err := encodeValue(v, format, types[i])
				if err != nil {
					t.Fatal(err)
				}
				b.int32(len(enc))
				b.raw(enc)
			}
			want.WriteByte(msgDataRow)
			want.Write([]byte{byte((len(b) + 4) >> 24), byte((len(b) + 4) >> 16), byte((len(b) + 4) >> 8), byte(len(b) + 4)})
			want.Write(b)
		}
		var got bytes.Buffer
		c := &conn{w: bufio.NewWriter(&got)}
		for range 2 { // the second pass reuses the retained buffer
			got.Reset()
			if !c.sendDataRows(rows, formats, types, nil) {
				t.Fatal("sendDataRows failed")
			}
			c.w.Flush()
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatalf("format %d: DataRow bytes differ from per-value encoding", format)
			}
		}
		if cap(c.rowBuf) > maxRetainedRowBuf {
			t.Fatalf("retained a %d-byte row buffer", cap(c.rowBuf))
		}
	}
}
