package pgwire

import (
	"math"
	"reflect"
	"testing"
)

// TestDecodeParamText covers the text-format decode paths not hit by
// the round-trip tests: each type's parse-failure branch and the bytea
// hex escape (with and without the \x prefix).
func TestDecodeParamText(t *testing.T) {
	// Success paths, including bytea's two accepted spellings.
	okCases := []struct {
		in   string
		oid  uint32
		want any
	}{
		{"true", oidBool, true},
		{"0", oidBool, false},
		{"-7", oidInt2, int64(-7)}, // int2/int4/int8 share one branch
		{"1e3", oidFloat8, 1000.0},
		{`\x00ff`, oidBytea, []byte{0x00, 0xff}},
		// Without the \x prefix the raw text bytes pass through.
		{"raw", oidBytea, []byte("raw")},
	}
	for _, c := range okCases {
		got, err := decodeParam([]byte(c.in), fmtText, c.oid)
		if err != nil {
			t.Fatalf("decodeParam(%q, text, %d): %v", c.in, c.oid, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("decodeParam(%q, text, %d) = %#v; want %#v", c.in, c.oid, got, c.want)
		}
	}

	// Parse-failure paths: each typed OID refuses malformed text.
	badCases := []struct {
		in  string
		oid uint32
	}{
		{"maybe", oidBool},
		{"12x", oidInt8},
		{"1.2.3", oidFloat4},
		{`\xzz`, oidBytea}, // \x prefix but invalid hex digits
	}
	for _, c := range badCases {
		if got, err := decodeParam([]byte(c.in), fmtText, c.oid); err == nil {
			t.Errorf("decodeParam(%q, text, %d) = %#v; want error", c.in, c.oid, got)
		}
	}
}

// TestDecodeParamBinary covers the binary-format paths the round-trip
// tests miss: the length check on every fixed-width type, the text and
// bytea pass-throughs, and the unknown-format-code refusal.
func TestDecodeParamBinary(t *testing.T) {
	// float4 widens to float64 exactly for values float32 represents.
	f4 := make([]byte, 4)
	bits := math.Float32bits(2.5)
	f4[0], f4[1], f4[2], f4[3] = byte(bits>>24), byte(bits>>16), byte(bits>>8), byte(bits)
	if v, err := decodeParam(f4, fmtBinary, oidFloat4); err != nil || v != 2.5 {
		t.Errorf("binary float4: %v %v", v, err)
	}

	// Variable-width binary types pass through byte-for-byte.
	if v, err := decodeParam([]byte("héllo"), fmtBinary, oidText); err != nil || v != "héllo" {
		t.Errorf("binary text: %v %v", v, err)
	}

	// Every fixed-width binary type rejects a wrong-length payload —
	// the wire gives explicit lengths, so a mismatch is a client bug
	// worth refusing rather than truncating or zero-padding.
	wrongLen := []struct {
		oid uint32
		n   int
	}{
		{oidBool, 2},
		{oidInt2, 1},
		{oidInt4, 8},
		{oidInt8, 4},
		{oidFloat4, 8},
		{oidFloat8, 4},
	}
	for _, c := range wrongLen {
		if got, err := decodeParam(make([]byte, c.n), fmtBinary, c.oid); err == nil {
			t.Errorf("decodeParam(len %d, binary, oid %d) = %#v; want error", c.n, c.oid, got)
		}
	}

	// Format codes other than 0 (text) and 1 (binary) are refused.
	if got, err := decodeParam([]byte{1}, 2, oidBool); err == nil {
		t.Errorf("decodeParam(format 2) = %#v; want error", got)
	}
}
