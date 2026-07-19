package bytdb

// Edge cases for jsonb canonicalization — the canonical text CanonJSONB
// produces is the runtime AND storage form of every jsonb value, so
// document equality reduces to string equality exactly when these
// canonicalizations hold.

import (
	"path/filepath"
	"testing"
)

func TestJSONBCanon(t *testing.T) {
	cases := []struct {
		in    string
		canon string
	}{
		{`{}`, `{}`},
		{`[]`, `[]`},
		{`null`, `null`},
		{`true`, `true`},
		{`"s"`, `"s"`},
		{`  {"a": 1}  `, `{"a":1}`},                     // whitespace vanishes
		{`{"b":2,"a":1}`, `{"a":1,"b":2}`},              // keys sort
		{`{"a":1,"a":2}`, `{"a":2}`},                    // last duplicate key wins
		{`{"a":{"c":3,"b":2}}`, `{"a":{"b":2,"c":3}}`},  // nested objects sort too
		{`[1, 2, 3]`, `[1,2,3]`},                        // array order is data, kept
		{`12345678901234567890`, `12345678901234567890`}, // big ints survive as text
		{`1.50`, `1.50`},                                // deliberate decimals survive
		{`"a<b>&c"`, `"a<b>&c"`},                        // no HTML escaping
		{`{"k":[{"z":1,"a":null}]}`, `{"k":[{"a":null,"z":1}]}`},
	}
	for _, c := range cases {
		got, err := CanonJSONB(c.in)
		if err != nil {
			t.Fatalf("CanonJSONB(%q): %v", c.in, err)
		}
		if got != c.canon {
			t.Fatalf("CanonJSONB(%q) = %q, want %q", c.in, got, c.canon)
		}
		// Canonical text is a fixed point.
		again, err := CanonJSONB(got)
		if err != nil || again != got {
			t.Fatalf("re-canon(%q) = %q, %v", got, again, err)
		}
	}
}

func TestJSONBMalformed(t *testing.T) {
	for _, in := range []string{
		``, `{`, `}`, `{"a":}`, `{"a" 1}`, `[1,]`, `nul`, `'a'`,
		`{"a":1} extra`, `{"a":1}{"b":2}`, // one document only
	} {
		if _, err := CanonJSONB(in); err == nil {
			t.Fatalf("CanonJSONB(%q) succeeded; want error", in)
		}
	}
}

func TestJSONBEngineWrites(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "jsonb.db"))
	defer e.Close()
	if _, err := e.CreateTable("docs", []Column{
		{Name: "id", Type: TInt},
		{Name: "body", Type: TJSONB},
	}, "id"); err != nil {
		t.Fatal(err)
	}

	// String and []byte (json.RawMessage's shape) both canonicalize on
	// the way in; a non-canonical spelling reads back canonical.
	if err := e.Insert("docs", 1, `{"b": 2, "a": 1}`); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("docs", 2, []byte(`[1, 2]`)); err != nil {
		t.Fatal(err)
	}
	row, ok, err := e.Get("docs", 1)
	if err != nil || !ok {
		t.Fatalf("get: %v ok=%v", err, ok)
	}
	if row.Col("body") != `{"a":1,"b":2}` {
		t.Fatalf("stored body = %q", row.Col("body"))
	}
	row, ok, err = e.Get("docs", 2)
	if err != nil || !ok {
		t.Fatalf("get: %v ok=%v", err, ok)
	}
	if row.Col("body") != `[1,2]` {
		t.Fatalf("stored body = %q", row.Col("body"))
	}

	// Malformed JSON is refused at the write, not stored.
	if err := e.Insert("docs", 3, `{oops`); err == nil {
		t.Fatal("malformed jsonb accepted")
	}
}
