package tuple

import (
	"bytes"
	"math"
	"testing"
)

// Fuzz targets for the codec — the correctness foundation of the whole
// store. Two invariants are checked:
//
//  1. Decode of arbitrary bytes never panics (error-or-values, always),
//     and any successfully decoded value re-encodes to a stable
//     canonical form.
//  2. Byte order of encodings equals semantic order of tuples, in both
//     ascending and descending encodings.
//
// Run with: go test -fuzz=FuzzTupleRoundTrip ./tuple (and FuzzTupleOrder).
// Without -fuzz these run the seed corpus as regular tests.

// FuzzTupleRoundTrip feeds arbitrary bytes to the decoders. The
// decoders must never panic or read out of bounds. When arbitrary
// bytes happen to decode, re-encoding must be canonical: a second
// decode+encode round trip reproduces the same bytes and the same
// semantic value.
func FuzzTupleRoundTrip(f *testing.F) {
	// Seed with valid encodings of the tricky fixtures...
	seeds := [][]any{
		{nil},
		{true}, {false},
		{int64(0)}, {int64(-1)}, {int64(math.MaxInt64)}, {int64(math.MinInt64)},
		{0.0}, {-1.5}, {math.Inf(1)}, {math.Inf(-1)}, {math.SmallestNonzeroFloat64},
		{""}, {"a\x00b"}, {"a\x00"}, {[]byte{}}, {[]byte{0x00, 0xFF, 0x00}},
		{nil, false, int64(-7), 2.5, "k\x00ey", []byte{0}, true},
	}
	for _, vals := range seeds {
		enc, err := Encode(vals...)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(enc)
	}
	// ...and with known-corrupt inputs so the fuzzer starts at the
	// error paths too.
	for _, corrupt := range [][]byte{
		{tagInt, 0x01},
		{tagFloat},
		{tagString, 'a'},
		{tagString, 'a', 0x00},
		{tagString, 'a', 0x00, 0x77},
		{0x7F},
		{},
	} {
		f.Add(corrupt)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Descending decode must be panic-free on hostile bytes too;
		// its result is checked separately in FuzzTupleOrder.
		DecodeOne(data, true) //nolint:errcheck

		vals, err := Decode(data)
		if err != nil {
			return // rejected garbage is fine; a panic is a fuzz failure
		}
		enc, err := Encode(vals...)
		if err != nil {
			// Arbitrary bytes can decode to values Encode rejects
			// (only NaN today: Encode never emits it, but the float
			// bit pattern is reachable by crafted input).
			for _, v := range vals {
				if fv, ok := v.(float64); ok && math.IsNaN(fv) {
					return
				}
			}
			t.Fatalf("decoded values %#v failed to re-encode: %v", vals, err)
		}

		// From here on we are on canonical bytes: the round trip must
		// be exact and byte-stable.
		vals2, err := Decode(enc)
		if err != nil {
			t.Fatalf("re-decode of canonical encoding failed: %v", err)
		}
		if Compare(vals, vals2) != 0 {
			t.Fatalf("round trip changed value: %#v vs %#v", vals, vals2)
		}
		enc2, err := Encode(vals2...)
		if err != nil {
			t.Fatalf("re-encode failed: %v", err)
		}
		if !bytes.Equal(enc, enc2) {
			t.Fatalf("canonical encoding not byte-stable: % x vs % x", enc, enc2)
		}
	})
}

