package sqlexecaudit

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

// open creates a fresh engine + SQL DB in a temp dir.
func open(t testing.TB) (*bsql.DB, *bytdb.Engine) {
	t.Helper()
	e, err := bytdb.Open(filepath.Join(tempDirOf(t), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return bsql.New(e), e
}

// must runs statements that have to succeed.
func must(t testing.TB, d *bsql.DB, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// show runs a query and returns a compact rendering of rows or the error.
func show(d *bsql.DB, q string, args ...any) string {
	res, err := d.Exec(q, args...)
	if err != nil {
		return "ERR: " + err.Error()
	}
	var b strings.Builder
	for i, r := range res.Rows {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(fmt.Sprintf("%v", r))
	}
	if len(res.Rows) == 0 {
		b.WriteString(fmt.Sprintf("(0 rows, affected=%d)", res.RowsAffected))
	}
	return b.String()
}

// logq prints a query and its outcome.
func logq(t testing.TB, d *bsql.DB, q string, args ...any) string {
	out := show(d, q, args...)
	t.Logf("%s\n    => %s", q, out)
	return out
}

// tempDirOf lets the benchmark harness substitute a longer-lived dir.
func tempDirOf(t testing.TB) string { return t.TempDir() }
