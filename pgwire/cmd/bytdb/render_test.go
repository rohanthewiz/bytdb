package main

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/rohanthewiz/bytdb"
)

func TestFormatFloat(t *testing.T) {
	cases := map[float64]string{
		0:            "0",
		1000000:      "1000000",
		0.1:          "0.1",
		-2.5:         "-2.5",
		1e15:         "1e+15",
		1e-5:         "1e-05",
		0.0001:       "0.0001",
		math.Inf(1):  "Infinity",
		math.Inf(-1): "-Infinity",
		math.NaN():   "NaN",
	}
	for in, want := range cases {
		if got := formatFloat(in); got != want {
			t.Errorf("formatFloat(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"users":      "users",
		"user":       `"user"`, // reserved
		"Order":      `"Order"`,
		"full name":  `"full name"`,
		"a\"b":       `"a""b"`,
		"_x9":        "_x9",
		"9x":         `"9x"`,
		"":           `""`,
		"select":     `"select"`,
		"created_at": "created_at",
	}
	for in, want := range cases {
		if got := quoteIdent(in); got != want {
			t.Errorf("quoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSQLLiteralAndJSON(t *testing.T) {
	uuid := []byte{0xa0, 0xee, 0xbc, 0x99, 0x9c, 0x0b, 0x4e, 0xf8, 0xbb, 0x6d, 0x6b, 0xb9, 0xbd, 0x38, 0x0a, 0x11}
	cases := []struct {
		v       any
		t       bytdb.ColType
		lit, js string
	}{
		{nil, bytdb.TInt, "NULL", "null"},
		{int64(42), bytdb.TInt, "42", "42"},
		{true, bytdb.TBool, "true", "true"},
		{"it's", bytdb.TString, "'it''s'", `"it's"`},
		{`back\slash`, bytdb.TString, `'back\slash'`, `"back\\slash"`},
		{1.5, bytdb.TFloat, "1.5", "1.5"},
		{math.Inf(1), bytdb.TFloat, "'Infinity'", `"Infinity"`},
		{[]byte{1, 2}, bytdb.TBytes, `'\x0102'`, `"\\x0102"`},
		{uuid, bytdb.TUUID, "'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'", `"a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"`},
		{int64(19724), bytdb.TDate, "'2024-01-02'", `"2024-01-02"`},
		{`{a,"b c",NULL}`, bytdb.TTextArray, `'{a,"b c",NULL}'`, `["a","b c",null]`},
		{`{"k":[1,2]}`, bytdb.TJSONB, `'{"k":[1,2]}'`, `{"k":[1,2]}`},
	}
	for _, c := range cases {
		if got := sqlLiteral(c.v, c.t); got != c.lit {
			t.Errorf("sqlLiteral(%v, %s) = %s, want %s", c.v, c.t, got, c.lit)
		}
		if got := jsonValue(c.v, c.t); got != c.js {
			t.Errorf("jsonValue(%v, %s) = %s, want %s", c.v, c.t, got, c.js)
		}
	}
}

func sampleSet() *resultSet {
	return &resultSet{
		cols:  []string{"id", "name"},
		types: []bytdb.ColType{bytdb.TInt, bytdb.TString},
		rows:  [][]any{{int64(1), "ada"}, {int64(22), nil}},
	}
}

func renderString(t *testing.T, rs *resultSet, o renderOpts) string {
	t.Helper()
	var b strings.Builder
	if err := render(&b, rs, o); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestRenderDuckbox(t *testing.T) {
	got := renderString(t, sampleSet(), renderOpts{mode: "duckbox", headers: true, null: "NULL"})
	want := `┌────────┬──────┐
│   id   │ name │
│ bigint │ text │
├────────┼──────┤
│      1 │ ada  │
│     22 │ NULL │
└────────┴──────┘
`
	if got != want {
		t.Errorf("duckbox:\n%s\nwant:\n%s", got, want)
	}

	empty := &resultSet{cols: []string{"a"}, types: []bytdb.ColType{bytdb.TInt}}
	got = renderString(t, empty, renderOpts{mode: "duckbox", headers: true})
	want = `┌────────┐
│   a    │
│ bigint │
├────────┤
│ 0 rows │
└────────┘
`
	if got != want {
		t.Errorf("empty duckbox:\n%s\nwant:\n%s", got, want)
	}
}

// TestRenderDuckboxElision checks the middle-row elision: first and
// last halves shown, three dot rows between, footer with the totals.
func TestRenderDuckboxElision(t *testing.T) {
	rs := &resultSet{cols: []string{"n"}, types: []bytdb.ColType{bytdb.TInt}}
	for i := 1; i <= 10; i++ {
		rs.rows = append(rs.rows, []any{int64(i)})
	}
	got := renderString(t, rs, renderOpts{mode: "duckbox", headers: true, maxRows: 4})
	// The cells between the borders, top to bottom, ignoring padding
	// (the footer legitimately widens the column).
	var cells []string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "│") {
			cells = append(cells, strings.TrimSpace(strings.Trim(line, "│")))
		}
	}
	want := []string{"n", "bigint", "1", "2", "·", "·", "·", "9", "10", "10 rows (4 shown)"}
	if strings.Join(cells, ",") != strings.Join(want, ",") {
		t.Errorf("cells %q, want %q in\n%s", cells, want, got)
	}
}

// TestFitColumnsProperty throws random column widths and budgets at the
// fitter and checks its contract: the table never exceeds the budget
// when any fit is possible, columns stay in order, and a fit without
// dropping columns is used whenever one exists.
func TestFitColumnsProperty(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for iter := 0; iter < 5000; iter++ {
		n := 1 + rng.IntN(12)
		natural := make([]int, n)
		for i := range natural {
			natural[i] = 1 + rng.IntN(60)
		}
		budget := 10 + rng.IntN(200)
		vis, ws := fitColumns(natural, budget)
		if len(vis) != len(ws) {
			t.Fatalf("len(vis)=%d len(ws)=%d", len(vis), len(ws))
		}
		total := 1
		last := -1
		for i, c := range vis {
			total += ws[i] + 3
			if c == ellipsisCol {
				continue
			}
			if c <= last {
				t.Fatalf("columns out of order: %v", vis)
			}
			last = c
			if ws[i] > natural[c] {
				t.Fatalf("column %d widened past natural %d: %d", c, natural[c], ws[i])
			}
		}
		if total > budget {
			t.Fatalf("natural %v budget %d: table width %d (vis %v ws %v)", natural, budget, total, vis, ws)
		}
		floorTotal := 1
		for _, w := range natural {
			floorTotal += min(w, boxMinWidth) + 3
		}
		if floorTotal <= budget && containsInt(vis, ellipsisCol) {
			t.Fatalf("natural %v budget %d: dropped columns although floors fit", natural, budget)
		}
	}
}

// TestRenderWideCells checks display-width alignment with wide runes:
// every line of the box must be the same number of terminal cells.
func TestRenderWideCells(t *testing.T) {
	rs := &resultSet{
		cols:  []string{"word"},
		types: []bytdb.ColType{bytdb.TString},
		rows:  [][]any{{"日本語"}, {"a"}, {"emoji 🎉"}},
	}
	out := renderString(t, rs, renderOpts{mode: "box", headers: true})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	w := runewidth.StringWidth(lines[0])
	for _, l := range lines {
		if runewidth.StringWidth(l) != w {
			t.Errorf("ragged box line %q (width %d, want %d)", l, runewidth.StringWidth(l), w)
		}
	}
}

func TestRenderOtherModes(t *testing.T) {
	rs := sampleSet()
	cases := []struct {
		o    renderOpts
		want string
	}{
		{renderOpts{mode: "csv", headers: true, colSep: ","}, "id,name\n1,ada\n22,\n"},
		{renderOpts{mode: "csv", headers: false, colSep: ",", csvNull: "\\N"}, "1,ada\n22,\\N\n"},
		{renderOpts{mode: "tabs", headers: true, colSep: "\t"}, "id\tname\n1\tada\n22\t\n"},
		{renderOpts{mode: "list", headers: true, colSep: "|", rowSep: "\n"}, "id|name\n1|ada\n22|\n"},
		{renderOpts{mode: "json"}, "[{\"id\":1,\"name\":\"ada\"},\n{\"id\":22,\"name\":null}]\n"},
		{renderOpts{mode: "jsonlines"}, "{\"id\":1,\"name\":\"ada\"}\n{\"id\":22,\"name\":null}\n"},
		{renderOpts{mode: "line", null: "NULL"}, "  id = 1\nname = ada\n\n  id = 22\nname = NULL\n"},
		{renderOpts{mode: "markdown", null: "NULL"}, "| id  | name |\n|----:|------|\n|   1 | ada  |\n|  22 | NULL |\n"},
		{renderOpts{mode: "insert", insertTable: "t"}, "INSERT INTO t (id, name) VALUES (1, 'ada');\nINSERT INTO t (id, name) VALUES (22, NULL);\n"},
		{renderOpts{mode: "trash"}, ""},
	}
	for _, c := range cases {
		if got := renderString(t, rs, c.o); got != c.want {
			t.Errorf("%s:\n%q\nwant\n%q", c.o.mode, got, c.want)
		}
	}
}
