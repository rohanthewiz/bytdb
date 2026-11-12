package bytdb

import (
	"errors"
	"slices"
	"strings"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/serr"
)

// Sentinel errors for the conditions callers branch on, matched with
// errors.Is (deep-look D-56). The errors themselves keep their messages
// — Postgres's wording, naming the table, column or constraint — and
// their serr fields; the sentinels only classify them:
//
//	_, err := db.Exec(`insert into users values (1, 'ada')`)
//	if errors.Is(err, bytdb.ErrUniqueViolation) { ... }
//
// ErrDuplicateKey is the primary-key case of ErrUniqueViolation: a
// duplicate primary key matches both.
var (
	ErrNoSuchTable     = errors.New("relation does not exist")
	ErrNoSuchColumn    = errors.New("column does not exist")
	ErrDuplicateKey    = errors.New("duplicate primary key")
	ErrUniqueViolation = errors.New("unique constraint violation")
	ErrNotNull         = errors.New("not-null constraint violation")
	ErrForeignKey      = errors.New("foreign key constraint violation")
	ErrCheck           = errors.New("check constraint violation")
	// ErrRelationExists is a CREATE (or RENAME) onto a name a relation
	// already holds. Tables, views and sequences share one namespace,
	// so any of the three collides with the others; index names are
	// scoped to their table in bytdb (Postgres scopes them to the
	// schema), so an index collides only with its table's other
	// indexes. Postgres's SQLSTATE for every case is 42P07.
	ErrRelationExists = errors.New("relation already exists")
)

// Kind returns an error whose message is msg and which errors.Is
// matches against each of kinds. The engine and the SQL layer build
// their classified errors on it (wrapped in serr, so fields and the
// raising location ride along as before); an embedder can use it to
// raise errors the same callers recognize.
func Kind(msg string, kinds ...error) error { return &kindError{msg: msg, kinds: kinds} }

type kindError struct {
	msg   string
	kinds []error
}

func (e *kindError) Error() string        { return e.msg }
func (e *kindError) Is(target error) bool { return slices.Contains(e.kinds, target) }

// KindErr is Kind as a structured error: fields attach as serr
// attributes and the recorded location is skip frames above the
// caller — 0 for the function calling KindErr, 1 for its caller (a
// constructor like the SQL layer's noSuchTable reports its own caller).
func KindErr(skip int, msg string, kinds []error, fields ...string) error {
	se := serr.SErrFromErr(Kind(msg, kinds...))
	se.AppendKeyValPairs(fields...)
	// Frames from FunctionLoc's runtime.Caller: 0 FunctionLoc, 1
	// AppendCallerContext, 2 KindErr, 3 KindErr's caller.
	se.AppendCallerContext(serr.FrameLevels.FrameLevel3 + skip)
	return se
}

// errNoSuchTable is the undefined-table error, in Postgres's wording.
func errNoSuchTable(table string) error {
	return KindErr(1, `relation "`+table+`" does not exist`, []error{ErrNoSuchTable}, "table", table)
}

// errRelationExists is the duplicate-relation error, in Postgres's
// wording: `relation "x" already exists`, whatever kind of relation
// (table, view, sequence, index) holds or wants the name. It adds no
// fields of its own, since the message already names the relation; the
// DDL entry points' wrappers still attach their usual op/table context.
func errRelationExists(name string) error {
	return KindErr(1, `relation "`+name+`" already exists`, []error{ErrRelationExists})
}

// errNoSuchColumn is the undefined-column error. With a table it names
// both, as Postgres does for a write's target columns.
func errNoSuchColumn(table, column string) error {
	return KindErr(1, ColumnMissingMsg(table, column), []error{ErrNoSuchColumn}, "table", table, "column", column)
}

// ColumnMissingMsg words a missing column as Postgres does:
// `column "c" of relation "t" does not exist`, or without a table
// `column "c" does not exist`.
func ColumnMissingMsg(table, column string) string {
	if table == "" {
		return `column "` + column + `" does not exist`
	}
	return `column "` + column + `" of relation "` + table + `" does not exist`
}

// errUniqueViolation is a write that would duplicate a unique key:
// Postgres's message, naming the constraint (the primary key's is
// table_pkey). A primary-key duplicate also matches ErrDuplicateKey.
func errUniqueViolation(table, constraint string, primary bool) error {
	kinds := []error{ErrUniqueViolation}
	if primary {
		kinds = append(kinds, ErrDuplicateKey)
	}
	return KindErr(1, `duplicate key value violates unique constraint "`+constraint+`"`, kinds,
		"table", table, "constraint", constraint)
}

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
