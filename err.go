package bytdb

import (
	"strings"

	"github.com/rohanthewiz/serr"
)

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
