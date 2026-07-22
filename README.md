# bytdb

A relational layer over [btypedb](https://github.com/rohanthewiz/btypedb),
built the way CockroachDB layers SQL over Pebble: tables, rows, and
(eventually) secondary indexes are all encoded into **one ordered key
space**, so relational operations become key scans against an ordered
store that already provides atomic batches, O(1) copy-on-write snapshot
reads, range deletes, and power-loss-tested durability.

Embedded and in-process, for datasets that fit in memory — the SQLite
niche, not the CockroachDB niche.

## Status

Milestones 1–29 plus a production-readiness sweep beyond them: a
working relational store, queryable in SQL — in process or over the
Postgres wire protocol, with transaction blocks and savepoints, NOT
NULL / CHECK / UNIQUE constraints, foreign keys (MATCH SIMPLE, NO
ACTION/RESTRICT or ON DELETE CASCADE, SQLSTATE 23503), SERIAL
identity columns, standalone sequences (`CREATE SEQUENCE`,
`nextval`/`setval`), column DEFAULTs (constants plus the `now()` /
`current_date` clock markers), RETURNING, upsert
(`INSERT ... ON CONFLICT`), window functions (ranking, value, and
aggregate — DISTINCT included — with explicit ROWS/RANGE/GROUPS
frames including RANGE offsets on the sort key and frame EXCLUDE,
composing with GROUP BY), WITH CTEs, derived tables, and persistent
views, hash joins for unindexed equijoins, `LIKE`/`ILIKE`,
timestamp/timestamptz/date/uuid/jsonb/text[] column types — jsonb
with the everyday operator family (`->` `->>` `#>` `#>>` `@>` `<@`
`?` `?|` `?&` `||` `-`) — TRUNCATE, SET/SHOW, ALTER TABLE RENAME,
descending index columns, order-aware index selection, EXPLAIN,
SCRAM-SHA-256(-PLUS) auth on the wire, and enough system catalog and
expression language that psql's `\dt`, `\d`, `\d <table>`, `\di`,
`\du`, and `\l` render for real — check and foreign-key constraints,
column defaults, and sequences included.

- **`tuple`** — an order-preserving binary encoding for composite keys:
  for any two tuples, `bytes.Compare` on their encodings equals
  element-wise semantic comparison. NULL, bool, int64, float64, string,
  and []byte elements; property-tested (random tuples, encode → byte
  order ≡ semantic order).
- **table layer** — a persistent catalog (descriptors stored as rows of
  a system table), `CreateTable`/`DropTable`, `Insert` with type
  coercion and PK uniqueness, `Get`/`Delete` by primary key, and
  `Scan`/`ScanRange` in primary-key order with partial-prefix bounds on
  composite keys.
- **secondary indexes** — `CreateIndex` (with atomic backfill over
  existing rows), `DropIndex`, unique indexes, per-column DESC key
  ordering (byte-inverted encoding), and `ScanIndex` with range
  bounds; every insert and delete maintains all indexes in the same
  atomic commit as the row.
- **row updates and transactions** — `Update` sets columns by name
  (primary-key changes move the row), with every affected index entry
  moved and uniqueness re-checked before anything is written;
  `WriteTxn`/`ReadTxn` run multi-statement work on a serializable
  snapshot of data and catalog, with own-write visibility.
- **schema changes without rewrites** — row values are sparse
  (column ID, value) pairs with NULLs omitted, so `AddColumn` and
  `DropColumn` touch only the descriptor: old rows read added columns
  as NULL, dropped-column data is skipped on decode, and a re-added
  name gets a fresh ID so stale data can never resurface.
- **SQL frontend** — the `sql` package parses, plans, and executes a
  small Postgres-flavored dialect over the engine: full DDL, INSERT
  (with `ON CONFLICT` upsert, SERIAL identity columns, constant
  column DEFAULTs, and expression values — `VALUES (nextval('s'), ...)`),
  SELECT/UPDATE/DELETE with a planner that pushes WHERE predicates
  down to point gets and bounded key scans, RETURNING on every DML
  statement (reporting rows as stored — drawn SERIAL values included),
  aggregates with GROUP BY
  and HAVING, SELECT DISTINCT, INNER/LEFT/CROSS joins executed as
  index nested loops,
  prepared statements with `$1`-style parameters, NOT NULL and CHECK
  constraints with Postgres wording, EXPLAIN, and an expression
  language: CASE, IN, regex matches (`~`, `!~`, ...), `::` casts,
  arithmetic and `||`, output aliases, correlated scalar subqueries,
  EXISTS, and UNION [ALL].
- **Postgres wire protocol** — the `pgwire` module (its own go.mod)
  serves a database to psql, pgx, and `database/sql`: simple and
  extended query protocols, text and binary formats, inferred
  parameter types, and structured errors with SQLSTATE codes and
  error positions.
- **system catalog** — virtual `pg_catalog` and `information_schema`
  tables synthesized from the engine catalog, so clients and ORMs
  introspect with the queries they already send (GORM's `HasTable`,
  the pg_attribute/pg_type join, `SELECT version()`, and psql's
  backslash commands verbatim), all read-only and flowing through the
  ordinary join and aggregate machinery.

## Example

```go
e, err := bytdb.Open("app.db")
defer e.Close()

_, err = e.CreateTable("events", []bytdb.Column{
    {Name: "org", Type: bytdb.TString}, {Name: "seq", Type: bytdb.TInt}, {Name: "note", Type: bytdb.TString},
}, "org", "seq") // composite primary key

err = e.Insert("events", "acme", 1, "signup")
err = e.Insert("events", "acme", 10, "upgrade")

row, ok, err := e.Get("events", "acme", 1)

// Ordered range scan: all of acme's events, seq order, via one key range.
for row, err := range e.ScanRange("events", []any{"acme"}, []any{"acmf"}) {
    if err != nil { break }
    fmt.Println(row.Col("seq"), row.Col("note"))
}

// Secondary index: backfilled from existing rows, then maintained by
// every write in the same atomic commit.
_, err = e.CreateIndex("events", "by-note", false, "note")
for row, err := range e.ScanIndex("events", "by-note", []any{"s"}, []any{"t"}) {
    // notes in ["s", "t"), note order
}

// Row update by primary key: named columns, indexes maintained.
updated, err := e.Update("events", []any{"acme", 1}, map[string]any{"note": "signup+trial"})

// Transactions: serializable, atomic, own writes visible inside.
err = e.WriteTxn(func(tx *bytdb.Txn) error {
    if err := tx.Insert("events", "acme", 11, "invite"); err != nil {
        return err
    }
    _, err := tx.Update("events", []any{"acme", 10}, map[string]any{"note": "upgraded"})
    return err // nil commits both; error rolls both back
})
```

## SQL

```go
import bsql "github.com/rohanthewiz/bytdb/sql"

db := bsql.New(e)

_, err = db.Exec(`CREATE TABLE users (id int PRIMARY KEY, name text, age int, city text)`)
_, err = db.Exec(`CREATE INDEX by_city ON users (city)`)
_, err = db.Exec(`INSERT INTO users VALUES (1, 'ada', 36, 'london'), (2, 'grace', 45, 'nyc')`)

res, err := db.Exec(`SELECT name, age FROM users WHERE city = 'london' ORDER BY age DESC LIMIT 10`)
for _, row := range res.Rows {
    fmt.Println(row[0], row[1]) // values typed per res.Types
}

res, err = db.Exec(`UPDATE users SET city = 'sf' WHERE id = 2`) // res.RowsAffected == 1
```

The dialect is deliberately small and Postgres-flavored — `'string'`
literals, `"quoted"` identifiers (unquoted ones fold to lowercase),
`--` and `/* */` comments, and Postgres type names as aliases
(`bigint`/`int8` → int, `double precision`/`real` → float, `text`/
`varchar(n)` → string, `boolean` → bool, `bytea` → bytes). Beyond
those, `timestamp[tz]`, `date`, and `uuid` store UTC instants (int64
micros / days) and 16-byte values that order chronologically in keys
and indexes and present natively over the wire; `text[]` is a
one-dimensional string array riding on canonical Postgres
array-literal text (OID 1009 both wire formats, with real
`array_to_string`/`array_length` and `= ANY(col)`); and `jsonb`
(`json` is an alias) stores documents in a canonical rendering —
compact, keys sorted, one spelling per document — so plain `=` is
document equality, with the operator family `->` `->>` `#>` `#>>`
`@>` `<@` `?` `?|` `?&` `||` `-` and `::jsonb` casts (OID 3802,
binary format included). A `varchar(n)` limit is enforced on every
write with Postgres's wording (22001). As in
Postgres, a quoted literal is untyped until context types it:
`WHERE id = '2'` against an int column compares as the integer 2 (and
errors if the text doesn't parse as one), which is what quote-happy
clients like pgx's simple protocol produce.

Supported statements:

```sql
CREATE TABLE t (id serial PRIMARY KEY,            -- or PRIMARY KEY (a, b)
                c type [NOT NULL] [UNIQUE] [DEFAULT lit]
                       [REFERENCES p [(col)] [ON DELETE CASCADE]], ...,
                [UNIQUE (cols)] [[CONSTRAINT name] CHECK (expr)]
                [[CONSTRAINT name] FOREIGN KEY (cols) REFERENCES p [(cols)]])
DROP TABLE t
ALTER TABLE t ADD COLUMN c type | DROP COLUMN c
ALTER TABLE t ADD [CONSTRAINT name] CHECK (expr) | FOREIGN KEY ...
ALTER TABLE t DROP CONSTRAINT [IF EXISTS] name
ALTER TABLE t RENAME TO t2 | RENAME [COLUMN] c TO c2
ALTER TABLE t OWNER TO role         -- accepted, no-op (bytdb has no roles)
CREATE [UNIQUE] INDEX idx ON t (c [ASC|DESC], ...)
DROP INDEX idx [ON t]
CREATE SEQUENCE [IF NOT EXISTS] s [options] | ALTER SEQUENCE | DROP SEQUENCE
CREATE [OR REPLACE] VIEW v AS SELECT ... | DROP VIEW [IF EXISTS] v
INSERT INTO t [(cols)] VALUES (...), (...) | DEFAULT VALUES
       [ON CONFLICT [(cols)] DO NOTHING | DO UPDATE SET ... [WHERE ...]]
       [RETURNING items]
[WITH name [(cols)] AS (SELECT ...), ...]
SELECT * | items FROM tables [WHERE ...] [GROUP BY ...] [HAVING ...]
       [ORDER BY item [DESC], ...] [LIMIT n] [OFFSET n]
UPDATE t SET c = v, ... [WHERE ...] [RETURNING items]
DELETE FROM t [WHERE ...] [RETURNING items]
TRUNCATE [TABLE] t [, ...] [RESTART IDENTITY | CONTINUE IDENTITY]
SET [SESSION|LOCAL] name {=|TO} value | RESET name | SHOW name | SHOW ALL
EXPLAIN statement
BEGIN | START TRANSACTION ... COMMIT | END | ROLLBACK | ABORT
SAVEPOINT name | RELEASE [SAVEPOINT] name | ROLLBACK TO [SAVEPOINT] name
```

`serial` (also `bigserial`/`smallserial`) is Postgres-style sugar for
an int identity column — `GENERATED BY DEFAULT AS IDENTITY` spelled
out — with a durable counter per column: omitting the column (or
inserting NULL) draws the next value, and an explicit value inserts
as given and bumps the counter past itself, so later draws never
collide (MySQL's semantics, deliberately: it removes Postgres's
duplicate-key-after-restore footgun). `lastval()` and
`currval('t_col_seq')` read back the session's draws, though
`RETURNING id` is the one-round-trip way to learn a generated key.
Column `DEFAULT`s are constant literals — plus exactly the clock
functions: `DEFAULT now()` (all `CURRENT_TIMESTAMP`-family spellings
normalize to it) and `DEFAULT current_date` on timestamp/date
columns, evaluated once per INSERT statement so a multi-row insert
stamps every row with the same instant. Defaults apply when a column
list omits the column, by the `DEFAULT` keyword in VALUES, and by
`DEFAULT VALUES`; general expression defaults stay rejected — a
default is a stored constant or a clock marker, never an expression
tree. `ON CONFLICT` follows
Postgres exactly: the conflict target names the primary key's or a
unique index's columns (`DO NOTHING` may omit it to absorb any
uniqueness collision), and in `DO UPDATE SET` bare or table-qualified
names read the existing row while `excluded.col` reads the proposed
one, with an optional `WHERE` to leave non-matching pairs alone.

`FROM` names one table or a left-deep chain of joins — `a [AS] x
[INNER] JOIN b ON x.id = b.a_id`, `LEFT [OUTER] JOIN`, `CROSS JOIN`
(a comma is a cross join) — with qualified column references and
`t.*`. A FROM item may also be a derived table (`(SELECT ...) alias`),
a `WITH` CTE (non-recursive; materialized once, visible everywhere in
the statement), or a persistent view (`CREATE [OR REPLACE] VIEW`
stores the SELECT's text and any statement naming it materializes it
at that moment). Joins run as nested loops, but equality conjuncts
re-bind per outer row, so an inner table joined on its primary key or
an indexed column is a point get or bounded scan per row, not a full
scan; when no index can serve an equijoin — including every join
against a CTE, derived table, or view — the step becomes a hash join
instead, so unindexed equijoins are linear, not quadratic.

Foreign keys are declared column-level (`REFERENCES parent [(col)]`)
or table-level (`FOREIGN KEY (cols) REFERENCES parent [(cols)]`), and
may be added later by `ALTER TABLE ADD` (existing rows validated in
the transaction that publishes the constraint). The referenced
columns must be the parent's primary key or a unique index's columns.
Semantics are MATCH SIMPLE: a child INSERT/UPDATE requires the
referenced parent row to exist (any NULL FK column satisfies the
constraint), and a parent DELETE/UPDATE is refused while child rows
reference the old key — checked at end of statement, so deleting a
parent together with its children in one statement is legal. `ON
DELETE CASCADE` is the one supported referential action: the parent
DELETE removes referencing rows transitively instead of refusing
(cascaded rows do not count toward RowsAffected or RETURNING, and a
NO ACTION constraint further down still blocks the whole statement).
Violations carry Postgres's wording and SQLSTATE 23503.

Aggregates are `COUNT(*)`, `COUNT(x)`, `SUM(x)`, `AVG(x)`, `MIN(x)`,
`MAX(x)` — over a column, any per-row expression (`SUM(a * b)`), or
`DISTINCT x` (`COUNT(DISTINCT city)`: each distinct non-NULL value
counts once per group) — with SQL semantics throughout: aggregates ignore NULLs (`COUNT(*)`
counts rows), NULL group values form one group, an ungrouped
aggregate query returns exactly one row even over zero rows, and
`HAVING` filters groups. A `GROUP BY` key is a column, an expression,
or an integer ordinal naming a select-list position (`GROUP BY 1`),
and select items, `HAVING`, and `ORDER BY` take expressions over the
grouped data — group keys, aggregate results, and anything built from
them:

```sql
SELECT age / 10 AS decade, count(*) AS n, max(age)
FROM users WHERE age > 18
GROUP BY age / 10 HAVING count(*) >= 2
ORDER BY n DESC, decade LIMIT 3
```

WHERE and HAVING are boolean expressions: predicates — `column op
literal` (`=`, `!=`, `<>`, `<`, `<=`, `>`, `>=`, either operand
order) or `column IS [NOT] NULL` — combined with `AND`, `OR`, and
`NOT` (standard precedence, parentheses group), evaluated with SQL
three-valued logic, so `NOT v = 1` still excludes NULL rows. The
planner is the roadmap's "filter pushdown" made real: equality on
every primary-key column becomes a point `Get`; an equality prefix
(plus at most one range column) of the primary key or of a secondary
index becomes a bounded ordered scan with early termination — using
only predicates that are top-level `AND` conjuncts (anything under
`OR`/`NOT` stays filter-only); everything else falls back to a
filtered full scan. The whole condition is also re-checked row by
row, so pushdown only narrows what is visited — correctness never
depends on it.

Statements take `$1`-style parameters wherever a literal may appear —
`Exec` binds its trailing arguments, and `Prepare` parses once for
repeated execution:

```go
res, err = db.Exec(`SELECT name FROM users WHERE city = $1 AND age > $2`, "london", 30)

st, err := db.Prepare(`INSERT INTO users VALUES ($1, $2, $3, $4)`)
_, err = st.Exec(3, "alan", 41, "london") // re-executable; safe for concurrent use
```

For introspection there is a virtual system catalog:
`pg_catalog.pg_namespace`, `pg_class` (sequences and views included),
`pg_attribute`, `pg_attrdef`, `pg_type`, `pg_index`, `pg_sequence`,
`pg_constraint` (checks and foreign keys, `confdeltype` included),
`pg_am`, `pg_database`, `pg_roles`, and `pg_stat_activity` with real
rows, a set of always-empty tables psql probes (`pg_policy`, the
`pg_publication` family, ...), plus `information_schema.tables`,
`columns`, and `sequences`, all synthesized from the engine catalog
on the fly and queryable like any tables — WHERE, joins, and
aggregates included — but read-only. Table names may be schema-qualified (`public.t` is
`t`; bare `pg_class` resolves because pg_catalog is on the search
path). `SELECT` works without FROM (`SELECT 1`), a small whitelist of
zero-argument functions folds to constants (`version()`,
`current_schema()`, `current_database()`, ...), the introspection
functions psql calls evaluate for real (`format_type`,
`pg_get_indexdef`, `pg_get_userbyid`, `pg_table_is_visible`, ...),
and `ORDER BY 1, 2` addresses select-list positions. That covers real
client probes verbatim:

```sql
SELECT count(*) FROM information_schema.tables
WHERE table_schema = CURRENT_SCHEMA() AND table_name = $1
  AND table_type = 'BASE TABLE'            -- GORM HasTable

SELECT a.attname, t.typname, a.attnotnull
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
WHERE c.relname = $1 ORDER BY a.attnum     -- column introspection
```

psql's `\dt`, `\d`, `\d <table>`, `\d <index>`, `\di`, `\dn`, `\du`,
and `\l` render against it — including the queries whose exotic
corners (`generate_series` in FROM, array subscripts, `string_agg`)
sit in branches that only run over rows the empty catalog tables
never produce: unknown names error at evaluation, not at parse, so
zero-row queries succeed.

Each statement is atomic: a multi-row INSERT, an UPDATE, or a DELETE
runs in one engine transaction and rolls back entirely on error. For
multi-statement transactions, `DB.NewSession()` executes `BEGIN ...
COMMIT | ROLLBACK` blocks with Postgres semantics: the block is one
engine transaction, an error fails the block until `ROLLBACK`
(`COMMIT` then rolls back and says so in its command tag), and
redundant control statements warn without failing. `SAVEPOINT` marks
a point inside the block — an O(1) copy-on-write snapshot of the
transaction's state — that `ROLLBACK TO` rewinds to, clearing the
failed state, so a block recovers from an error instead of losing
everything; `RELEASE` drops the mark and keeps the work. Isolation levels
parse and are ignored — single-writer transactions are serializable,
which satisfies every level — and `READ ONLY` is honored. A writable
block holds the engine's writer lock from `BEGIN` to its end (other
sessions' writes wait; reads never do), and DDL cannot run inside a
block since every schema change is its own transaction.

Errors are [serr](https://github.com/rohanthewiz/serr) structured
errors: `%v` prints just the message, `bytdb.ErrText(err)` renders it
with the structured attributes for user-facing surfaces — `wrong
number of parameters (want: 1, got: 0)` — and a serr-aware logger
gets the full context including code locations.

## Postgres wire protocol

The `pgwire` module (a nested module, so the core library keeps zero
serving dependencies) exposes a database over PostgreSQL protocol
3.0 — psql, pgx, `database/sql` via pgx's stdlib adapter, and
anything else that speaks Postgres connects to it:

```go
import (
    "github.com/rohanthewiz/bytdb/pgwire"
    bsql "github.com/rohanthewiz/bytdb/sql"
)

err := pgwire.NewServer(bsql.New(e)).ListenAndServe("127.0.0.1:5433")
```

or as a standalone server:

```
go run github.com/rohanthewiz/bytdb/pgwire/cmd/bytdbd -db app.db -addr 127.0.0.1:5433
psql "postgres://any@127.0.0.1:5433/any?sslmode=disable"
```

The embedded API maps directly onto the protocol: `Prepare` is Parse,
`Stmt.Describe` answers Describe — parameter types are inferred from
the column each `$n` compares against or inserts into, and the result
shape (`Result`'s columns + types) is computed from the catalog
without executing — and `Stmt.Exec` is Bind/Execute. Both the simple
(`Q`, multi-statement) and extended protocols work, in text and
binary formats for all five column types. Errors cross the wire
structurally from serr fields: the parser's byte offset becomes the
error Position (psql's `LINE 1: ... ^` caret), structured attributes
become DETAIL, and stable message texts map to SQLSTATE codes
(syntax_error, undefined_table, unique_violation, ...).

Auth is trust by default (user/database accepted and ignored); with a
credentials registry set, SCRAM-SHA-256 runs for real — RFC 5802 with
channel binding (SCRAM-SHA-256-PLUS) over TLS. `bytdbd` adds
`-max-conns` (a connection cap), `-sync always|never` (WAL fsync
policy), and query logging. Transaction blocks work as in Postgres: each connection is
a `sql.Session`, `ReadyForQuery` reports the real status (idle / in
transaction / failed), redundant `BEGIN`/`COMMIT` raise
`NoticeResponse` warnings, and a dropped connection rolls back its
open block. Savepoints work over the wire too — pgx's nested
transactions ride on them. Cancellation, portal suspension, and COPY are not
implemented. The end-to-end tests drive a real
pgx v5 client — including pgx's statement cache and its simple
protocol mode, which renders every argument as a quoted literal and
so exercises the dialect's untyped-literal coercion.

## How it maps onto the key space

Everything lives in a single `btypedb.DB[string, []byte]`:

```
tuple(tableID, 1, pk cols...)             → tuple(colID, val, ...)  table rows (primary index)
tuple(tableID, idxID, indexed..., pk...)  → ()                      secondary index entry
tuple(tableID, idxID, indexed...)         → tuple(pk cols...)       unique index entry
tuple(1, 1, tableName)                    → JSON descriptor         catalog
tuple(0)                                  → next table ID           ID sequence
```

Row values tag every non-NULL, non-key column with its stable column
ID rather than relying on position — the CRDB value-encoding idea that
makes `ALTER TABLE` metadata-only.

A unique index enforces uniqueness by key collision — the primary key
moves into the value. Rows with NULL in an indexed column fall back to
the pk-suffixed form even in a unique index, so NULLs never conflict
(SQL semantics); the two entry forms are distinguished by tuple arity
on decode.

The `tuple` encoding is what makes this work: integers are
sign-flipped big-endian so negatives sort first, floats get the
standard order-preserving bit transform, strings escape embedded zero
bytes with an ordered terminator so `"a" < "a\x00" < "ab"`, and a
tuple that is a prefix of another sorts first. Because a composite
key's encoding is ordered per-column, a partial tuple is a valid scan
bound — `ScanRange(t, []any{"acme"}, []any{"beta"})` is one contiguous
key range.

Type tags are persistent format; they are never renumbered.

What btypedb supplies underneath (the same contract CockroachDB asks
of Pebble): ordered iteration with pivots, atomic multi-key commits
(row + future index writes, all-or-nothing in the WAL), snapshot
isolation via O(1) COW snapshots, savepoints as O(1) COW marks within
a transaction, `DeleteRange` for `DROP TABLE`, and fsync-before-ack
durability with group commit.

## Roadmap

- [x] **Milestone 1**: order-preserving tuple encoding; catalog; create/drop table; insert/get/delete; ordered scans with range bounds
- [x] **Milestone 2**: secondary indexes as key ranges, backfilled and maintained in the same atomic commit as the row; unique indexes (NULLs exempt); `ScanIndex` with partial-prefix bounds
- [x] **Milestone 3**: `Update` by primary key (pk moves included) with check-before-write index maintenance; `WriteTxn`/`ReadTxn` engine transactions over btypedb's — serializable via single-writer, catalog snapshotted at begin (DDL stays outside transactions)
- [x] **Milestone 4**: column-ID-tagged sparse row values; `AddColumn`/`DropColumn` as metadata-only changes (no row rewrites), with never-reused column IDs
- [x] **Milestone 5**: SQL frontend — a hand-rolled Postgres-flavored dialect (zero new dependencies): lexer → recursive-descent parser → planner with filter pushdown to point gets and bounded key scans → executor over engine transactions
- [x] **Milestone 6**: aggregates — COUNT/SUM/AVG/MIN/MAX, GROUP BY (hash aggregation keyed by the order-preserving tuple encoding, so groups emit in order), HAVING, ORDER BY over grouped columns and aggregates
- [x] **Milestone 7**: boolean expressions — AND/OR/NOT with parentheses in WHERE and HAVING, SQL three-valued logic, pushdown restricted to top-level AND conjuncts
- [x] **Milestone 8**: joins — INNER/LEFT/CROSS as left-deep nested loops with per-outer-row re-binding of equality conjuncts (index nested loop), qualified column references, `t.*`
- [x] **Milestone 9**: prepared statements — `$1` parameters in WHERE/ON/HAVING, INSERT, and UPDATE SET; variadic `Exec` and `Prepare`/`Stmt` that re-bind into a copy per execution
- [x] **Milestone 10**: Postgres wire protocol — the `pgwire` nested module: startup/auth handshake, simple + extended query protocols mapped onto `Prepare`/`Describe`/`Exec` (with parameter-type inference and catalog-computed result shapes), text + binary formats, structured errors with SQLSTATE and positions; plus Postgres-style untyped-literal coercion in the dialect
- [x] **Milestone 11**: system catalog — virtual `pg_catalog` + `information_schema` tables from the engine catalog through the ordinary executor; schema-qualified names, SELECT without FROM, literal select items, folded zero-arg functions (`version()`, `current_schema()`, ...), ORDER BY ordinals; catalog writes rejected
- [x] **Milestone 12**: expression language — one grammar for select items and conditions (CASE, IN, `~`/`!~`/`~*`/`!~*`, `OPERATOR()`/`COLLATE`, `::` casts, arithmetic/`||`, functions with arguments, output aliases), lowered to the legacy predicate shapes where simple so pushdown survives; eval-time name resolution through an environment chain (correlated scalar subqueries, EXISTS, ARRAY(SELECT ...) — and unknown functions error only if a row reaches them); UNION [ALL]; catalog grown until psql's `\dt`, `\d`, `\d <table>`, `\d <index>`, `\di`, `\dn`, `\du`, `\l` all render
- [x] **Milestone 13**: transaction blocks — `Engine.Begin`/`Txn.Commit`/`Txn.Rollback` over btypedb's manual transactions; `sql.Session` with Postgres block semantics (failed-block state and 25P02, `COMMIT`-of-failed reports `ROLLBACK`, warnings for redundant control, `READ ONLY`, isolation levels accepted as serializable, DDL refused in-block); pgwire sessions with real `ReadyForQuery` status, `NoticeResponse`, and rollback on disconnect
- [x] **Milestone 14**: GROUP BY ordinals (`GROUP BY 1`); savepoints — btypedb v0.4 `Savepoint`/`RollbackTo`/`Release` as O(1) COW marks with WAL-batch truncation, `SAVEPOINT`/`RELEASE`/`ROLLBACK TO` in sessions with Postgres semantics (name shadowing, failed-block recovery, 3B001/25P01), pgx nested transactions over the wire
- [x] **Milestone 15**: aggregate expressions — GROUP BY keys as arbitrary expressions (columns, ordinals, `age / 10`, CASE); select items, HAVING, and ORDER BY rewritten against the keys (matching subtrees read the group's key value, aggregate calls read accumulators, leftover columns get the classic must-appear-in-GROUP-BY error) and evaluated per group by the ordinary expression evaluator; aggregate calls over expressions (`SUM(a * b)`)
- [x] **Milestone 16**: DESC index columns — per-column byte-inverted key encoding (CRDB-style), mixed-direction composite keys, planner range pushdown swapped on descending columns, `pg_get_indexdef` rendering; NOT NULL and CHECK constraints — NOT NULL enforced in the engine, CHECK expressions stored as text in the descriptor and enforced by the SQL layer on INSERT/UPDATE (NULLs pass, Postgres wording and SQLSTATEs 23502/23514, `pg_constraint` + `pg_get_constraintdef` so `\d` shows them); EXPLAIN — the Postgres-shaped plan tree (Point Get / Index Scan / Seq Scan, Index Cond vs Filter, Nested Loop / Aggregate / Sort / Limit / Append), no invented costs, ANALYZE rejected
- [x] **Milestone 17**: DISTINCT aggregates — `COUNT(DISTINCT x)` and friends, deduplicated per group by the order-preserving tuple encoding (the same equivalence GROUP BY uses); ALTER TABLE ADD/DROP CONSTRAINT — `AddCheck` verifies every existing row satisfies a new check inside the transaction that publishes the descriptor (`is violated by some row`, SQLSTATE 23514), `DROP CONSTRAINT [IF EXISTS]` removes checks by name (42704 when absent), which also unblocks dropping a previously check-referenced column
- [x] **Milestone 18**: `op ANY(...)` / `op ALL(...)` — a right-hand side that is an `ARRAY[...]` constructor, a single-column subquery, or a Postgres `'{...}'` array-literal string (elements coerce to the left operand's type); Postgres three-valued logic (empty array is ANY→false, ALL→true even against a NULL left side), so `= ANY` generalizes `IN` and `<> ALL` generalizes `NOT IN`; `ARRAY[...]` is also a value that renders as `{...}`
- [x] **Milestone 19**: window functions — `ROW_NUMBER`/`RANK`/`DENSE_RANK` and aggregate windows (`SUM/COUNT/AVG/MIN/MAX(...) OVER (PARTITION BY ... ORDER BY ...)`) as a third execution shape that emits every input row: materialize post-filter rows, partition by the order-preserving tuple encoding (the GROUP BY equivalence), sort each partition, assign per-row values (ranking by ordinal / prior-key compare; aggregates whole-partition when unordered, else running with the Postgres RANGE peer-sharing default); window calls rewrite to environment refs so the ordinary evaluator handles nesting (`rn + 1`); `EXPLAIN` gains a WindowAgg node; explicit ROWS/RANGE frames, `DISTINCT`, and window+GROUP BY are rejected
- [x] **Milestone 20**: ORDER BY exploiting index order — a single-table SELECT whose sort matches a scan's key order skips the sort (and, under a LIMIT, stops early). The chosen path's key columns each advance a fixed way (ascending, or descending for a DESC index column); an equality-pinned prefix carries no order and is skipped, so `WHERE a = 1 ORDER BY b` reads a `(a, b)` index directly. A fully-reversed match runs the scan backward (new engine `ScanRangeRev`/`ScanIndexRev` over btypedb's `Descend`), giving bounded `ORDER BY … DESC LIMIT n`; when no WHERE pushes a path and a LIMIT bounds the result, a NOT NULL indexed column's ordered walk replaces the full scan + sort. Ordering columns must be NOT NULL (the key encoding places NULLs opposite to `NULLS LAST`/`FIRST`), so nullable columns keep the sort; `EXPLAIN` shows `Index Scan [Backward] …` and drops the `Sort`
- [x] **Milestone 21**: bounded reverse scans — any pushed-down scan region can now be read backward, not just an unbounded one. A forward plan's region is already key-delimited on both edges (`from` below, the early-stops above), so reversing converts the stops into the backward walk's entry bound: equality-prefix values plus the range-stop value, closed over the value's group or open by stop kind (`revEnd`); the engine's `ScanRangeRev`/`ScanIndexRev` gained a `toIncl` upper bound closing over a whole key-prefix group (`tuple.PrefixEnd`), which an exclusive partial-prefix bound cannot express. So `WHERE cat = 2 ORDER BY pri DESC LIMIT n` on a `(cat, pri)` index walks just the tail of the `cat = 2` region backward — no sort, early termination — and `WHERE id > 2 ORDER BY id DESC`, range predicates on the ordered column, and DESC-index ranges read in reverse all elide the sort too
- [x] **Milestone 22**: identity columns and RETURNING — `SERIAL`/`BIGSERIAL`/`SMALLSERIAL` as sugar for `GENERATED BY DEFAULT AS IDENTITY` (NOT NULL implied; `ALWAYS` rejected as an unenforceable promise), each identity column owning a durable engine counter where an explicit insert bumps the counter past itself so post-restore draws never collide; `RETURNING` on INSERT/UPDATE/DELETE as a full select list reporting rows as stored (drawn identity values, coercions, SET applied; DELETE reports the pre-image), `Describe`-able over the wire, and mirrored in the embedded API (`Engine.InsertReturning`, `Txn.InsertReturning`/`UpdateReturning`); `information_schema.columns` shows the serial-style `column_default` ORMs key off
- [x] **Milestone 23**: write-path ORM enablement — `INSERT ... ON CONFLICT DO NOTHING | DO UPDATE` with arbiter inference over the primary key and unique indexes (probe-based: no new engine machinery), `excluded.*` and Postgres name resolution in SET/WHERE, only real writes counted; constant column `DEFAULT`s (column-list fill, `DEFAULT` keyword in VALUES, `DEFAULT VALUES`; expression defaults rejected, `ADD COLUMN ... DEFAULT` empty-table only); `lastval()`/`currval('t_col_seq')` session readback of identity draws with Postgres's 55000 before the first draw
- [x] **Milestone 24**: value window functions — `LAG`/`LEAD` (offset defaults to 1, evaluated per row, may be negative to flip direction, NULL offset → NULL; optional default expression for rows past the partition edge) addressing the whole partition as in Postgres, plus frame-sensitive `FIRST_VALUE`/`LAST_VALUE`/`NTH_VALUE` honoring the Postgres default frame — with ORDER BY the frame ends at the current row's last peer, so `LAST_VALUE` is the last *peer*, the same surprise PG ships; all five are new per-row assignment cases over the milestone-19 partition/sort machinery, with argument counts checked at parse, nested window calls rejected, and `$n` placeholders binding inside offset/default arguments
- [x] **Milestone 25**: explicit window frames — `{ROWS|RANGE|GROUPS} [BETWEEN <start> AND <end>]` with `UNBOUNDED PRECEDING/FOLLOWING`, `<n> PRECEDING/FOLLOWING`, and `CURRENT ROW` bounds; a row's frame reduces to a half-open position range over the sorted partition (ROWS by position arithmetic, RANGE/GROUPS through peer groups, where CURRENT ROW spans the current row's peers and a GROUPS offset steps whole groups), honored by aggregate windows and `FIRST/LAST/NTH_VALUE` (empty frames → NULL, COUNT 0) and ignored by ranking and `LAG`/`LEAD` as in Postgres; frames anchored at UNBOUNDED PRECEDING accumulate incrementally (the default frame stays O(rows)), moving-start frames recompute per distinct frame with peer memoization; offsets are row-independent (columns rejected at parse, `$n` binds, negative/NULL/non-int error at run); `RANGE <n> PRECEDING` (PG 11+ typed sort-key arithmetic) and `EXCLUDE` other than `NO OTHERS` are rejected; PG-faithful bound-pair validation ("frame starting from following row cannot reference current row", ...)
- [x] **Milestone 26**: RANGE offset frames — `RANGE <offset> PRECEDING/FOLLOWING` bounds, where the offset is a distance measured on the window ORDER BY key (PG 11's typed sort-key arithmetic) rather than a row or group count: exactly one ORDER BY column (parse-checked, PG wording), numeric key type (int/float; checked at execution — "not supported for column type ..."), fractional offsets legal (`RANGE 0.5 PRECEDING`, over int keys too — mixed numerics compare); since the partition is sorted, each offset bound is a binary search for `key ∓ offset` moved against/with the sort direction (DESC flips the sign), pure-int arithmetic saturates to ±Inf on overflow instead of wrapping, and NaN keys ride the comparator's NaN-sorts-last order (NaN is within any distance of NaN only, as in PG's `in_range`); NULL sort keys follow Postgres — a NULL row's offset frame is exactly its peer group, non-NULL rows never reach NULLs through an offset bound but UNBOUNDED bounds still take them in; offsets must be non-null, numeric, non-negative, non-NaN ("invalid preceding or following size in window function"); `$n` offsets describe with the sort key's type so wire drivers encode fractional offsets as float8, not truncated int8
- [x] **Milestone 27**: frame `EXCLUDE` — `EXCLUDE CURRENT ROW | GROUP | TIES` (and the explicit no-op `NO OTHERS`) on every frame mode, completing the window-frame matrix: after the bounds pick a row's frame, exclusion removes the current row, its whole ORDER BY peer group, or the peers minus the row itself — but only rows the bounds actually selected, so `TIES` never re-admits a current row from outside the frame; a frame stops being one contiguous range and becomes up to three disjoint segments, which `FIRST/LAST/NTH_VALUE` walk in order (`NTH_VALUE` counts across the hole) and aggregates recompute per distinct segment list with last-frame memoization (whole peer groups share segments under `EXCLUDE GROUP`, so RANGE/GROUPS frames still aggregate once per group; the UNBOUNDED PRECEDING fast path is exclusion-free by construction); `GROUP`/`TIES` stay legal without ORDER BY — the whole partition is one peer group, so `GROUP` empties every frame and `TIES` leaves just the current row — and EXPLAIN renders the clause
- [x] **Milestone 28**: the outstanding-list sweep — five features closing the window arc and the catalog gaps. *Window + GROUP BY*: windows now evaluate after grouping and HAVING (Postgres' order), so `RANK() OVER (ORDER BY SUM(x) DESC)` ranks whole groups; the bridge is the aggregate resolver's new row mode — each surviving group materializes as a synthetic row (key values then accumulator results), every group-phase expression rewrites to positional reads, and the milestone-19 window machinery runs over those rows unchanged, frames and all (`SUM(SUM(x)) OVER`, EXPLAIN puts WindowAgg above HashAggregate). *DISTINCT in window aggregates* (a bytdb extension — PG doesn't implement it): `COUNT(DISTINCT x) OVER (...)` dedups within each row's frame, riding the existing accumulator dedup. *Standalone sequences*: `CREATE/ALTER/DROP SEQUENCE` with the Postgres option set (AS type bounds, INCREMENT, MIN/MAXVALUE, START, CYCLE, CACHE stored-but-1) over new engine sequence objects — one JSON record holding options and state, IDs from the table-ID counter so pg_class oids stay unique, names sharing the relation namespace; `nextval`/`setval` evaluate anywhere (a SELECT calling them silently runs in a write txn), accept `'s'::regclass`, and feed `lastval`/`currval`; sequences appear in `pg_class` (relkind 'S'), `pg_sequence`, `information_schema.sequences`, and each reads as its one-row state relation. *pg_attrdef*: declared column defaults get real catalog rows (adbin carries the stored literal text, `pg_get_expr` surfaces it, `atthasdef` gates it), identity columns report via `attidentity = 'd'` — psql's `\d` Default column renders. *Order-aware path selection among redundant indexes* (the milestone-20 deferral): when paths tie on predicate score — `(a)` vs `(a, b)` under `WHERE a = 1` — the planner now prefers the one whose scan order also serves ORDER BY, eliding the sort; selectivity still outranks order, and zero-score ties stay with the LIMIT-gated full-scan override
- [x] **Milestone 29**: expression values in INSERT and `SELECT DISTINCT`. *INSERT expressions*: `VALUES` entries parse through the full expression grammar — arithmetic, `||`, casts, `CASE`, function calls, scalar subqueries, and the driving case, `VALUES (nextval('s'), ...)` — evaluated per row at execution against an empty scope (column references fail with "no such column"; aggregates and windows are rejected at parse with Postgres' wording); bare literals keep the historical fast path (parse-time values, no evaluation), `$n` placeholders bind inside expressions and describe as the target column's type, and each execution re-evaluates — a prepared insert draws fresh sequence values every run, and the evaluated value is what the ON CONFLICT probe, CHECKs, and RETURNING see. *SELECT DISTINCT*: the projected rows dedup (by the order-preserving tuple encoding — NULLs equal NULLs) before ORDER BY/OFFSET/LIMIT apply, so ORDER BY is restricted to output columns and positions with Postgres' message (a sort key the projection dropped would decide which duplicate survives); one wrapper over every select shape — plain, aggregate, windowed, and UNION arms — plus the inline subquery evaluators: a DISTINCT scalar subquery collapses duplicates before the one-row rule, ANY/ALL and `ARRAY(SELECT ...)` dedup their value lists; `SELECT ALL` parses as the default's explicit spelling, `DISTINCT ON (...)` is rejected outright rather than misread, and EXPLAIN shows the Unique node between the Sort and the scan
Beyond the milestones (production-readiness sweep and app-migration work):

- [x] **Auth & serving**: SCRAM-SHA-256 with channel binding (SCRAM-SHA-256-PLUS) over TLS; server connection cap; `bytdbd -sync` fsync policy; query logging; `pg_stat_activity`
- [x] **Types**: TIMESTAMP / TIMESTAMPTZ / DATE (int64 micros/days, chronological key order) and UUID (16-byte); one-dimensional `text[]` on canonical array-literal text (OID 1009, both wire formats, `array_to_string`/`array_length`, `= ANY(col)`); `jsonb` on canonical document text (OID 3802, binary format, `::jsonb`), with the operator family `->` `->>` `#>` `#>>` `@>` `<@` `?` `?|` `?&` `||` `-`
- [x] **Constraints**: `VARCHAR(n)` enforcement (22001); UNIQUE constraint sugar over unique indexes; foreign keys — MATCH SIMPLE, NO ACTION/RESTRICT, end-of-statement checks, SQLSTATE 23503, schema-side guards — and `ON DELETE CASCADE` with transitive cascades that never bypass a NO ACTION constraint downstream
- [x] **Statements**: TRUNCATE (transactional, RESTART IDENTITY); SET/RESET/SHOW; ALTER TABLE RENAME (table and column); ALTER TABLE OWNER TO (accepted as a no-op — bytdb has no roles — so pg_dump/goose DDL runs unmodified, even in a transaction block); `LIKE`/`ILIKE`; `BETWEEN`; `$n` placeholders in LIMIT/OFFSET; DEFAULT `now()`/`current_date` clock markers evaluated per statement
- [x] **Query shapes**: derived tables, non-recursive WITH CTEs, persistent views (all via one virtual-table mechanism), `IN (SELECT ...)`, and hash joins for unindexed equijoins
- [x] **Replication**: the `replicate` nested package — litestream-style asynchronous shipping of the storage log to any S3-compatible object store (dependency-free SigV4 client included), generation rollover on compaction/restart, retention pruning, point-in-time `Restore`; plus streaming `Engine.BackupTo(io.Writer)` for direct-to-bucket snapshots
- [ ] Later: sequence functions in column DEFAULTs, `SELECT DISTINCT` ordered by select-list *expressions* (output names and positions only today), `DISTINCT ON`, `jsonb_set`/`jsonb_build_*`, jsonb indexing

## Replication

The storage log is append-only between compactions, which makes
incremental replication byte-range shipping — no page shadowing, no
checkpoint racing. The `replicate` package polls the engine's log
cursor (`Engine.LogState` / `Engine.ReadLogRange`, new in btypedb
v0.6) and uploads whatever appended since its watermark as immutable
chunk objects:

```
<prefix>/gen/<generation-id>/<start>-<end>.wlog   one shipped byte range
<prefix>/gen/<generation-id>/manifest.json        completeness marker
```

A compaction rewrites the file, so it (and each process start) rolls a
new *generation* shipped from offset zero; older generations are
pruned once enough newer ones exist. The first time a generation is
shipped in full it gets a `manifest.json` certifying its
complete size, so `replicate.Restore` can pick the newest *complete*
generation — a freshly rolled generation that has only shipped its
first chunk is never chosen over a complete older one (no silent
roll-backward), and a certified generation later missing chunks is
detected rather than restored as a fragment. Because every record is
CRC-framed with batch atomicity, restore just concatenates the chosen
generation's chunks and the result opens exactly like a crash-recovered
local file — a torn or missing tail chunk costs seconds of data, never
validity.

```go
e, _ := bytdb.Open("site.db")

store, _ := s3.New(s3.Config{
    Endpoint:  "https://us-east-1.linodeobjects.com",
    Region:    "us-east-1",
    Bucket:    "db-replicas",
    AccessKey: os.Getenv("S3_ACCESS_KEY"),
    SecretKey: os.Getenv("S3_SECRET_KEY"),
})

r := replicate.New(e, store, replicate.Options{
    Interval: 5 * time.Second, // the data-loss window
    Prefix:   "sites/stjohns",
})
r.Start()
defer r.Close() // final flush; close before e.Close()

// Disaster recovery, elsewhere:
info, _ := replicate.Restore(ctx, store, "sites/stjohns", "site.db")
```

This is recovery, not high availability: a restored node comes up from
object-store state; it does not fail over live. `Replicator.ShipNow`
forces a synchronous ship (after a critical transaction, say), and
`Status()` feeds health endpoints. The `Storage` interface is four
methods, so any store with atomic PUT and ordered listing can stand in
for S3.

The bundled S3 client defaults to bounded dial, TLS-handshake, and
response-header timeouts (not a whole-request timeout, which would cap a
large restore's body transfer), so a black-holed endpoint fails a ship
in seconds and the next tick retries rather than wedging the replicator.
Supply your own `Config.HTTPClient` to override.

## Design notes

- **One writer at a time** (btypedb's model) means serializable
  isolation comes free, SQLite-style. MVCC for concurrent writers is
  explicitly out of scope until a real need appears.
- **btypedb's comparator indexes are not used** — SQL indexes will be
  key ranges, which makes them persistent and replayed from the WAL
  like all other data.
- Columns are typed, so cross-type key ordering never arises within a
  column; the tuple encoding still defines it (by type tag) so that
  corrupt or mixed data fails loudly rather than undefined-ly.
