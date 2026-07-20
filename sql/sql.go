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
//	ALTER TABLE t ADD [CONSTRAINT name] CHECK (expr)
//	ALTER TABLE t DROP CONSTRAINT [IF EXISTS] name
//	ALTER TABLE t OWNER TO role       (accepted and ignored; no roles)
//	CREATE [UNIQUE] INDEX idx ON t (c [ASC|DESC], ...)
//	DROP INDEX idx [ON t]
//	CREATE SEQUENCE [IF NOT EXISTS] s [AS type] [INCREMENT [BY] n]
//	       [MINVALUE n | NO MINVALUE] [MAXVALUE n | NO MAXVALUE]
//	       [START [WITH] n] [CACHE n] [CYCLE | NO CYCLE] [OWNED BY NONE]
//	ALTER SEQUENCE s options... [RESTART [[WITH] n]]
//	DROP SEQUENCE [IF EXISTS] s [RESTRICT]
//	EXPLAIN statement
//	INSERT INTO t [(c, ...)] VALUES (v, ...), ...
//	       [ON CONFLICT [(c, ...)] DO NOTHING |
//	        ON CONFLICT (c, ...) DO UPDATE SET c = expr, ... [WHERE ...]]
//	       [RETURNING ...]
//	[WITH name [(cols)] AS (SELECT ...), ...]
//	SELECT * | items FROM tables [WHERE ...] [GROUP BY c, ...] [HAVING ...]
//	       [ORDER BY item [ASC|DESC], ...] [LIMIT n] [OFFSET n]
//	CREATE [OR REPLACE] VIEW v AS SELECT ... | DROP VIEW [IF EXISTS] v
//	UPDATE t SET c = v, ... [WHERE ...] [RETURNING ...]
//	DELETE FROM t [WHERE ...] [RETURNING ...]
//	TRUNCATE [TABLE] t [, ...] [RESTART IDENTITY | CONTINUE IDENTITY]
//	SET [SESSION|LOCAL] name {=|TO} value | RESET name | RESET ALL
//	SHOW name | SHOW ALL
//	BEGIN | START TRANSACTION ... COMMIT | END | ROLLBACK | ABORT
//	SAVEPOINT name | RELEASE [SAVEPOINT] name | ROLLBACK TO [SAVEPOINT] name
//
// FROM names one table or a left-deep chain of joins:
//
//	FROM a [AS] x [INNER] JOIN b ON x.id = b.a_id
//	       LEFT [OUTER] JOIN c ON ... CROSS JOIN d
//
// A FROM item may also be a derived table — (SELECT ...) alias, the
// alias mandatory — which materializes once and joins like any table.
// WITH (non-recursive) names such subqueries: each CTE materializes
// once, before the body, may use the CTEs before it, shadows a real
// table of its name, and is visible everywhere in the statement,
// scalar subqueries and union arms included; WITH x(a, b) renames its
// output columns. IN (SELECT ...) works, lowered to = ANY. Views are
// the persistent flavor: CREATE [OR REPLACE] VIEW stores the SELECT's
// text (validated by running it once), any statement naming the view
// materializes it at that moment, views may reference other views
// (cycles are caught), and the relation namespace is shared with
// tables and sequences. Views list in pg_class as relkind 'v' with
// pg_get_viewdef; derived tables, CTEs, and views all join through
// the virtual-table machinery, so ON/WHERE filter them but index
// pushdown does not apply inside a materialized result.
//
// A comma-separated table is a cross join; RIGHT and FULL joins are
// not supported. Columns may be qualified (t.c, using the alias when
// one is given); unqualified names must be unambiguous across the
// FROM tables. Select lists also accept t.* . Joins execute as nested
// loops, but ON and WHERE equality conjuncts re-bind per outer row,
// so an inner table joined on its primary key or an indexed column is
// a point get or bounded scan per row, not a full scan. When no index
// can serve an equijoin — including every join against a CTE, derived
// table, or view — the step becomes a hash join instead: the inner
// side is scanned once into a hash table and each outer row probes it,
// so unindexed equijoins are linear, not quadratic (EXPLAIN shows Hash
// Join with its Hash Cond). A LEFT JOIN
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
// IN (list), [NOT] BETWEEN [SYMMETRIC] — sugar for the comparisons it
// is defined as, usable anywhere they are, CHECK constraints included
// — and EXISTS (SELECT ...); in HAVING an operand may be an
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
// (SUM(a * b)); aggregate calls cannot nest. The argument may be
// DISTINCT x (COUNT(DISTINCT city)): the aggregate then consumes each
// distinct non-NULL value once per group, deduplicated by the same
// equivalence GROUP BY uses. Any aggregate, GROUP BY,
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
// varchar[(n)], character varying[(n)], string; bool, boolean; bytea,
// bytes; timestamp[tz] [with|without time zone], date, uuid). The
// date/time types store UTC instants — timestamps as int64
// microseconds and dates as days since the Unix epoch (that is what
// embedded queries return) — parse the ISO text forms as literals,
// order chronologically in keys and indexes, and present as
// timestamptz/date over the wire; now(), CURRENT_TIMESTAMP,
// current_date, and gen_random_uuid() evaluate per call (now() is
// not transaction-frozen as in Postgres). UUIDs are 16-byte values
// written as the usual dashed hex. A varchar(n) limit is enforced on
// every insert and update —
// overflow errors with Postgres's wording (22001 over the wire), and
// overflow that is entirely spaces truncates silently, per the SQL
// standard. As in Postgres,
// a quoted literal is untyped until context types it: '2' compares,
// inserts, and assigns as an integer against an int column (and
// likewise for float, bool, and bytea columns), erroring when the
// text does not parse as the column's type.
//
// For introspection, the virtual system catalog serves
// pg_catalog.pg_namespace, pg_class (sequences included, relkind 'S'),
// pg_attribute, pg_attrdef (declared column defaults; identity columns
// report via attidentity instead), pg_type, pg_index, pg_sequence,
// pg_am, pg_database, and pg_roles with real rows, a set of
// always-empty tables psql probes (pg_collation, pg_inherits,
// pg_policy, pg_statistic_ext, the pg_publication family,
// pg_auth_members; pg_constraint lists CHECK constraints), plus
// information_schema.tables, columns, and sequences — all synthesized
// from the engine catalog and queryable like any tables (read-only;
// their names are reserved). psql's \dt, \d, \d <table>, \d <index>, \di,
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
// them), and a column a check mentions cannot be dropped (drop the
// constraint first). ALTER TABLE ADD [CONSTRAINT name] CHECK installs
// a check on an existing table after verifying every existing row
// satisfies it — in the transaction that publishes it, so no write
// slips in between — and ALTER TABLE DROP CONSTRAINT removes one by
// name (checks and foreign keys: the primary key is structural, and
// unique constraints are indexes here). A UNIQUE constraint — on a
// column or table-level over a column list — is sugar for CREATE
// UNIQUE INDEX, creating an index named t_cols_key that DROP INDEX
// removes.
//
// Foreign keys: column-level REFERENCES parent [(col)] and
// table-level [CONSTRAINT name] FOREIGN KEY (cols) REFERENCES parent
// [(cols)], plus ALTER TABLE ADD the same (existing rows validated in
// the publishing transaction) and DROP CONSTRAINT. The referenced
// columns must be the parent's primary key (the default when the list
// is omitted) or a unique index's columns. Semantics are MATCH SIMPLE:
// a child INSERT/UPDATE requires the referenced parent row to exist
// (any NULL FK column satisfies the constraint), and a parent
// DELETE/UPDATE is refused while child rows reference the old key —
// checked after the statement's writes, so deleting a parent together
// with its children in one statement is legal. ON DELETE CASCADE is
// the one supported referential action: the parent DELETE removes
// referencing rows transitively instead of refusing (cascaded rows do
// not count toward RowsAffected or RETURNING). ON UPDATE CASCADE and
// SET NULL/DEFAULT stay rejected at parse.
// Violations carry Postgres's wording and SQLSTATE 23503. The schema
// side is guarded too: a referenced table cannot be dropped or
// truncated alone, and columns or unique indexes a constraint depends
// on cannot be dropped while it stands.
//
// TRUNCATE empties one or more tables (rows and index entries) in a
// single atomic statement without touching the schema; it is DML —
// unlike bytdb DDL it runs inside transaction blocks and rolls back
// with them. RESTART IDENTITY resets the tables' identity counters;
// CASCADE is rejected. SHOW reads configuration parameters: a
// Session's SET values overlay built-in defaults (server_version,
// timezone, transaction_isolation, ...), SHOW ALL lists them, and the
// multi-word forms clients probe (SHOW TIME ZONE, SHOW TRANSACTION
// ISOLATION LEVEL) parse.
//
// A column may declare DEFAULT <constant> — a literal of the column's
// type (quoted literals adapt), stored as text in the descriptor. A
// column-list INSERT fills omitted columns with their defaults, the
// DEFAULT keyword works as a VALUES entry, and INSERT ... DEFAULT
// VALUES inserts a whole row of them; an explicit NULL still inserts
// NULL (DEFAULT NULL declares the absence of a default, as it does in
// Postgres). Beyond constants, exactly the clock functions are
// accepted — DEFAULT now() (and its CURRENT_TIMESTAMP-family
// spellings, all normalized to now()) and DEFAULT current_date, on
// timestamp/date columns — evaluated once per INSERT statement, so a
// multi-row insert stamps every row with the same instant. General
// expressions stay rejected: a default is a stored constant or a
// clock marker, never an expression tree. information_schema.columns
// reports the stored text in column_default. ALTER TABLE ADD COLUMN
// with a DEFAULT is allowed only while the table is empty — Postgres
// backfills existing rows; this engine leaves rows untouched, and the
// two are only equivalent when there are none.
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
// UPDATE and ON CONFLICT DO UPDATE SET values, RETURNING
// expressions, and LIMIT/OFFSET counts (bound as a non-negative
// integer; NULL means no limit / zero offset, as in Postgres).
// Exec binds its trailing arguments to them, and Prepare parses a
// statement once for repeated execution. Parameters are numbered: a
// statement takes exactly as many values as its highest $n, and the
// same $n may be used more than once. Arguments are Go values —
// int64, float64, string, bool, []byte, or nil for NULL, with other
// integer and float types converted.
//
// INSERT, UPDATE, and DELETE take an optional RETURNING clause — a
// select list (expressions, aliases, *, t.*) evaluated once per
// affected row and returned like a SELECT's rows, alongside the
// affected count. INSERT and UPDATE report each row as stored — an
// identity column's drawn value, coerced values, SET applied — and
// DELETE reports the row as it was before its removal; rows come back
// in the statement's processing order (insertion order for INSERT,
// scan order otherwise). Aggregates and window functions are rejected
// in the clause, as in Postgres: there is no row set to fold — each
// affected row yields exactly one output row.
//
// Standalone sequences are first-class: CREATE SEQUENCE with the
// Postgres option set (AS smallint|integer|bigint bounds the range;
// INCREMENT, MINVALUE/MAXVALUE, START, CYCLE, CACHE — cache is stored
// and reported but allocation behaves as CACHE 1), ALTER SEQUENCE
// (options plus RESTART [[WITH] n]), and DROP SEQUENCE. nextval('s')
// and setval('s', v [, is_called]) work wherever expressions evaluate
// — a SELECT calling them runs in a writable transaction under the
// covers — and accept 's'::regclass. Sequences appear in pg_class
// (relkind 'S'), pg_sequence, and information_schema.sequences, and
// each reads as a one-row state relation (SELECT last_value FROM s).
// Unlike Postgres, allocation is transactional: a rolled-back
// nextval's value is handed out again. INSERT VALUES takes literals,
// so draw the key first (SELECT nextval) and insert it — the pattern
// drivers use.
//
// lastval() and currval('t_col_seq') read back the session's draws —
// identity-column draws under the implied t_col_seq name, and real
// sequences under their own (setval repositions currval too) — for
// drivers that probe instead of using RETURNING, with Postgres's "not
// yet defined in this session" error (SQLSTATE 55000) before the
// first draw. The state is per Session (a bare DB is one shared
// session) and survives a rolled-back block, as in Postgres —
// the readback state is session-local, not transactional.
//
// INSERT takes an optional ON CONFLICT clause with Postgres upsert
// semantics. The conflict target names the primary key's or a unique
// index's columns (inferred set-wise, order-insensitive); DO NOTHING
// may omit it to absorb a collision on any of them. DO NOTHING drops
// the conflicting proposal silently. DO UPDATE applies its SET to the
// existing row instead: unqualified and table-qualified references
// read that row, excluded.col reads the proposed one, and an optional
// WHERE limits which conflicting pairs update — a filtered pair is
// skipped entirely. Only rows actually inserted or updated count in
// the result (and feed RETURNING). A NULL in a key position never
// conflicts, matching unique-index semantics; a collision on a
// constraint other than the named target stays an error; and, as in
// Postgres, DO UPDATE reaching the same row twice in one statement —
// including a row the statement itself inserted — is an error rather
// than a silent double update. ON CONFLICT ON CONSTRAINT and partial-
// index predicates are not supported (name the columns instead).
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
	"context"
	"strings"

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

	// seq is the sequence-function state lastval()/currval() read.
	// Sessions each carry their own; a bare DB is one shared session.
	seq *seqSession

	// ctx, when set, is the running statement's cancellation scope
	// (ExecCtx, a Session statement timeout, a wire CancelRequest).
	// Row pumps poll it and abort the statement with its error. It
	// rides the same copy-the-DB convention as tx: each ...Ctx entry
	// point stamps it on a copy, so a DB value is never mutated while
	// another statement runs.
	ctx context.Context

	// vtabs are the statement's materialized virtual tables — CTEs,
	// derived tables, and views the statement references — layered over
	// the catalog in lookup. Rides the copy-the-DB convention: run
	// stamps it on a copy, so nothing leaks between statements.
	vtabs map[string]vtab

	// activity, when set, feeds pg_catalog.pg_stat_activity: a wire
	// server installs it (SetActivityProvider) so sessions can see the
	// other backends. Copied along with the DB into every session.
	activity func() []Activity
}

