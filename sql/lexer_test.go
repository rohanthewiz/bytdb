package sql

import (
	"strings"
	"testing"
)

func TestLexBasics(t *testing.T) {
	src := `SELECT Id, "Weird Col" FROM users -- trailing
		WHERE age >= 21 AND name = 'it''s' /* block
		comment */ LIMIT 1.5e2;`
	toks, err := lex(src)
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		kind tokKind
		text string
	}
	wants := []want{
		{tIdent, "select"}, {tIdent, "id"}, {tOp, ","}, {tQIdent, "Weird Col"},
		{tIdent, "from"}, {tIdent, "users"}, {tIdent, "where"},
		{tIdent, "age"}, {tOp, ">="}, {tNumber, "21"}, {tIdent, "and"},
		{tIdent, "name"}, {tOp, "="}, {tString, "it's"},
		{tIdent, "limit"}, {tNumber, "1.5e2"}, {tOp, ";"}, {tEOF, ""},
	}
	if len(toks) != len(wants) {
		t.Fatalf("got %d tokens, want %d: %v", len(toks), len(wants), toks)
	}
	for i, w := range wants {
		if toks[i].kind != w.kind || toks[i].text != w.text {
			t.Errorf("token %d: got {%d %q}, want {%d %q}", i, toks[i].kind, toks[i].text, w.kind, w.text)
		}
	}
}

func TestLexOpsAndParams(t *testing.T) {
	toks, err := lex(`a != b <> c <= d $1 .5 -2`)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []tokKind
	var texts []string
	for _, tk := range toks {
		kinds = append(kinds, tk.kind)
		texts = append(texts, tk.text)
	}
	wantTexts := []string{"a", "!=", "b", "<>", "c", "<=", "d", "$1", ".5", "-", "2", ""}
	if len(texts) != len(wantTexts) {
		t.Fatalf("got %v", texts)
	}
	for i, w := range wantTexts {
		if texts[i] != w {
			t.Errorf("token %d: got %q, want %q", i, texts[i], w)
		}
	}
	if kinds[7] != tParam || kinds[8] != tNumber {
		t.Errorf("kinds: $1 -> %d, .5 -> %d", kinds[7], kinds[8])
	}
}

// TestLexNestedBlockComments: block comments nest, as in Postgres
// (N-034), so the comment ends at the "*/" that closes the outermost
// "/*", not at the first one.
func TestLexNestedBlockComments(t *testing.T) {
	for _, c := range []struct {
		src  string
		want []string // token texts, EOF excluded
	}{
		{`a /* x /* y */ z */ b`, []string{"a", "b"}},
		{`a /* 1 /* 2 /* 3 */ 2 */ 1 */ b`, []string{"a", "b"}},
		{`a /**/ b /* /**/ */ c`, []string{"a", "b", "c"}},
		{`a /* ** */ b`, []string{"a", "b"}},
		{`a /* -- */ b`, []string{"a", "b"}},             // a line comment does not hide the close
		{`a /* ' */ b`, []string{"a", "b"}},              // nor does a quote
		{`a /* x */ */ b`, []string{"a", "*", "/", "b"}}, // a stray close is operators
	} {
		toks, err := lex(c.src)
		if err != nil {
			t.Errorf("lex(%q): %v", c.src, err)
			continue
		}
		var got []string
		for _, tk := range toks[:len(toks)-1] {
			got = append(got, tk.text)
		}
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("lex(%q) = %q, want %q", c.src, got, c.want)
		}
	}
	// Depth that never returns to 0 is unterminated, including "/*/",
	// whose "/" does not close the "/*" it follows.
	for _, src := range []string{`a /* x /* y */ b`, `/*/`, `/*/**/`} {
		if _, err := lex(src); err == nil || !strings.Contains(err.Error(), "unterminated block comment") {
			t.Errorf("lex(%q): got %v, want unterminated block comment", src, err)
		}
	}
}

func TestLexErrors(t *testing.T) {
	for _, src := range []string{`'unterminated`, `"unterminated`, `/* open`, `a & b`} {
		if _, err := lex(src); err == nil {
			t.Errorf("lex(%q): expected error", src)
		}
	}
}

func TestLexQuotedEscapes(t *testing.T) {
	toks, err := lex(`"we""ird"`)
	if err != nil {
		t.Fatal(err)
	}
	if toks[0].kind != tQIdent || toks[0].text != `we"ird` {
		t.Fatalf("got {%d %q}", toks[0].kind, toks[0].text)
	}
	if !strings.HasPrefix(toks[0].text, "we") {
		t.Fatal("sanity")
	}
}
