package tuple

import (
	"math"
	"testing"
)

// TestNormalizeWidths pins the full width-normalization table: every
// supported Go integer and float kind maps onto the canonical element
// types (int64 / float64), so encodings of equal values are identical
// regardless of the caller's declared width.
func TestNormalizeWidths(t *testing.T) {
	cases := []struct {
		in   any
		want any
	}{
		// Canonical kinds pass through untouched.
		{nil, nil},
		{true, true},
		{int64(-9), int64(-9)},
		{"s", "s"},

		// Signed widths widen to int64, preserving sign.
		{int(-3), int64(-3)},
		{int8(-8), int64(-8)},
		{int16(-16), int64(-16)},
		{int32(-32), int64(-32)},

		// Unsigned widths widen to int64 (range-checked where the
		// unsigned range exceeds int64).
		{uint(3), int64(3)},
		{uint8(math.MaxUint8), int64(math.MaxUint8)},
		{uint16(math.MaxUint16), int64(math.MaxUint16)},
		{uint32(math.MaxUint32), int64(math.MaxUint32)},
		{uint64(math.MaxInt64), int64(math.MaxInt64)},

		// Floats widen/pass through; -0 collapses to +0 so equal
		// values encode identically.
		{float32(1.5), 1.5},
		{2.25, 2.25},
		{math.Copysign(0, -1), 0.0},
	}
	for _, c := range cases {
		got, err := normalize(c.in)
		if err != nil {
			t.Fatalf("normalize(%#v): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("normalize(%#v) = %#v; want %#v", c.in, got, c.want)
		}
	}

	// []byte compares by identity under ==, so check it separately.
	raw := []byte{0x00, 0x01}
	got, err := normalize(raw)
	if err != nil {
		t.Fatalf("normalize([]byte): %v", err)
	}
	if b, ok := got.([]byte); !ok || &b[0] != &raw[0] {
		t.Fatalf("normalize([]byte) did not pass the slice through: %#v", got)
	}
}

// TestNormalizeRejects covers every rejection path: values outside
// int64's range (from both uint and uint64), NaN, and Go types the
// encoding does not speak.
func TestNormalizeRejects(t *testing.T) {
	rejects := []any{
		uint(math.MaxUint64),   // uint overflow branch
		uint64(math.MaxUint64), // uint64 overflow branch
		math.NaN(),             // float64 NaN
		float32(math.NaN()),    // float32 NaN routes through the same check
		struct{}{},             // unsupported type
		[]int{1},               // unsupported type (slice of non-byte)
	}
	for _, v := range rejects {
		if got, err := normalize(v); err == nil {
			t.Fatalf("normalize(%#v) = %#v; want error", v, got)
		}
	}
}
