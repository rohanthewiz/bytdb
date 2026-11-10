package pgwire

import (
	"reflect"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestValueRoundTrip(t *testing.T) {
	// Each value is encoded under its declared column type, as
	// sendDataRows does: the type is what the RowDescription told the
	// client, so it is what decides the bytes.
	cases := []struct {
		v   any
		oid uint32
		t   bytdb.ColType
	}{
		{true, oidBool, bytdb.TBool},
		{false, oidBool, bytdb.TBool},
		{int64(-42), oidInt8, bytdb.TInt},
		{int64(1 << 40), oidInt8, bytdb.TInt},
		{3.25, oidFloat8, bytdb.TFloat},
		{"héllo; 'quoted'", oidText, bytdb.TString},
		{[]byte{0x00, 0xff, 0x10}, oidBytea, bytdb.TBytes},
	}
	for _, c := range cases {
		for _, format := range []int{fmtText, fmtBinary} {
			enc, err := encodeValue(c.v, format, c.t)
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

// TestEncodeValueKindGuard (D-17): a value whose Go kind contradicts its
// declared column type used to go out as the value's own bytes under
// the declared OID — misread by the client, silently. Text-declared
// columns render any value as text (the right answer for the
// "described as text" fallback); an int under a float column converts;
// everything else is refused.
func TestEncodeValueKindGuard(t *testing.T) {
	for _, format := range []int{fmtText, fmtBinary} {
		// Text-declared: rendered as text in both formats.
		for _, tt := range []bytdb.ColType{"", bytdb.TString} {
			enc, err := encodeValue(int64(7), format, tt)
			if err != nil || string(enc) != "7" {
				t.Errorf("int64 under %q, format %d: %q, %v; want \"7\"", tt, format, enc, err)
			}
			enc, err = encodeValue(true, format, tt)
			if err != nil || string(enc) != "t" {
				t.Errorf("bool under %q, format %d: %q, %v; want \"t\"", tt, format, enc, err)
			}
		}
		// int under float converts.
		enc, err := encodeValue(int64(3), format, bytdb.TFloat)
		if err != nil {
			t.Fatalf("int64 under float: %v", err)
		}
		if dec, err := decodeParam(enc, format, oidFloat8); err != nil || dec != 3.0 {
			t.Errorf("int64 under float, format %d: decoded %v, %v", format, dec, err)
		}
		// Contradictions are refused.
		for _, c := range []struct {
			v any
			t bytdb.ColType
		}{
			{int64(1), bytdb.TBool},
			{"abc", bytdb.TInt},
			{"2024-01-01", bytdb.TTimestamp},
			{1.5, bytdb.TDate},
			{int64(5), bytdb.TUUID},
			{[]byte{1, 2}, bytdb.TUUID},
			{"x", bytdb.TBytes},
			{int64(1), bytdb.TJSONB},
		} {
			if enc, err := encodeValue(c.v, format, c.t); err == nil {
				t.Errorf("%T under %s, format %d: sent %q; want an error", c.v, c.t, format, enc)
			}
		}
	}
}