// FuzzTupleOrder derives two tuples from raw fuzz bytes and checks the
// core invariant: bytes.Compare on encodings orders exactly like
// Compare on the tuples — ascending, and reversed for descending.
func FuzzTupleOrder(f *testing.F) {
	f.Add([]byte{6, 0}, []byte{6, 1})       // boundary table entries
	f.Add([]byte{2, 0xFF}, []byte{3, 0x40}) // int vs float
	f.Add([]byte{4, 2, 'a', 0x00}, []byte{4, 1, 'a'})
	f.Add([]byte{0, 1, 1}, []byte{0}) // tuple-prefix case
	f.Add([]byte{5, 3, 0x00, 0x01, 0xFF}, []byte{5, 1, 0x00})

	f.Fuzz(func(t *testing.T, rawA, rawB []byte) {
		a := genTuple(rawA)
		b := genTuple(rawB)

		encA, err := Encode(a...)
		if err != nil {
			t.Fatalf("Encode(%#v): %v", a, err) // generator only emits encodable values
		}
		encB, err := Encode(b...)
		if err != nil {
			t.Fatalf("Encode(%#v): %v", b, err)
		}
		want := sign(Compare(a, b))
		if got := sign(bytes.Compare(encA, encB)); got != want {
			t.Fatalf("order mismatch: Compare(%#v, %#v)=%d but bytes.Compare=%d", a, b, want, got)
		}

		// Descending single-element encodings must reverse the order.
		// Whole-tuple desc comparison is only meaningful per element
		// (a shorter tuple's terminator-free end breaks the analogy),
		// so compare leading elements.
		if len(a) > 0 && len(b) > 0 {
			descA, err := AppendDesc(nil, a[0])
			if err != nil {
				t.Fatal(err)
			}
			descB, err := AppendDesc(nil, b[0])
			if err != nil {
				t.Fatal(err)
			}
			want := sign(Compare(a[:1], b[:1]))
			if got := sign(bytes.Compare(descA, descB)); got != -want {
				t.Fatalf("desc order not reversed for %#v vs %#v: want %d got %d", a[0], b[0], -want, got)
			}
			// Desc must round-trip through DecodeOne.
			v, rest, err := DecodeOne(descA, true)
			if err != nil || len(rest) != 0 {
				t.Fatalf("desc decode of %#v: v=%v rest=%d err=%v", a[0], v, len(rest), err)
			}
			if Compare([]any{v}, a[:1]) != 0 {
				t.Fatalf("desc round trip changed value: %#v vs %#v", a[0], v)
			}
		}
	})
}

// boundaryVals are the values most likely to break ordering: type-tag
// borders, integer extremes, float specials, and strings built from the
// escape-scheme bytes. genTuple reaches them with a two-byte sequence
// so the fuzzer finds cross-boundary comparisons quickly.
var boundaryVals = []any{
	nil, false, true,
	int64(math.MinInt64), int64(-1), int64(0), int64(1), int64(math.MaxInt64),
	math.Inf(-1), -math.MaxFloat64, -0.5, 0.0, math.SmallestNonzeroFloat64, 0.5, math.MaxFloat64, math.Inf(1),
	"", "\x00", "\x00\x01", "\x01", "\xff", "a", "a\x00", "a\x00b", "a\x01", "ab",
	[]byte{}, []byte{0x00}, []byte{0x00, 0xFF}, []byte{0x01}, []byte{0xFF},
}

// genTuple deterministically maps raw fuzz bytes onto a tuple of
// encodable values. The mapping is a tiny bytecode: one selector byte
// picks the element type, subsequent bytes supply the payload. Every
// output is encodable by construction (NaN is redirected), so the fuzz
// body can treat Encode errors as failures.
func genTuple(data []byte) []any {
	var vals []any
	for len(data) > 0 && len(vals) < 6 {
		sel := data[0] % 7
		data = data[1:]
		switch sel {
		case 0:
			vals = append(vals, nil)
		case 1:
			v := len(data) > 0 && data[0]&1 == 1
			data = consume(data, 1)
			vals = append(vals, v)
		case 2: // int64 from up to 8 payload bytes
			var u uint64
			n := min(len(data), 8)
			for _, c := range data[:n] {
				u = u<<8 | uint64(c)
			}
			data = consume(data, n)
			vals = append(vals, int64(u))
		case 3: // float64 from 8 payload bytes; NaN redirected to +Inf
			var u uint64
			n := min(len(data), 8)
			for _, c := range data[:n] {
				u = u<<8 | uint64(c)
			}
			data = consume(data, n)
			fv := math.Float64frombits(u)
			if math.IsNaN(fv) {
				fv = math.Inf(1)
			}
			vals = append(vals, fv)
		case 4: // string with explicit length byte
			var n int
			if len(data) > 0 {
				n = int(data[0] % 9)
				data = data[1:]
			}
			n = min(n, len(data))
			vals = append(vals, string(data[:n]))
			data = consume(data, n)
		case 5: // []byte with explicit length byte
			var n int
			if len(data) > 0 {
				n = int(data[0] % 9)
				data = data[1:]
			}
			n = min(n, len(data))
			vals = append(vals, bytes.Clone(data[:n]))
			data = consume(data, n)
		case 6: // pick from the boundary table
			var idx int
			if len(data) > 0 {
				idx = int(data[0]) % len(boundaryVals)
				data = data[1:]
			}
			vals = append(vals, boundaryVals[idx])
		}
	}
	return vals
}

func consume(data []byte, n int) []byte {
	if n > len(data) {
		return nil
	}
	return data[n:]
}
