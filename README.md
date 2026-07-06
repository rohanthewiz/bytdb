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

Milestones 1–16: a working relational store, queryable in SQL — in
process or over the Postgres wire protocol, with transaction blocks
and savepoints, NOT NULL and CHECK constraints, descending index
columns, EXPLAIN, and enough system catalog and expression language
that psql's `\dt`, `\d`, `\d <table>`, `\di`, `\du`, and `\l` render
for real — check constraints included.

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
  small Postgres-flavored dialect over the engine: full DDL, INSERT,
  SELECT/UPDATE/DELETE with a planner that pushes WHERE predicates
  down to point gets and bounded key scans, aggregates with GROUP BY
  and HAVING, INNER/LEFT/CROSS joins executed as index nested loops,
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
`varchar(n)` → string, `boolean` → bool, `bytea` → bytes). As in
Postgres, a quoted literal is untyped until context types it:
`WHERE id = '2'` against an int column compares as the integer 2 (and
errors if the text doesn't parse as one), which is what quote-happy
clients like pgx's simple protocol produce.

Supported statements:

```sql
CREATE TABLE t (id int PRIMARY KEY, ...)          -- or PRIMARY KEY (a, b)
DROP TABLE t
ALTER TABLE t ADD COLUMN c type | DROP COLUMN c
CREATE [UNIQUE] INDEX idx ON t (c, ...)
DROP INDEX idx [ON t]
INSERT INTO t [(cols)] VALUES (...), (...)
SELECT * | items FROM tables [WHERE ...] [GROUP BY ...] [HAVING ...]
       [ORDER BY item [DESC], ...] [LIMIT n] [OFFSET n]
UPDATE t SET c = v, ... [WHERE ...]
DELETE FROM t [WHERE ...]
BEGIN | START TRANSACTION ... COMMIT | END | ROLLBACK | ABORT
SAVEPOINT name | RELEASE [SAVEPOINT] name | ROLLBACK TO [SAVEPOINT] name
```

`FROM` names one table or a left-deep chain of joins — `a [AS] x
[INNER] JOIN b ON x.id = b.a_id`, `LEFT [OUTER] JOIN`, `CROSS JOIN`
(a comma is a cross join) — with qualified column references and
`t.*`. Joins run as nested loops, but equality conjuncts re-bind per
outer row, so an inner table joined on its primary key or an indexed
column is a point get or bounded scan per row, not a full scan.

Aggregates are `COUNT(*)`, `COUNT(x)`, `SUM(x)`, `AVG(x)`, `MIN(x)`,
`MAX(x)` — over a column or any per-row expression (`SUM(a * b)`) —
with SQL semantics throughout: aggregates ignore NULLs (`COUNT(*)`
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
`pg_catalog.pg_namespace`, `pg_class`, `pg_attribute`, `pg_type`,
`pg_index`, `pg_am`, `pg_database`, and `pg_roles` with real rows, a
set of always-empty tables psql probes (`pg_constraint`, `pg_policy`,
the `pg_publication` family, ...), plus `information_schema.tables`
and `columns`, all synthesized from the engine catalog on the fly and
queryable like any tables — WHERE, joins, and aggregates included —
but read-only. Table names may be schema-qualified (`public.t` is
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

Trust auth (user/database accepted and ignored), TLS declined
politely. Transaction blocks work as in Postgres: each connection is
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
- [ ] Later: `count(distinct x)`, `= ANY(array)`, ALTER TABLE ADD/DROP CONSTRAINT

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
