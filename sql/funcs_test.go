package sql

// Deep-look D-53: the typing pass refuses a function no one implements,
// so its list of the functions outside the library (knownFuncs) must
// match what evalFunc's switch actually handles. This test reads the
// switch's case labels from the source.

import (
	"go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestKnownFuncsMatchEvalFunc(t *testing.T) {
	fset := gotoken.NewFileSet()
	f, err := goparser.ParseFile(fset, "expr.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	inSwitch := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "evalFunc" {
			return true
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == gotoken.STRING {
					name, _ := strconv.Unquote(lit.Value)
					inSwitch[name] = true
				}
			}
			return true
		})
		return false
	})
	if len(inSwitch) == 0 {
		t.Fatal("found no case labels in evalFunc")
	}
	var missing []string
	for name := range inSwitch {
		if !isKnownFunc(name) {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("evalFunc handles functions knownFuncs omits (the typing pass would refuse them): %s",
			strings.Join(missing, ", "))
	}
}

func TestFunctionTypingErrors(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table ft (id int primary key, n int, s text, ts timestamp)`)
	for q, want := range map[string]string{
		`select nope(1)`:                   "function nope(bigint) does not exist",
		`select upper(n) from ft`:          "function upper(bigint) does not exist",
		`select length(n) from ft`:         "function length(bigint) does not exist",
		`select abs(s) from ft`:            "function abs(text) does not exist",
		`select round(1, 2, 3)`:            "does not exist",
		`select date_trunc(1, ts) from ft`: "does not exist",
	} {
		if _, err := d.Prepare(q); err == nil {
			// Typing runs at execution for a bare DB; either path must refuse.
			if _, err = d.Exec(q); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %v, want %q", q, err, want)
			}
		}
	}
	// Unknown functions fail before any row is read: an empty table
	// still refuses the statement.
	if _, err := d.Exec(`select nope(n) from ft`); err == nil {
		t.Error("unknown function over an empty table accepted")
	}
	// timezone with a numeric offset (POSIX sign: +05 is west of UTC).
	r := exec(t, d, `select '2024-01-01 12:00:00+00'::timestamptz at time zone '+05'`)
	if got := bytdbFormatTS(r.Rows[0][0]); got != "2024-01-01 07:00:00+00" {
		t.Errorf("AT TIME ZONE '+05' = %s", got)
	}
}

func bytdbFormatTS(v any) string {
	if n, ok := v.(int64); ok {
		return bytdb.FormatTimestamp(n)
	}
	return "?"
}
