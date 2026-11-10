package sql

// Deep-look D-44: LIKE 'prefix%' bounds a text-key scan; || and -
// decide jsonb-or-text once, in the typing pass.

import (
	"fmt"
	"strings"
	"testing"
)

func TestLikePrefixRange(t *testing.T) {
	for _, c := range []struct {
		pat, from, to string
		bounded, ok   bool
	}{
		{"abc%", "abc", "abd", true, true},
		{"ab_d", "ab", "ac", true, true},
		{"abc", "abc", "abd", true, true},
		{`a\%b%`, "a%b", "a%c", true, true},
		{"a\xff\xff%", "a\xff\xff", "b", true, true},
		{"\xff%", "\xff", "", false, true},
		{"%abc", "", "", false, false},
		{"_", "", "", false, false},
		{`ab\`, "", "", false, false},
		{"é%", "é", "\xc3\xaa", true, true},
	} {
		from, to, bounded, ok := likePrefixRange(c.pat)
		if from != c.from || to != c.to || bounded != c.bounded || ok != c.ok {
			t.Errorf("likePrefixRange(%q) = %q, %q, %v, %v; want %q, %q, %v, %v",
				c.pat, from, to, bounded, ok, c.from, c.to, c.bounded, c.ok)
		}
	}
}

func TestLikePrefixPushdown(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table w (id int primary key, name text, note text)`)
	exec(t, d, `create index w_name on w (name)`)
	for i, n := range []string{"apple", "apricot", "banana", "app", "ap", "b%x", "b%y", "Apple", "applesauce"} {
		exec(t, d, fmt.Sprintf(`insert into w values (%d, '%s', 'n')`, i, n))
	}
	q := `select name from w where name like 'app%' order by name`
	plan := explainText(t, d, q)
	if !strings.Contains(plan, "Index Scan using w_name") || !strings.Contains(plan, "(name >= 'app')") ||
		!strings.Contains(plan, "(name < 'apq')") || !strings.Contains(plan, "Filter: (name ~~ 'app%')") {
		t.Errorf("LIKE prefix not pushed:\n%s", plan)
	}
	for q, want := range map[string]string{
		`select name from w where name like 'app%' order by name`:   "[[app] [apple] [applesauce]]",
		`select name from w where name like 'ap_' order by name`:    "[[app]]",
		`select name from w where name like 'b\%%' order by name`:   "[[b%x] [b%y]]",
		`select name from w where name ilike 'app%' order by name`:  "[[Apple] [app] [apple] [applesauce]]",
		`select name from w where name not like 'a%' order by name`: "[[Apple] [b%x] [b%y] [banana]]",
	} {
		if got := fmt.Sprint(exec(t, d, q).Rows); got != want {
			t.Errorf("%s = %s, want %s", q, got, want)
		}
	}
}

// A long || chain is decided once by the typing pass (ExArith.Kind),
// and text and jsonb chains still evaluate as before.
func TestArithKindDecidedByTyping(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table j (id int primary key, t text, doc jsonb)`)
	exec(t, d, `insert into j values (1, 'x', '{"a": 1}')`)
	parts := make([]string, 30)
	for i := range parts {
		parts[i] = "t"
	}
	if got := exec(t, d, `select `+strings.Join(parts, " || ")+` from j`).Rows[0][0]; got != strings.Repeat("x", 30) {
		t.Fatalf("text chain = %v", got)
	}
	if got := exec(t, d, `select doc || '{"b": 2}' || '{"c": 3}' from j`).Rows[0][0]; got != `{"a":1,"b":2,"c":3}` {
		t.Fatalf("jsonb chain = %v", got)
	}
	if got := exec(t, d, `select doc - 'a' from j`).Rows[0][0]; got != `{}` {
		t.Fatalf("jsonb minus = %v", got)
	}
	st, err := Parse(`select t || t, doc || doc, id - 1 from j`)
	if err != nil {
		t.Fatal(err)
	}
	typed, err := d.typeStatement(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []arithKind
	for _, it := range typed.(*Select).Items {
		kinds = append(kinds, it.Ex.(*ExArith).Kind)
	}
	if fmt.Sprint(kinds) != fmt.Sprint([]arithKind{arithPlain, arithJSONB, arithPlain}) {
		t.Fatalf("kinds = %v", kinds)
	}
}
