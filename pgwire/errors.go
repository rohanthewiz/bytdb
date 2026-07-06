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
	case strings.Contains(msg, "violates not-null constraint"),
		strings.Contains(msg, "primary key column may not be NULL"),
		strings.Contains(msg, "contains null values"):
		return "23502" // not_null_violation
	case strings.Contains(msg, "violates check constraint"),
		strings.Contains(msg, "is violated by some row"):
		return "23514" // check_violation
	case strings.Contains(msg, "constraint") && strings.Contains(msg, "already exists"):
		return "42710" // duplicate_object
	case strings.Contains(msg, "constraint") && strings.Contains(msg, "does not exist"):
		return "42704" // undefined_object
	case strings.Contains(msg, "wrong number of parameters"):
		return "08P01" // protocol_violation
	case strings.Contains(msg, "current transaction is aborted"):
		return "25P02" // in_failed_sql_transaction
	case strings.Contains(msg, "read-only transaction"):
		return "25006" // read_only_sql_transaction
	case strings.Contains(msg, "cannot run inside a transaction block"):
		return "25001" // active_sql_transaction
	case strings.Contains(msg, "can only be used in transaction blocks"):
		return "25P01" // no_active_sql_transaction
	case strings.Contains(msg, "savepoint") && strings.Contains(msg, "does not exist"):
		return "3B001" // invalid_savepoint_specification
	}
	return "XX000"
}

// noticeBody builds a NoticeResponse body for a statement warning —
// the same field format as ErrorResponse, at WARNING severity, with
// the code Postgres uses for the same notice.
func noticeBody(msg string) wbuf {
	code := "01000" // warning
	switch {
	case strings.Contains(msg, "already a transaction"):
		code = "25001" // active_sql_transaction
	case strings.Contains(msg, "no transaction"):
		code = "25P01" // no_active_sql_transaction
	}
	var b wbuf
	b.byte('S')
	b.cstr("WARNING")
	b.byte('V')
	b.cstr("WARNING")
	b.byte('C')
	b.cstr(code)
	b.byte('M')
	b.cstr(msg)
	b.byte(0)
	return b
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
