// Package sql is bytdb's SQL frontend: a small Postgres-flavored
// dialect parsed, planned, and executed directly over an Engine.
//
// Supported statements:
//
//	CREATE TABLE t (id int PRIMARY KEY, name text NOT NULL,
//	       price int CHECK (price > 0), ...)
//	CREATE TABLE t (a int, b int, ..., PRIMARY KEY (a, b),
//	       [CONSTRAINT name] CHECK (expr), ...)
//	DROP TABLE t
//	ALTER TABLE t ADD [COLUMN] c type [NOT NULL]
//	ALTER TABLE t DROP [COLUMN] c
//	CREATE [UNIQUE] INDEX idx ON t (c [ASC|DESC], ...)
//	DROP INDEX idx [ON t]
//	EXPLAIN statement
//	INSERT INTO t [(c, ...)] VALUES (v, ...), ...
//	SELECT * | items FROM tables [WHERE ...] [GROUP BY c, ...] [HAVING ...]
//	       [ORDER BY item [ASC|DESC], ...] [LIMIT n] [OFFSET n]
//	UPDATE t SET c = v, ... [WHERE ...]
//	DELETE FROM t [WHERE ...]
//	BEGIN | START TRANSACTION ... COMMIT | END | ROLLBACK | ABORT
//	SAVEPOINT name | RELEASE [SAVEPOINT] name | ROLLBACK TO [SAVEPOINT] name
//
// FROM names one table or a left-deep chain of joins:
//
//	FROM a [AS] x [INNER] JOIN b ON x.id = b.a_id
//	       LEFT [OUTER] JOIN c ON ... CROSS JOIN d
//
// A comma-separated table is a cross join; RIGHT and FULL joins are
// not supported. Columns may be qualified (t.c, using the alias when
// one is given); unqualified names must be unambiguous across the
// FROM tables. Select lists also accept t.* . Joins execute as nested
// loops, but ON and WHERE equality conjuncts re-bind per outer row,
// so an inner table joined on its primary key or an indexed column is
// a point get or bounded scan per row, not a full scan. A LEFT JOIN
// extends unmatched left rows with NULLs; WHERE predicates on its
// right table apply after that extension (so o.id IS NULL is the
// anti-join).
//
// WHERE, ON, and HAVING are boolean expressions — conditions combined
// with AND, OR, and NOT (standard precedence; parentheses group),
// evaluated with SQL three-valued logic: a comparison against NULL is
// unknown, and only definitely-true rows match. Comparisons are =,
// !=, <>, <, <=, >, >= and the regex operators ~, !~, ~*, !~* (Go
// regexp syntax), in either operand order, plus IS [NOT] NULL, [NOT]
// IN (list), and EXISTS (SELECT ...); in HAVING an operand may be an
// aggregate call. The planner turns equality and range predicates
// that are top-level AND conjuncts on a prefix of the primary key or
// of a secondary index into point gets or bounded ordered scans
// (anything under OR or NOT, and every non-simple expression, stays
// filter-only); the whole condition is still re-checked per row, so
// pushdown only narrows what is visited.
//
// Beyond plain columns, select items and conditions take a general
// expression language: CASE (both forms), casts with :: (integer and
// oid families, text, bool, float, and the reg* types — 'users'::
// regclass resolves through the catalog), arithmetic and || concat,
// a whitelist of functions (coalesce, nullif, upper, lower, length,
// and the pg_catalog introspection set: format_type, pg_get_indexdef,
// pg_get_userbyid, pg_table_is_visible, ...), scalar subqueries —
// correlated ones resolve outer columns through the enclosing scopes
// — including single-aggregate forms like (SELECT count(*) ... WHERE
// x = outer.y), EXISTS, and ARRAY(SELECT ...) rendered as Postgres
// array text. Select items take [AS] output aliases, and ORDER BY
// resolves bare names against them. Unknown functions parse and only
// error if a row actually evaluates them, which is what lets psql's
// catalog queries (with their generate_series/string_agg/array-
// subscript corners) run against empty catalog tables. SELECT cores
// combine with UNION [ALL]; ORDER BY, LIMIT, and OFFSET then apply to
// the combined rows by select-list position or output name.
//
// Aggregates are COUNT(*), COUNT(x), SUM(x), AVG(x), MIN(x), MAX(x),
// where x is a column or any expression evaluated per input row
// (SUM(a * b)); aggregate calls cannot nest. Any aggregate, GROUP BY,
// or HAVING makes the query aggregate rows, with SQL semantics:
// aggregates ignore NULL inputs (COUNT(*) counts rows), NULL group
// values form one group, an ungrouped aggregate query returns exactly
// one row, and HAVING filters groups. A GROUP BY key is a column, an
// expression (GROUP BY age / 10), or an integer ordinal naming a
// select-list position (GROUP BY 1). Select items, HAVING, and ORDER
// BY take expressions over grouped data — a subtree matching a GROUP
// BY key reads the group's key value, aggregates read their
// accumulated results, and any other column reference is the classic
// "must appear in the GROUP BY clause" error. Without ORDER BY,
// groups return in ascending group-key order.
//
// The dialect follows Postgres conventions: 'string' literals with ”
// escapes, "quoted" identifiers, unquoted identifiers folded to
// lowercase, -- and /* */ comments, and type aliases (int, integer,
// bigint, int8...; float, float8, real, double precision; text,
// varchar[(n)], string; bool, boolean; bytea, bytes). As in Postgres,
// a quoted literal is untyped until context types it: '2' compares,
// inserts, and assigns as an integer against an int column (and
// likewise for float, bool, and bytea columns), erroring when the
// text does not parse as the column's type.
//
// For introspection, the virtual system catalog serves
// pg_catalog.pg_namespace, pg_class, pg_attribute, pg_type, pg_index,
// pg_am, pg_database, and pg_roles with real rows, a set of
// always-empty tables psql probes (pg_attrdef, pg_collation,
// pg_constraint, pg_inherits, pg_policy, pg_statistic_ext, the
// pg_publication family, pg_auth_members), plus
// information_schema.tables and columns — all synthesized from the
// engine catalog and queryable like any tables (read-only; their
// names are reserved). psql's \dt, \d, \d <table>, \d <index>, \di,
// \dn, \du, and \l render against it. Table names may be
// schema-qualified — public.t is t; pg_catalog members also resolve
// bare, as on Postgres's search path. SELECT works without FROM over
// literal select items (SELECT 1), a whitelist of zero-argument
// functions folds to constants wherever a literal fits (version(),
// current_database(), current_schema(), current_user(),
// session_user(), optionally pg_catalog-qualified), and ORDER BY
// takes select-list positions (ORDER BY 1, 2).
//
// Constraints: a NOT NULL column rejects NULL on insert and update
// (and may be added by ALTER TABLE only while the table is empty). A
// CHECK constraint — column-level or table-level, optionally named
// with CONSTRAINT — is any row-level boolean expression over the
// table's columns (no aggregates, subqueries, or placeholders),
// stored as text in the table descriptor and re-checked on every
// INSERT and UPDATE; a row is rejected only when a check is
// definitely false, so NULLs pass, per SQL. Errors carry Postgres's
// wording ("violates not-null constraint", "violates check
// constraint") and SQLSTATEs (23502, 23514) over the wire, checks
// appear in pg_constraint and pg_get_constraintdef (psql's \d shows
// them), and a column a check mentions cannot be dropped. DEFAULT,
// UNIQUE column constraints, and REFERENCES are rejected at parse.
//
// A CREATE INDEX key column may be DESC: the index stores that
// column's keys byte-inverted, so scans (and the rows a bounded scan
// visits) run high-to-low, with NULLs last — the mirror of ascending.
// The planner pushes range predicates on a descending column by
// swapping which side starts the scan and which stops it.
//
// EXPLAIN renders the plan the executor would run — Point Get, Index
// Scan (with Index Cond vs Filter), Seq Scan, Nested Loop, Aggregate,
// Sort, Limit, Append — as Postgres-shaped text, one row per line. No
// costs are shown (bytdb has no cost model), and EXPLAIN ANALYZE is
// rejected rather than pretending to instrument execution.
//
// Statements may use $1-style placeholders wherever a literal may
// appear: comparison values in WHERE, ON, and HAVING, INSERT values,
// and UPDATE SET values (LIMIT and OFFSET take literal counts only).
// Exec binds its trailing arguments to them, and Prepare parses a
// statement once for repeated execution. Parameters are numbered: a
// statement takes exactly as many values as its highest $n, and the
// same $n may be used more than once. Arguments are Go values —
// int64, float64, string, bool, []byte, or nil for NULL, with other
// integer and float types converted.
//
// Each statement is atomic: a multi-row INSERT, an UPDATE, or a DELETE
// runs in one engine transaction and rolls back entirely on error.
// For multi-statement transactions, a Session executes BEGIN ...
// COMMIT | ROLLBACK blocks with Postgres semantics: the block is one
// engine transaction; an error inside it fails the block, refusing
// everything but ROLLBACK (COMMIT then rolls back, reporting so in
// its tag); redundant control statements warn and do nothing.
// SAVEPOINT marks a point inside the block — an O(1) copy-on-write
// snapshot — that ROLLBACK TO rewinds to, clearing the failed state,
// so a block can recover from an error instead of losing everything;
// RELEASE drops the mark and keeps the work. Names may repeat
// (references resolve to the most recent), and rewinding or releasing
// destroys the savepoints made after the one named, as in Postgres.
// BEGIN
// accepts the standard transaction modes — isolation levels parse and
// are ignored (the engine's single-writer transactions are
// serializable, which satisfies them all) and READ ONLY is honored. A
// writable block holds the engine's single-writer lock from BEGIN to
// its end, so writes in other sessions wait behind it; reads never
// do. DDL cannot run inside a block, because the engine gives every
// schema change its own transaction.
package sql

