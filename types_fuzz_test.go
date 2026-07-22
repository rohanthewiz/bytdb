package bytdb

// Fuzz targets for the value text codecs that sit on the write path of
// every array / jsonb / temporal / uuid column. Two properties matter
// in production:
//
//  1. The hand-rolled parsers never panic on hostile input — they run
//     on user-supplied literals and wire parameters, so an out-of-bounds
//     read here would be a query-triggered crash.
//  2. Canonicalization is idempotent and the parse/format pairs are
//     lossless. Canonical text IS the stored form, so a value that does
//     not survive a round trip is silent data corruption: two equal
//     documents could compare unequal, or a stored value read back
//     changed.
//
// Run e.g. with: go test -fuzz=FuzzTextArrayCanon .

import "testing"

// FuzzTextArrayCanon feeds arbitrary bytes to the text[] literal parser.
// The parser must never panic; when a literal is well-formed, its
// canonical spelling must be a fixed point (canon(canon(x)) == canon(x))
// — the invariant that lets '=' on text[] reduce to string equality.
func FuzzTextArrayCanon(f *testing.F) {
	for _, s := range []string{
		`{}`, `{a}`, `{a,b,c}`, `{ a , b }`, `{"a b",c}`,
		`{"he said \"hi\""}`, `{"back\\slash"}`, `{NULL,a}`, `{null}`,
		`{"null"}`, `{""}`, `{"{brace}"}`, `  {a,b}  `,
		// malformed / hostile fragments the parser must reject cleanly
		``, `a,b`, `{a`, `a}`, `{a,}`, `{,a}`, `{"a}`, `{"a" b}`,
		`{a"b"}`, `{{a},{b}}`, `{"\`, `{`, `}`, `{"`,
		"{\x00}", "{a\x00b}", `{"\\"}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		canon, err := CanonTextArray(s)
		if err != nil {
			return // rejected garbage is fine; a panic is not
		}
		// Canonical output must itself parse, and to the same text.
		again, err := CanonTextArray(canon)
		if err != nil {
			t.Fatalf("canonical %q failed to re-parse: %v", canon, err)
		}
		if again != canon {
			t.Fatalf("canon not idempotent: %q -> %q -> %q", s, canon, again)
		}
		// The element list itself must also round-trip stably.
		elems, err := ParseTextArray(canon)
		if err != nil {
			t.Fatalf("ParseTextArray(%q): %v", canon, err)
		}
		if FormatTextArray(elems) != canon {
			t.Fatalf("format∘parse changed %q -> %q", canon, FormatTextArray(elems))
		}
	})
}

// FuzzJSONBCanon asserts jsonb canonicalization never panics and is a
// fixed point on anything it accepts — the basis of document equality
// via string comparison.
func FuzzJSONBCanon(f *testing.F) {
	for _, s := range []string{
		`{}`, `[]`, `null`, `true`, `"s"`, `{"a":1}`, `{"b":2,"a":1}`,
		`{"a":1,"a":2}`, `[1,2,3]`, `12345678901234567890`, `1.50`,
		`"a<b>&c"`, `{"k":[{"z":1,"a":null}]}`,
		// malformed
		``, `{`, `}`, `{"a":}`, `nul`, `{"a":1} x`, `[1,`, "\x00",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		canon, err := CanonJSONB(s)
		if err != nil {
			return
		}
		again, err := CanonJSONB(canon)
		if err != nil {
			t.Fatalf("canonical jsonb %q failed to re-parse: %v", canon, err)
		}
		if again != canon {
			t.Fatalf("jsonb canon not idempotent: %q -> %q -> %q", s, canon, again)
		}
	})
}

// FuzzTimestampRoundTrip checks micros -> text -> micros is exact over
// the range temporal columns actually hold. Bounded to years roughly
// 1..9999 so the property is about codec fidelity, not Go's 5-digit-year
// formatting at the int64 extremes (a separate, documented concern).
func FuzzTimestampRoundTrip(f *testing.F) {
	for _, m := range []int64{0, -1, 1, 1_500_000, -2_000_000, 1_704_164_645_123456} {
		f.Add(m)
	}
	const ( // microsecond bounds for 0001-01-01 .. 9999-12-31
		loMicros = -62_135_596_800_000_000
		hiMicros = 253_402_300_799_999_999
	)
	f.Fuzz(func(t *testing.T, micros int64) {
		if micros < loMicros || micros > hiMicros {
			return
		}
		got, err := ParseTimestamp(FormatTimestamp(micros))
		if err != nil {
			t.Fatalf("ParseTimestamp(FormatTimestamp(%d)) = err %v", micros, err)
		}
		if got != micros {
			t.Fatalf("timestamp round trip: %d -> %q -> %d", micros, FormatTimestamp(micros), got)
		}
	})
}

// FuzzUUIDRoundTrip checks that any 16 bytes render and parse back
// unchanged, and that the parser tolerates arbitrary-length text without
// panicking (36-char and 32-char paths plus everything that misses both).
func FuzzUUIDRoundTrip(f *testing.F) {
	f.Add([]byte("0123456789abcdef"))
	f.Add(make([]byte, 16))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		// Arbitrary text must never panic the parser.
		_, _ = ParseUUID(string(b))
		if len(b) != 16 {
			return
		}
		got, err := ParseUUID(FormatUUID(b))
		if err != nil {
			t.Fatalf("ParseUUID(FormatUUID(% x)) = err %v", b, err)
		}
		if string(got) != string(b) {
			t.Fatalf("uuid round trip: % x -> %q -> % x", b, FormatUUID(b), got)
		}
	})
}
