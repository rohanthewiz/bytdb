package pgwire

// errors.go: rendering bytdb errors as ErrorResponse messages. The
// error's structured fields (serr UserFields) map onto the protocol's
// typed fields — a "pos" byte offset becomes Position (1-based
// character offset), the message stays Message, and the remaining
// fields join into Detail — rather than being flattened into one
// string.

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rohanthewiz/serr"
)

// sqlstate picks a five-character SQLSTATE code for an error. bytdb
// errors carry no code, so this keys off the stable message texts;
// anything unrecognized is XX000 (internal_error).
func sqlstate(msg string, hasPos bool) string {
	switch {
	case hasPos:
		return "42601" // syntax_error
	case strings.Contains(msg, "no such table"):
		return "42P01" // undefined_table
	case strings.Contains(msg, "no such column"):
		return "42703" // undefined_column
	case strings.Contains(msg, "ambiguous"):
		return "42702" // ambiguous_column
	case strings.Contains(msg, "duplicate primary key"),
		strings.Contains(msg, "unique index violation"):
		return "23505" // unique_violation
	case strings.Contains(msg, "wrong number of parameters"):
		return "08P01" // protocol_violation
	}
	return "XX000"
}

// errorBody builds an ErrorResponse body. query is the full text the
// position should index into and base the byte offset of the failing
// statement within it (0 when they are the same string).
func errorBody(err error, query string, base int) wbuf {
	msg := err.Error()
	flds := serr.SErrFromErr(err).UserFields()
	pos := -1
	var detail []string
	for i := 0; i+1 < len(flds); i += 2 {
		k, v := flds[i], flds[i+1]
		if k == "pos" {
			if p, perr := strconv.Atoi(v); perr == nil {
				pos = p
			}
			continue
		}
		detail = append(detail, k+": "+v)
	}
	var b wbuf
	b.byte('S')
	b.cstr("ERROR")
	b.byte('V')
	b.cstr("ERROR")
	b.byte('C')
	b.cstr(sqlstate(msg, pos >= 0))
	b.byte('M')
	b.cstr(msg)
	if len(detail) > 0 {
		b.byte('D')
		b.cstr(strings.Join(detail, ", "))
	}
	if pos >= 0 {
		// Byte offset within the statement -> 1-based character
		// offset within the full query buffer.
		if off := base + pos; off <= len(query) {
			b.byte('P')
			b.cstr(strconv.Itoa(utf8.RuneCountInString(query[:off]) + 1))
		}
	}
	b.byte(0)
	return b
}