// Activity is one backend's pg_stat_activity row, reported by the
// serving layer (which is the only party that knows connections).
type Activity struct {
	PID        int32
	User       string
	AppName    string
	ClientAddr string
	State      string // active | idle | idle in transaction [(aborted)]
	Query      string // current statement, or the last one when idle
}

// SetActivityProvider installs the pg_stat_activity source. Call
// before sessions are created — they copy the DB, provider included.
// The provider is called from whatever session queries the table, so
// it must be safe for concurrent use.
func (d *DB) SetActivityProvider(f func() []Activity) { d.activity = f }

// vtab is one materialized virtual table: a synthetic descriptor plus
// its rows, exactly the shape the system catalog serves its tables in.
type vtab struct {
	desc *bytdb.TableDesc
	rows [][]any
}

// New wraps an Engine for SQL execution.
func New(e *bytdb.Engine) *DB { return &DB{e: e, seq: &seqSession{}} }

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

// ExecCtx is Exec bounded by ctx: cancellation or deadline expiry
// aborts the statement mid-execution with ctx's error (row pumps poll
// it, so a full scan, join, or long write stops within a few hundred
// rows). A statement aborted inside a transaction block fails the
// block like any other error.
func (d *DB) ExecCtx(ctx context.Context, query string, args ...any) (*Result, error) {
	st, err := Parse(query)
	if err != nil {
		return nil, err
	}
	return d.runCtx(ctx, st, args)
}

