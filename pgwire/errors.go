package pgwire

// errors.go: rendering bytdb errors as ErrorResponse messages. The
// error's structured fields (serr UserFields) map onto the protocol's
// typed fields — a "pos" byte offset becomes Position (1-based
// character offset), the message stays Message, and the remaining
// fields join into Detail — rather than being flattened into one
// string.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rohanthewiz/bytdb"
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
	// Also Postgres's wording for ALTER COLUMN / ADD PRIMARY KEY naming
	// a missing column, and CREATE TABLE's key naming an undeclared one.
	// The `column "` prefix keeps a constraint's "does not exist"
	// (42704, below) out of this case.
	case strings.Contains(msg, "no such column"),
		strings.Contains(msg, "primary key column not declared"),
		strings.HasPrefix(msg, `column "`) && strings.HasSuffix(msg, `" does not exist`):
		return "42703" // undefined_column
	case strings.Contains(msg, "ambiguous"):
		return "42702" // ambiguous_column
	// A column named twice in a key, or a RENAME onto a taken name.
	// This must precede the 23505 case: "duplicate primary key column"
	// is a DDL mistake, but it contains "duplicate primary key", the
	// data-conflict wording, and used to be reported as unique_violation.
	case strings.Contains(msg, "duplicate primary key column"),
		strings.Contains(msg, "appears twice in primary key"),
		strings.HasPrefix(msg, `column "`) && strings.HasSuffix(msg, `" already exists`):
		return "42701" // duplicate_column
	case strings.Contains(msg, "duplicate primary key"),
		strings.Contains(msg, "unique index violation"),
		strings.Contains(msg, "could not create unique index"):
		return "23505" // unique_violation
	case strings.Contains(msg, "cannot be cast automatically"):
		return "42804" // datatype_mismatch
	// Every refusal here protects the one-primary-key-per-table
	// invariant. Postgres itself uses 42P16 for a second key (in CREATE
	// TABLE, ALTER TABLE ADD PRIMARY KEY, or ADD COLUMN ... PRIMARY KEY)
	// and for DROP NOT NULL on a key column. The rest exist because
	// bytdb requires a key where Postgres would allow a keyless table:
	// CREATE TABLE without one, dropping the pkey constraint, or
	// dropping a key column (Postgres would drop the key with it).
	// "multiple primary keys" matches both the parser's short form and
	// the ALTER form ("... for table "t" are not allowed").
	case strings.Contains(msg, "multiple primary keys"),
		strings.Contains(msg, "cannot add a primary key column"),
		strings.Contains(msg, "cannot drop constraint"),
		strings.Contains(msg, "cannot drop a primary key column"),
		strings.Contains(msg, "a primary key is required"),
		strings.HasSuffix(msg, `" is in a primary key`):
		return "42P16" // invalid_table_definition
	// Every per-type literal parse failure (int, float, bool, bytea,
	// date, timestamp, uuid, json) shares this wording, as in Postgres.
	case strings.Contains(msg, "invalid input syntax for type"):
		return "22P02" // invalid_text_representation
	// Bind-time parameter decoding (values.go). A malformed binary value
	// is Postgres's 22P03; a malformed text value is the same 22P02 as a
	// bad literal. The space count pins the text form to "bad <type>
	// parameter", so no other "bad ..." message can fall into it
	// ("bad parameter format code" does not end in "parameter" anyway;
	// it is a protocol violation, mapped below).
	case strings.HasPrefix(msg, "bad binary ") && strings.HasSuffix(msg, " parameter"):
		return "22P03" // invalid_binary_representation
	case strings.HasPrefix(msg, "bad ") && strings.HasSuffix(msg, " parameter") &&
		strings.Count(msg, " ") == 2:
		return "22P02" // invalid_text_representation
	case strings.Contains(msg, "violates not-null constraint"),
		strings.Contains(msg, "primary key column may not be NULL"),
		strings.Contains(msg, "contains null values"):
		return "23502" // not_null_violation
	case strings.Contains(msg, "violates foreign key constraint"):
		return "23503" // foreign_key_violation
	case strings.Contains(msg, "truncate a table referenced in a foreign key"):
		return "0A000" // feature_not_supported (Postgres's code for it sans CASCADE)
	// Schema changes refused because another object depends on the
	// target: a foreign key, a CHECK, or an index. Postgres reports its
	// own such refusals (DROP without CASCADE) as 2BP01, and the remedy
	// is the same for all of bytdb's: remove the dependent object first,
	// then retry. That holds even where Postgres would have allowed the
	// change (it tracks dependencies by oid, bytdb by name): renaming a
	// referenced table or column, changing a FK column's type, renaming
	// a column a CHECK mentions, or dropping an indexed or FK column,
	// which Postgres would cascade. 0A000 would tell a client the change
	// can never work, and it can once the dependent object is gone.
	// The wordings covered, by origin:
	//   "... because other objects depend on it"      DROP TABLE, DROP/RENAME COLUMN (CHECK)
	//   "... a foreign key depends on"                 DROP INDEX / DROP CONSTRAINT, PK replace
	//   "... referenced by a foreign key"             DROP/RENAME COLUMN, RENAME TABLE, ALTER TYPE
	//   "cannot drop|change the type of a foreign key column"
	//   "cannot drop an indexed column"
	// The truncate case above says "referenced in", not "by", so it
	// keeps Postgres's 0A000.
	case strings.Contains(msg, "because other objects depend on it"),
		strings.Contains(msg, "a foreign key depends on"),
		strings.HasPrefix(msg, "cannot ") && (strings.Contains(msg, "referenced by a foreign key") ||
			strings.Contains(msg, "a foreign key column") ||
			strings.Contains(msg, "an indexed column")):
		return "2BP01" // dependent_objects_still_exist
	case strings.Contains(msg, "violates check constraint"),
		strings.Contains(msg, "is violated by some row"):
		return "23514" // check_violation
	case strings.Contains(msg, "constraint") && strings.Contains(msg, "already exists"):
		return "42710" // duplicate_object
	case strings.Contains(msg, "constraint") && strings.Contains(msg, "does not exist"):
		return "42704" // undefined_object
	case strings.Contains(msg, "prepared statement already exists"):
		return "42P05" // duplicate_prepared_statement
	case strings.Contains(msg, "too many prepared statements"),
		strings.Contains(msg, "too many portals"):
		return "54000" // program_limit_exceeded
	// Both are malformed Bind messages rather than bad data: a
	// parameter count that disagrees with the statement, or a format
	// code other than 0 (text) / 1 (binary). Postgres reports the
	// latter as "unsupported format code" under 08P01 too. The format
	// code case cannot reach the "bad <type> parameter" rules above:
	// its message does not end in "parameter".
	case strings.Contains(msg, "wrong number of parameters"),
		strings.Contains(msg, "bad parameter format code"):
		return "08P01" // protocol_violation
	// A negative bound count surfaces at execution (a negative literal
	// is caught at parse and reports 42601 like any syntax error).
	case strings.Contains(msg, "LIMIT must not be negative"):
		return "2201W" // invalid_row_count_in_limit_clause
	case strings.Contains(msg, "OFFSET must not be negative"):
		return "2201X" // invalid_row_count_in_result_offset_clause
	case strings.Contains(msg, "current transaction is aborted"):
		return "25P02" // in_failed_sql_transaction
	case strings.Contains(msg, "read-only transaction"):
		return "25006" // read_only_sql_transaction
	case strings.Contains(msg, "cannot run inside a transaction block"),
		strings.Contains(msg, "must be called before any query"):
		return "25001" // active_sql_transaction (late SET TRANSACTION)
	case strings.Contains(msg, "can only be used in transaction blocks"):
		return "25P01" // no_active_sql_transaction
	case strings.Contains(msg, "savepoint") && strings.Contains(msg, "does not exist"):
		return "3B001" // invalid_savepoint_specification
	case strings.Contains(msg, "not yet defined in this session"):
		return "55000" // object_not_in_prerequisite_state (lastval/currval)
	}
	return "XX000"
}

