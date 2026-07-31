package bytdb

import (
	"strings"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/serr"
)

// ErrTxConflict is the kv store's optimistic-concurrency conflict,
// re-exported so embedders and the SQL/wire layers can match it with
// errors.Is without importing btypedb. It is returned by Commit (and
// the closure-running write methods) only under WithConcurrentWrites,
// and it is always retryable: another transaction touching the same
// keys committed first — re-run the transaction from the top.
var ErrTxConflict = btypedb.ErrTxConflict

// ErrText renders an error for user-facing surfaces (a REPL, a wire
// protocol, a CLI): the message followed by the error's structured
// attributes in parentheses — "wrong number of parameters (want: 1,
// got: 0)". Frame context (location, function) stays out; pass the
// error to a structured logger to capture everything.
func ErrText(err error) string {
	if err == nil {
		return ""
	}
	flds := serr.SErrFromErr(err).UserFields()
	if len(flds) < 2 {
		return err.Error()
	}
	var b strings.Builder
	b.WriteString(err.Error())
	b.WriteString(" (")
	for i := 0; i+1 < len(flds); i += 2 {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(flds[i])
		b.WriteString(": ")
		b.WriteString(flds[i+1])
	}
	b.WriteByte(')')
	return b.String()
}