// runCtx runs st with ctx as its cancellation scope, on a copy so the
// shared DB value never carries one statement's context into another.
func (d *DB) runCtx(ctx context.Context, st Statement, args []any) (*Result, error) {
	dw := *d
	dw.ctx = ctx
	return dw.run(st, args)
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

// ExecCtx executes the prepared statement bounded by ctx; see
// DB.ExecCtx for the cancellation semantics.
func (s *Stmt) ExecCtx(ctx context.Context, args ...any) (*Result, error) {
	return s.db.runCtx(ctx, s.st, args)
}

// toEngineColumn maps a parsed column definition onto the engine's,
// validating a DEFAULT against the column type (a quoted literal
// adapts, Postgres-style) and rendering it to the literal text the
// descriptor stores. An ExprDefault marker stores its own text
// unquoted — distinguishable from every constant, whose string form
// renderLit always quotes — after checking the column can hold what
// the clock function yields (both markers evaluate through the
// time.Time arm of coerceLit, which fills timestamp or date columns
// and nothing else).
func toEngineColumn(c ColDef) (bytdb.Column, error) {
	col := bytdb.Column{Name: c.Name, Type: c.Type, NotNull: c.NotNull,
		Identity: c.Identity, MaxLen: c.MaxLen}
	if c.HasDefault {
		if ed, ok := c.Default.(ExprDefault); ok {
			if c.Type != bytdb.TTimestamp && c.Type != bytdb.TDate {
				return bytdb.Column{}, serr.New(
					"DEFAULT "+string(ed)+" requires a timestamp or date column",
					"column", c.Name)
			}
			col.Default = string(ed)
			return col, nil
		}
		cv, err := coerceLit(c.Default, c.Type)
		if err != nil {
			return bytdb.Column{}, serr.Wrap(err, "clause", "DEFAULT", "column", c.Name)
		}
		col.Default = renderLit(cv)
	}
	return col, nil
}

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
			col, err := toEngineColumn(c)
			if err != nil {
				return nil, err
			}
			cols[i] = col
		}
		checks, err := resolveChecks(s, cols)
		if err != nil {
			return nil, err
		}
		desc, err := d.e.CreateTableWithChecks(s.Table, cols, checks, s.PK...)
		if err != nil {
			return nil, err
		}
		// UNIQUE constraints lower to unique indexes, created right after
		// the table. Engine DDL is one transaction per change, so a failed
		// index (a bad column name, a duplicate) un-creates the table to
		// keep CREATE TABLE all-or-nothing from the caller's view.
		for _, ucols := range s.Uniques {
			keys := make([]bytdb.IndexCol, len(ucols))
			for i, c := range ucols {
				keys[i] = bytdb.IndexCol{Name: c}
			}
			idxName := s.Table + "_" + strings.Join(ucols, "_") + "_key"
			if _, err := d.e.CreateIndexCols(s.Table, idxName, true, keys); err != nil {
				d.e.DropTable(s.Table) // best-effort unwind; the create error matters more
				return nil, err
			}
		}
		// Foreign keys go on last: a same-statement UNIQUE column may be
		// what a sibling table's reference (or a self-reference) needs.
		// The table is empty, so no row validation is required.
		for _, def := range s.FKs {
			fkd, err := resolveFK(desc, def)
			if err == nil {
				err = d.e.AddForeignKey(s.Table, fkd, false)
			}
			if err != nil {
				d.e.DropTable(s.Table) // best-effort unwind, as with unique indexes
				return nil, err
			}
			// Keep later name-collision checks accurate within this loop.
			desc = d.e.Table(s.Table)
		}
		return &Result{}, nil
	case *DropTable:
		if err := d.e.DropTable(s.Table); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *AddColumn:
		col, err := toEngineColumn(s.Col)
		if err != nil {
			return nil, err
		}
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
	case *RenameTable:
		if err := sysWriteGuard(s.To); err != nil {
			return nil, err
		}
		if err := d.e.RenameTable(s.Table, s.To); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *RenameColumn:
		// A CHECK constraint's stored text would silently orphan; the
		// engine guards the FK side.
		if err := d.checkColumnUnmentioned(s.Table, s.Col, "rename"); err != nil {
			return nil, err
		}
		if err := d.e.RenameColumn(s.Table, s.Col, s.To); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *AddConstraint:
		return d.execAddConstraint(s)
	case *AddFK:
		return d.execAddFK(s)
	case *DropConstraint:
		return d.execDropConstraint(s)
	case *AlterOwner:
		// No roles in bytdb; the statement exists only so Postgres DDL
		// (pg_dump output, goose migrations) runs unmodified. Succeed
		// without touching the catalog — even table existence is not
		// checked, matching the "ignore" half of parse-and-ignore.
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
	case *Truncate:
		return d.execTruncate(s)
	case *ShowVar:
		// A bare DB has no SET state; SHOW reports the defaults.
		return execShow(s, nil, 0)
	case *CreateSequence:
		return d.execCreateSequence(s)
	case *DropSequence:
		return d.execDropSequence(s)
	case *AlterSequence:
		return d.execAlterSequence(s)
	case *Insert:
		dv, err := d.withViews(s)
		if err != nil {
			return nil, err
		}
		return dv.execInsert(s)
	case *Select:
		// runIt materializes referenced views, then the statement's
		// CTEs, then runs the body.
		runIt := func(dd *DB) (*Result, error) {
			dv, err := dd.withViews(s)
			if err != nil {
				return nil, err
			}
			return dv.execSelectWith(s)
		}
		// A SELECT calling nextval/setval writes sequence state, so it
		// runs in a writable transaction: a copy of this DB carrying
		// the write txn makes every inner read() land in it. Session
		// blocks keep their own transaction (READ ONLY ones then fail
		// the write, as they should). CTEs and views get a wrapping
		// read transaction for the same reason — their materializations
		// and the body must share one snapshot.
		if d.tx == nil && (selectWritesSequences(s) || len(s.With) > 0 || d.usesViews(s)) {
			var res *Result
			txnRun := d.e.ReadTxn
			if selectWritesSequences(s) {
				txnRun = d.e.WriteTxn
			}
			err := txnRun(func(tx *bytdb.Txn) error {
				dw := *d
				dw.tx = tx
				var err error
				res, err = runIt(&dw)
				return err
			})
			if err != nil {
				return nil, err
			}
			return res, nil
		}
		return runIt(d)
	case *Update:
		dv, err := d.withViews(s)
		if err != nil {
			return nil, err
		}
		return dv.execUpdate(s)
	case *Delete:
		dv, err := d.withViews(s)
		if err != nil {
			return nil, err
		}
		return dv.execDelete(s)
	case *Explain:
		// EXPLAIN must not execute, so CTEs and views register as
		// empty-row virtual tables with statically derived shapes.
		dd, err := d.staticViews(s.Stmt)
		if err != nil {
			return nil, err
		}
		if sel, ok := s.Stmt.(*Select); ok {
			if dd, err = dd.staticWith(sel); err != nil {
				return nil, err
			}
		}
		return dd.execExplain(s)
	case *CreateView:
		return d.execCreateView(s)
	case *DropView:
		return d.execDropView(s)
	case *TxnControl:
		// Sessions intercept these before run; a bare DB has no
		// transaction state to control.
		return nil, serr.New("transaction control statements require a Session",
			"statement", s.Tag)
	case *SetVar:
		// Sessions intercept SET to give statement_timeout meaning; a
		// bare DB has no session state, so honoring the timeout is
		// impossible — refuse rather than silently not time out.
		// Everything else is accepted and ignored, exactly as the
		// Session does for parameters without bytdb semantics.
		if s.Name == "statement_timeout" {
			return nil, serr.New("SET statement_timeout requires a Session")
		}
		return &Result{Tag: s.Tag}, nil
	}
	return nil, serr.New("unhandled statement type")
}
