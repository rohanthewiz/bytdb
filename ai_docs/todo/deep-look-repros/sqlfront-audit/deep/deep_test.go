package deep

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

// A flat (paren-free) operator chain parses iteratively, so maxParseDepth never
// trips, yet it builds a left-nested AST as deep as the chain is long. Every
// later walk (bind, describe, eval) recurses once per term.
func TestDeepChain(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("DEEP_N"))
	kind := os.Getenv("DEEP_KIND")
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	d := bsql.New(e)
	var q string
	switch kind {
	case "plus":
		q = "select 1" + strings.Repeat("+1", n)
	case "cast":
		q = "select 1" + strings.Repeat("::int", n)
	case "concat":
		q = "select 'a'" + strings.Repeat("||'a'", n)
	case "minus":
		q = "select 1" + strings.Repeat("-1", n)
	case "isnull":
		q = "select 1" + strings.Repeat(" is null", n)
	}
	t.Logf("query bytes: %d", len(q))
	res, err := d.Exec(q)
	if err != nil {
		t.Logf("err: %.200v", err)
		return
	}
	t.Logf("ok: %.60v", res.Rows)
}
