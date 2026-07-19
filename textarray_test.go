package bytdb

// Edge cases for the text[] literal parser/formatter pair — the
// canonical text these produce is the runtime AND storage form of
// every array value, so round-trip fidelity here is data integrity.

import "testing"

func TestTextArrayRoundTrip(t *testing.T) {
	cases := []struct {
		in    string
		canon string
	}{
		{`{}`, `{}`},
		{`{a}`, `{a}`},
		{`{a,b,c}`, `{a,b,c}`},
		{`{ a , b }`, `{a,b}`},                       // spacing is decoration
		{`{"a b",c}`, `{"a b",c}`},                   // spaces force quotes
		{`{"he said \"hi\""}`, `{"he said \"hi\""}`}, // escaped quotes survive
		{`{"back\\slash"}`, `{"back\\slash"}`},       // escaped backslash survives
		{`{"a,b"}`, `{"a,b"}`},                       // comma forces quotes
		{`{NULL,a}`, `{NULL,a}`},                     // NULL element
		{`{null}`, `{NULL}`},                         // any case, canonical upper
		{`{"null"}`, `{"null"}`},                     // quoted "null" is the word
		{`{""}`, `{""}`},                             // empty string element
		{`{"{brace}"}`, `{"{brace}"}`},               // braces force quotes
		{`  {a,b}  `, `{a,b}`},                       // outer whitespace trims
	}
	for _, c := range cases {
		got, err := CanonTextArray(c.in)
		if err != nil {
			t.Fatalf("CanonTextArray(%q): %v", c.in, err)
		}
		if got != c.canon {
			t.Fatalf("CanonTextArray(%q) = %q, want %q", c.in, got, c.canon)
		}
		// Canonical text is a fixed point.
		again, err := CanonTextArray(got)
		if err != nil || again != got {
			t.Fatalf("re-canon(%q) = %q, %v", got, again, err)
		}
	}
}

func TestTextArrayMalformed(t *testing.T) {
	for _, in := range []string{
		``, `a,b`, `{a`, `a}`, `{a,}`, `{,a}`, `{"a}`, `{"a" b}`, `{a"b"}`,
	} {
		if _, err := ParseTextArray(in); err == nil {
			t.Fatalf("ParseTextArray(%q) succeeded; want error", in)
		}
	}
	if _, err := ParseTextArray(`{{a},{b}}`); err == nil {
		t.Fatal("multidimensional literal accepted")
	}
}
