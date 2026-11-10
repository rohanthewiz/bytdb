package pgwire

// split.go: splitting a simple-Query buffer into statements. The
// simple protocol allows several ;-separated statements per message;
// the split respects the dialect's string, identifier, dollar-quote and
// comment syntax so a quoted ';' never splits. The scanner itself lives
// in the sql package (sql.SplitScript), shared with DB.ExecScript and
// the database/sql driver's multi-statement Exec, so the three can
// never disagree about where a statement ends (deep-look D-56). The
// extended protocol never reaches this code (one statement per Parse).

import bsql "github.com/rohanthewiz/bytdb/sql"

// stmtPart is one statement of a Query buffer: its text and its byte
// offset in the original buffer (for error positions).
type stmtPart struct {
	text string
	off  int
}

// splitStatements splits on top-level semicolons, dropping empty
// statements (see sql.SplitScript, including why a comment-only
// statement is empty).
func splitStatements(q string) []stmtPart {
	var parts []stmtPart
	for _, p := range bsql.SplitScript(q) {
		parts = append(parts, stmtPart{text: p.Text, off: p.Offset})
	}
	return parts
}

// isEmptyQuery reports whether q holds no statement at all: nothing but
// whitespace, comments and semicolons. Postgres answers such a query
// with EmptyQueryResponse in either protocol.
func isEmptyQuery(q string) bool { return bsql.IsEmptyScript(q) }