// cancelBody builds the ErrorResponse for a canceled statement:
// SQLSTATE 57014 (query_canceled) at ordinary ERROR severity — the
// connection survives; only the statement died.
func cancelBody(msg string) wbuf {
	var b wbuf
	b.byte('S')
	b.cstr("ERROR")
	b.byte('V')
	b.cstr("ERROR")
	b.byte('C')
	b.cstr("57014")
	b.byte('M')
	b.cstr(msg)
	b.byte(0)
	return b
}

// fatalBody builds an ErrorResponse body at FATAL severity — the
// severity Postgres uses when the server, not the statement, is about
// to end the connection (e.g. the idle-in-transaction timeout).
func fatalBody(msg, code string) wbuf {
	var b wbuf
	b.byte('S')
	b.cstr("FATAL")
	b.byte('V')
	b.cstr("FATAL")
	b.byte('C')
	b.cstr(code)
	b.byte('M')
	b.cstr(msg)
	b.byte(0)
	return b
}

// conflictBody builds the ErrorResponse for an optimistic-concurrency
// loss: SQLSTATE 40001 (serialization_failure) with Postgres's exact
// message and retry hint — connection pools and ORM retry middleware
// pattern-match all three. Reaching here means the server has already
// spent its own retries where it safely could (autocommit statements);
// what remains is genuinely the client's to re-run.
func conflictBody() wbuf {
	var b wbuf
	b.byte('S')
	b.cstr("ERROR")
	b.byte('V')
	b.cstr("ERROR")
	b.byte('C')
	b.cstr("40001")
	b.byte('M')
	b.cstr("could not serialize access due to concurrent update")
	b.byte('H')
	b.cstr("The transaction might succeed if retried.")
	b.byte(0)
	return b
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
	case strings.Contains(msg, "can only be used in transaction blocks"):
		code = "25P01" // no_active_sql_transaction (stray SET TRANSACTION)
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
	// A canceled or timed-out statement gets Postgres's exact 57014
	// wording rather than the internal error chain: clients and
	// connection pools pattern-match both the code and the message.
	if errors.Is(err, context.DeadlineExceeded) {
		return cancelBody("canceling statement due to statement timeout")
	}
	if errors.Is(err, context.Canceled) {
		return cancelBody("canceling statement due to user request")
	}
	// An optimistic-concurrency loss likewise gets Postgres's canonical
	// rendering rather than the engine's error text.
	if errors.Is(err, bytdb.ErrTxConflict) {
		return conflictBody()
	}
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
