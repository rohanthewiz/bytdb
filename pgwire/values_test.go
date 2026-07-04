package pgwire

import (
	"reflect"
	"testing"
)

func TestValueRoundTrip(t *testing.T) {
	cases := []struct {
		v   any
		oid uint32
	}{
		{true, oidBool},
		{false, oidBool},
		{int64(-42), oidInt8},
		{int64(1 << 40), oidInt8},
		{3.25, oidFloat8},
		{"héllo; 'quoted'", oidText},
		{[]byte{0x00, 0xff, 0x10}, oidBytea},
	}
	for _, c := range cases {
		for _, format := range []int{fmtText, fmtBinary} {
			enc, err := encodeValue(c.v, format)
			if err != nil {
				t.Fatalf("encode(%v, %d): %v", c.v, format, err)
			}
			dec, err := decodeParam(enc, format, c.oid)
			if err != nil {
				t.Fatalf("decode(%v, %d): %v", c.v, format, err)
			}
			if !reflect.DeepEqual(dec, c.v) {
				t.Errorf("round trip %v format %d: got %v", c.v, format, dec)
			}
		}
	}
}

func TestDecodeClientTypes(t *testing.T) {
	// Client-declared narrow types widen to bytdb's int64/float64.
	if v, err := decodeParam([]byte{0x00, 0x07}, fmtBinary, oidInt2); err != nil || v != int64(7) {
		t.Errorf("int2: %v %v", v, err)
	}
	if v, err := decodeParam([]byte{0xff, 0xff, 0xff, 0xff}, fmtBinary, oidInt4); err != nil || v != int64(-1) {
		t.Errorf("int4: %v %v", v, err)
	}
	if v, err := decodeParam([]byte("2.5"), fmtText, oidFloat4); err != nil || v != 2.5 {
		t.Errorf("float4 text: %v %v", v, err)
	}
	// Unknown OID: text passes through as a string, binary refuses.
	if v, err := decodeParam([]byte("1700"), fmtText, 1700); err != nil || v != "1700" {
		t.Errorf("unknown text: %v %v", v, err)
	}
	if _, err := decodeParam([]byte{1}, fmtBinary, 1700); err == nil {
		t.Error("unknown binary: want error")
	}
}