import (
	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// DB executes SQL statements against a bytdb Engine. Each statement
// is its own transaction; for BEGIN/COMMIT transaction blocks, wrap
// it in a Session.
type DB struct {
	e *bytdb.Engine

	// tx, when set, is a Session's open transaction: every statement
	// runs inside it instead of opening its own.
	tx *bytdb.Txn
}

// New wraps an Engine for SQL execution.
func New(e *bytdb.Engine) *DB { return &DB{e: e} }

// read runs fn over the open session transaction, or a fresh
// read-only snapshot in autocommit.
func (d *DB) read(fn func(*bytdb.Txn) error) error {
	if d.tx != nil {
		return fn(d.tx)
	}
	return d.e.ReadTxn(fn)
}

// write runs fn in the open session transaction — staged writes
// commit or roll back with the block, so fn's error must abort the
// session (the Session does) — or in its own transaction in
// autocommit.
func (d *DB) write(fn func(*bytdb.Txn) error) error {
	if d.tx != nil {
		return fn(d.tx)
	}
	return d.e.WriteTxn(fn)
}

// Result is the outcome of one statement. SELECT fills Cols, Types,
// and Rows; INSERT, UPDATE, and DELETE fill RowsAffected; DDL fills
// nothing.
type Result struct {
	Cols         []string
	Types        []bytdb.ColType
	Rows         [][]any
	RowsAffected int

	// Tag, when set, overrides the statement's command tag: COMMIT of
	// a failed transaction reports ROLLBACK, as in Postgres.
	Tag string
	// Notice is a warning the statement raised without failing:
	// BEGIN inside a transaction block, COMMIT or ROLLBACK outside
	// one. Wire servers forward it as a NoticeResponse.
	Notice string
}

// Exec parses and executes one SQL statement, binding args to its
// $1-style placeholders (the statement's highest $n and len(args)
// must agree).
func (d *DB) Exec(query string, args ...any) (*Result, error) {
	st, err := Parse(query)
	if err != nil {
		return nil, err
	}
	return d.run(st, args)
}

// Prepare parses a statement once for repeated execution with
// per-call parameter values.
func (d *DB) Prepare(query string) (*Stmt, error) {
	st, err := Parse(query)
	if err != nil {
		return nil, err
	}
	return &Stmt{db: d, st: st, n: numParams(st)}, nil
}

// Stmt is a prepared statement. Execution binds parameters into a
// copy of the parsed form, so a Stmt may be executed any number of
// times and is safe for concurrent use.
type Stmt struct {
	db *DB
	st Statement
	n  int
}

// NumParams is the number of values Exec expects: the statement's
// highest $n.
func (s *Stmt) NumParams() int { return s.n }

// Exec executes the prepared statement with args bound to its
// placeholders.
func (s *Stmt) Exec(args ...any) (*Result, error) { return s.db.run(s.st, args) }

// run binds args into st, adapts quoted literals to their column
// types, and dispatches it.
func (d *DB) run(st Statement, args []any) (*Result, error) {
	st, err := bindParams(st, args)
	if err != nil {
		return nil, err
	}
	if st, err = d.coerceLiterals(st); err != nil {
		return nil, err
	}
	if err := sysWriteGuard(writeTarget(st)); err != nil {
		return nil, err
	}
	switch s := st.(type) {
	case *CreateTable:
		cols := make([]bytdb.Column, len(s.Cols))
		for i, c := range s.Cols {
			cols[i] = bytdb.Column{Name: c.Name, Type: c.Type, NotNull: c.NotNull}
		}
		checks, err := resolveChecks(s, cols)
		if err != nil {
			return nil, err
		}
		if _, err := d.e.CreateTableWithChecks(s.Table, cols, checks, s.PK...); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *DropTable:
		if err := d.e.DropTable(s.Table); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *AddColumn:
		col := bytdb.Column{Name: s.Col.Name, Type: s.Col.Type, NotNull: s.Col.NotNull}
		if err := d.e.AddColumn(s.Table, col); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *DropColumn:
		if err := d.checkDropColumn(s.Table, s.Col); err != nil {
			return nil, err
		}
		if err := d.e.DropColumn(s.Table, s.Col); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *CreateIndex:
		keys := make([]bytdb.IndexCol, len(s.Cols))
		for i, c := range s.Cols {
			keys[i] = bytdb.IndexCol{Name: c, Desc: i < len(s.Desc) && s.Desc[i]}
		}
		if _, err := d.e.CreateIndexCols(s.Table, s.Name, s.Unique, keys); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *DropIndex:
		return d.execDropIndex(s)
	case *Insert:
		return d.execInsert(s)
	case *Select:
		if len(s.Union) > 0 {
			return d.execUnion(s)
		}
		return d.runSelectCore(s)
	case *Update:
		return d.execUpdate(s)
	case *Delete:
		return d.execDelete(s)
	case *Explain:
		return d.execExplain(s)
	case *TxnControl:
		// Sessions intercept these before run; a bare DB has no
		// transaction state to control.
		return nil, serr.New("transaction control statements require a Session",
			"statement", s.Tag)
	}
	return nil, serr.New("unhandled statement type")
}
