package sqlfrontaudit

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

// openDB opens a throwaway engine under the test's temp dir.
func openDB(t testing.TB, opts ...btypedb.Option) *bsql.DB {
	t.Helper()
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "test.db"), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return bsql.New(e)
}

func must(t testing.TB, d *bsql.DB, q string, args ...any) *bsql.Result {
	t.Helper()
	res, err := d.Exec(q, args...)
	if err != nil {
		t.Fatalf("Exec(%q): %v", q, err)
	}
	return res
}

// show renders a result or error compactly on one line.
func show(res *bsql.Result, err error) string {
	if err != nil {
		s := err.Error()
		if len(s) > 160 {
			s = s[:160]
		}
		return "ERR: " + strings.ReplaceAll(s, "\n", " | ")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "types=%v rows=", res.Types)
	for i, r := range res.Rows {
		if i > 0 {
			b.WriteString(" ; ")
		}
		for j, v := range r {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%#v", v)
		}
	}
	if len(res.Rows) == 0 {
		fmt.Fprintf(&b, "(none) affected=%d", res.RowsAffected)
	}
	return b.String()
}

// probe runs each query (panics are caught and reported) and logs the outcome.
func probe(t *testing.T, d *bsql.DB, qs ...string) {
	for _, q := range qs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Logf("%-70s => PANIC: %v", q, r)
				}
			}()
			res, err := d.Exec(q)
			t.Logf("%-70s => %s", q, show(res, err))
		}()
	}
}
