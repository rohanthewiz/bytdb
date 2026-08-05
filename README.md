# bytdb

**An embedded, Postgres-compatible SQL database in pure Go.**

bytdb gives your Go application a real relational database that lives
*inside* the process — no server to run, no cgo to build, no socket to
cross. Datasets that fit in memory get in-memory speed; a write-ahead
log with power-loss-tested durability (optionally encrypted at rest)
makes them safe; and the whole thing speaks enough Postgres that psql,
pgx, `database/sql`, and GORM connect and just work.

It occupies the SQLite niche — one file, one process, zero ops — but
speaks the Postgres dialect your tools already know.

## Key features

- **Embedded and pure Go** — `go get` it, open a file, query. No cgo,
  no external server, no deployment step.
- **Three front doors** — a `database/sql` driver (`"bytdb"`), a direct
  SQL API, and a Postgres wire-protocol server (`pgwire`) that psql,
  pgx, ORMs, and anything Postgres-speaking can connect to, with
  SCRAM-SHA-256(-PLUS) auth over TLS.
- **Real SQL** — joins (index nested loop and hash), window functions
  with full frame support, CTEs, views, subqueries, upsert
  (`ON CONFLICT`), `RETURNING`, foreign keys with `ON DELETE CASCADE`,
  CHECK/UNIQUE/NOT NULL constraints, SERIAL identity columns,
  sequences, transactions with savepoints, and `EXPLAIN`.
- **Postgres-grade types** — `timestamp[tz]`, `date`, `uuid`, `text[]`,
  and `jsonb` with the everyday operator family
  (`->` `->>` `#>` `#>>` `@>` `<@` `?` `?|` `?&` `||` `-`).
- **Introspectable** — a virtual `pg_catalog` and `information_schema`
  answer the queries ORMs and psql actually send: `\dt`, `\d <table>`,
  `\di`, `\du`, `\l`, and GORM's probes render for real.
- **Fast where it counts** — read-heavy transactions beat every embedded
  store we benchmarked (numbers below, harness in the repo).
- **Serializable by default, concurrent when you want it** —
  single-writer serializable out of the box; `WithConcurrentWrites`
  opts into parallel optimistic writers with SQLSTATE 40001 conflicts,
  exactly as Postgres clients expect.
- **Durable and recoverable** — fsync-before-ack WAL, optional
  AES-256-GCM encryption at rest, streaming backups, and
  litestream-style asynchronous replication to any S3-compatible
  object store with point-in-time restore.

## Benchmarks

Head-to-head against other embedded (and near-embedded) stores: same
machine, same date (2026-08-05, Apple M1 Pro), durability off everywhere —
so this compares transaction machinery, not disks. SQLite and DuckDB
run through `database/sql` with prepared statements, Redis over
localhost TCP with persistence off; medians of 3, 8 parallel writers.

| engine                              | single-row insert | 400 point reads + insert |
|-------------------------------------|-------------------|--------------------------|
| Badger (LSM, SSI txns)              | 4.2µs             | 146µs                    |
| SQLite, mattn/cgo (WAL, sync OFF)   | 8.1µs             | 1,212µs                  |
| SQLite, modernc pure-Go             | 11.7µs            | 2,118µs                  |
| **bytdb single-writer**             | 14.3µs            | 189µs                    |
| **bytdb OCC**                       | 14.7µs            | **51µs**                 |
| Redis (localhost, no persistence)   | 21.9µs            | 222µs                    |
| BoltDB (NoSync)                     | 36.7µs            | 106µs                    |
| DuckDB (file, PK index)             | 97.9µs            | 10,015µs                 |

Badger's batched commit pipeline owns cheap inserts; bytdb's OCC mode
wins the read-heavy transaction shape outright — ahead of Bolt's
zero-copy mmap reads and Badger's SSI, and 24× ahead of the fastest
SQLite. (SQLite serializes writers while paying per-statement driver
overhead 400 times under the lock; Redis's heavy row is one MULTI/EXEC
pipeline round trip — atomic, but not an isolated read-then-write
transaction; DuckDB's per-point-lookup cost is the OLAP niche
mismatch, not a defect.)

The harness is committed in this repo — `bench/head2head_test.go`
documents each engine's exact mapping, raw output is at
`bench/head2head-results.txt`, and the table above is its verbatim
output. Validate it on your own hardware:

```sh
./bench/head2head.sh          # starts/stops a throwaway Redis for you
# or directly (Redis rows skip cleanly if no server on :63799):
cd bench && go test -run '^$' -bench 'Insert_|Heavy_' -cpu 8 -count 3
```

The bench module resolves bytdb to the checkout you are sitting in, so
you benchmark exactly the code you cloned; competitor libraries are
pinned at their latest releases in `bench/go.mod`.

And bytdb against itself — what opting into concurrent writes buys
(same machine and date, 8 parallel writers, `SyncNever`, median of 3):

| workload (per txn)                        | single-writer | OCC snapshot isolation | OCC serializable |
|-------------------------------------------|---------------|------------------------|------------------|
| single-row identity insert                | 14.6µs        | 14.8µs (1.0×)          | —                |
| 400 reads + identity insert (SQL engine)  | 195µs         | 79µs (**2.5×**)        | 101µs (1.9×)     |
| 2000 reads + 5 writes (btypedb storage)   | 634µs         | 125µs (**5.1×**)¹      | —                |
| 20 reads + 5 writes (btypedb storage)     | 24.6µs        | 30.4µs (0.8×)          | —                |

¹ This row writes 5 *random* keys per transaction over a 100k keyspace,
so real conflicts and retries occur and the OCC ratio is
conflict-dependent run to run (all three samples landed 124–131µs
here; conflict-heavier sessions have measured as low as ~2×). The
other rows write fresh keys and are stable within a few percent.

Long transactions are where OCC pays: read work that used to sit under
the writer lock overlaps instead. Short transactions bound the win,
and OCC's per-commit overhead can make it *slower* on light workloads,
as the last row shows. Details and the full concurrency contract:
[docs/concurrency.md](docs/concurrency.md).

## Show me the code

### `database/sql` — the interface Go already speaks

```go
import (
    "database/sql"
    "time"

    _ "github.com/rohanthewiz/bytdb/stdlib"
)

db, err := sql.Open("bytdb", "app.bytdb")
defer db.Close()

_, err = db.Exec(`CREATE TABLE users (id serial PRIMARY KEY, name text, joined timestamp)`)
_, err = db.Exec(`INSERT INTO users (name, joined) VALUES ($1, $2)`, "ada", time.Now())

var name string
var joined time.Time
err = db.QueryRow(`SELECT name, joined FROM users WHERE id = $1`, 1).Scan(&name, &joined)
```

sqlx, ORMs, migration tools, and anything written against `*sql.DB`
work unchanged — with no server, no socket, and no wire encoding.

### SQL, straight from Go

```go
import (
    "github.com/rohanthewiz/bytdb"
    bsql "github.com/rohanthewiz/bytdb/sql"
)

e, err := bytdb.Open("app.db")
defer e.Close()
db := bsql.New(e)

_, err = db.Exec(`CREATE TABLE users (id int PRIMARY KEY, name text, age int, city text)`)
_, err = db.Exec(`CREATE INDEX by_city ON users (city)`)
_, err = db.Exec(`INSERT INTO users VALUES (1, 'ada', 36, 'london'), (2, 'grace', 45, 'nyc')`)

res, err := db.Exec(`SELECT name, age FROM users WHERE city = 'london' ORDER BY age DESC LIMIT 10`)
for _, row := range res.Rows {
    fmt.Println(row[0], row[1]) // values typed per res.Types
}

// Parameters and prepared statements
res, err = db.Exec(`SELECT name FROM users WHERE city = $1 AND age > $2`, "london", 30)

st, err := db.Prepare(`INSERT INTO users VALUES ($1, $2, $3, $4)`)
_, err = st.Exec(3, "alan", 41, "london") // re-executable; safe for concurrent use
```

The dialect covers the queries real applications write:

```sql
-- Upsert with RETURNING
INSERT INTO users (id, name, city) VALUES (2, 'grace', 'sf')
ON CONFLICT (id) DO UPDATE SET city = excluded.city
RETURNING id, city;

-- Aggregates over expressions, HAVING, ordinals
SELECT age / 10 AS decade, count(*) AS n, max(age)
FROM users WHERE age > 18
GROUP BY age / 10 HAVING count(*) >= 2
ORDER BY n DESC, decade LIMIT 3;

-- Window functions with frames, composing with GROUP BY
SELECT city, sum(age) AS total,
       rank() OVER (ORDER BY sum(age) DESC) AS rnk
FROM users GROUP BY city;

-- CTEs, joins, jsonb
WITH londoners AS (SELECT * FROM users WHERE city = 'london')
SELECT u.name, o.doc ->> 'status'
FROM londoners u JOIN orders o ON o.user_id = u.id
WHERE o.doc @> '{"paid": true}';
```

### Serve it over the Postgres wire

```go
import (
    "github.com/rohanthewiz/bytdb/pgwire"
    bsql "github.com/rohanthewiz/bytdb/sql"
)

err := pgwire.NewServer(bsql.New(e)).ListenAndServe("127.0.0.1:5433")
```

or as a standalone server:

```sh
go run github.com/rohanthewiz/bytdb/pgwire/cmd/bytdbd -db app.db -addr 127.0.0.1:5433
psql "postgres://any@127.0.0.1:5433/any?sslmode=disable"
```

psql's `\dt`, `\d <table>`, `\di`, `\du`, and `\l` render for real;
pgx works in both simple and extended protocol modes, text and binary
formats, nested transactions included.

### The engine API — ordered scans SQL doesn't express

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

### Encrypt it, replicate it

```go
// AES-256-GCM at rest: one option, zero query-time cost.
key := loadKey() // 32 bytes from env / file / your KMS
e, err := bytdb.Open("app.db", bytdb.WithEncryptionKey(key))

// Litestream-style async replication to any S3-compatible store.
store, _ := s3.New(s3.Config{
    Endpoint: "https://us-east-1.linodeobjects.com", Region: "us-east-1",
    Bucket: "db-replicas",
    AccessKey: os.Getenv("S3_ACCESS_KEY"), SecretKey: os.Getenv("S3_SECRET_KEY"),
})
r := replicate.New(e, store, replicate.Options{
    Interval: 5 * time.Second, // the data-loss window
    Prefix:   "sites/stjohns",
})
r.Start()
defer r.Close()

// Disaster recovery, elsewhere:
info, _ := replicate.Restore(ctx, store, "sites/stjohns", "site.db")
```

Encrypted logs flow through to replicas and backups automatically —
what lands in the bucket is ciphertext.

## What's supported

The SQL surface at a glance:

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

Plus, throughout the expression language: CASE, IN, `BETWEEN`,
`LIKE`/`ILIKE`, regex matches (`~`, `!~`, ...), `::` casts, arithmetic
and `||`, `op ANY(...)`/`op ALL(...)`, correlated scalar subqueries,
EXISTS, `IN (SELECT ...)`, UNION [ALL], derived tables, and window
functions (ranking, value, and aggregate — DISTINCT included — with
explicit ROWS/RANGE/GROUPS frames, RANGE offsets on the sort key, and
frame EXCLUDE).

Types: int, float, string/text, bool, bytes, `timestamp`,
`timestamptz`, `date`, `uuid`, `text[]`, and `jsonb` — the date/time
and UUID types stored so they order chronologically in keys and
indexes, `jsonb` in a canonical rendering so plain `=` is document
equality.

The full feature inventory — every constraint, function, and
statement, with its exact semantics — lives in
[docs/features.md](docs/features.md); the milestone-by-milestone
build history is preserved in the session logs under
[ai_docs/claude_sessions](ai_docs/claude_sessions).

## Architecture

bytdb is a relational layer over
[btypedb](https://github.com/rohanthewiz/btypedb), built the way
CockroachDB layers SQL over Pebble: tables, rows, and secondary
indexes are all encoded into **one ordered key space**, so relational
operations become key scans against an ordered store that already
provides atomic batches, O(1) copy-on-write snapshot reads, range
deletes, and power-loss-tested durability.

The layers, bottom up:

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
- **SQL frontend** — the `sql` package parses, plans, and executes the
  dialect over the engine: a hand-rolled lexer and recursive-descent
  parser (zero new dependencies), a planner that pushes WHERE
  predicates down to point gets and bounded key scans, and an executor
  over engine transactions.
- **Postgres wire protocol** — the `pgwire` module (its own go.mod, so
  the core library keeps zero serving dependencies) serves a database
  to psql, pgx, and `database/sql`: simple and extended query
  protocols, text and binary formats, inferred parameter types, and
  structured errors with SQLSTATE codes and error positions.
- **`database/sql` driver** — the `stdlib` package (stdlib-only, so it
  stays in the core module) registers bytdb as the driver `"bytdb"`,
  reaching an embedded database in process: one engine per file, one
  bytdb Session per pooled connection, isolation levels mapped onto
  `BEGIN`, and the date/time and UUID types decoded back out of their
  integer and byte representations.
- **system catalog** — virtual `pg_catalog` and `information_schema`
  tables synthesized from the engine catalog, so clients and ORMs
  introspect with the queries they already send (GORM's `HasTable`,
  the pg_attribute/pg_type join, `SELECT version()`, and psql's
  backslash commands verbatim), all read-only and flowing through the
  ordinary join and aggregate machinery.

### How it maps onto the key space

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

## The SQL dialect, in detail

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
counts once per group) — with SQL semantics throughout: aggregates
ignore NULLs (`COUNT(*)` counts rows), NULL group values form one
group, an ungrouped aggregate query returns exactly one row even over
zero rows, and `HAVING` filters groups. A `GROUP BY` key is a column,
an expression, or an integer ordinal naming a select-list position
(`GROUP BY 1`), and select items, `HAVING`, and `ORDER BY` take
expressions over the grouped data — group keys, aggregate results,
and anything built from them.

WHERE and HAVING are boolean expressions: predicates — `column op
literal` (`=`, `!=`, `<>`, `<`, `<=`, `>`, `>=`, either operand
order) or `column IS [NOT] NULL` — combined with `AND`, `OR`, and
`NOT` (standard precedence, parentheses group), evaluated with SQL
three-valued logic, so `NOT v = 1` still excludes NULL rows. The
planner pushes filters down: equality on every primary-key column
becomes a point `Get`; an equality prefix (plus at most one range
column) of the primary key or of a secondary index becomes a bounded
ordered scan with early termination — using only predicates that are
top-level `AND` conjuncts (anything under `OR`/`NOT` stays
filter-only); everything else falls back to a filtered full scan. The
whole condition is also re-checked row by row, so pushdown only
narrows what is visited — correctness never depends on it. ORDER BY
that matches a scan's key order skips the sort (and, under a LIMIT,
stops early), including fully-reversed matches run as backward scans.

For introspection there is a virtual system catalog:
`pg_catalog.pg_namespace`, `pg_class` (sequences and views included),
`pg_attribute`, `pg_attrdef`, `pg_type`, `pg_index`, `pg_sequence`,
`pg_constraint` (checks and foreign keys, `confdeltype` included),
`pg_am`, `pg_database`, `pg_roles`, and `pg_stat_activity` with real
rows, a set of always-empty tables psql probes (`pg_policy`, the
`pg_publication` family, ...), plus `information_schema.tables`,
`columns`, and `sequences`, all synthesized from the engine catalog
on the fly and queryable like any tables — WHERE, joins, and
aggregates included — but read-only. Table names may be
schema-qualified (`public.t` is `t`; bare `pg_class` resolves because
pg_catalog is on the search path). `SELECT` works without FROM
(`SELECT 1`), a small whitelist of zero-argument functions folds to
constants (`version()`, `current_schema()`, `current_database()`,
...), the introspection functions psql calls evaluate for real
(`format_type`, `pg_get_indexdef`, `pg_get_userbyid`,
`pg_table_is_visible`, ...), and `ORDER BY 1, 2` addresses
select-list positions. That covers real client probes verbatim:

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
everything; `RELEASE` drops the mark and keeps the work. `READ ONLY`
is honored, and isolation levels do what the engine's mode implies: in
the default single-writer mode every transaction is serializable, so
requesting a level is a no-op; under `WithConcurrentWrites` blocks run
at snapshot isolation, and `BEGIN ISOLATION LEVEL SERIALIZABLE` (or
`SET TRANSACTION` as the block's first statement, or the session
default) opts a block up to serializable. In the default mode a
writable block holds the engine's writer lock from `BEGIN` to its end
(other sessions' writes wait; reads never do); under concurrent
writes blocks commit optimistically instead, and a losing commit
surfaces as SQLSTATE 40001 (see
[Concurrent writes](#concurrent-writes)). DDL cannot run inside a
block since every schema change is its own transaction.

Errors are [serr](https://github.com/rohanthewiz/serr) structured
errors: `%v` prints just the message, `bytdb.ErrText(err)` renders it
with the structured attributes for user-facing surfaces — `wrong
number of parameters (want: 1, got: 0)` — and a serr-aware logger
gets the full context including code locations.

## The `database/sql` driver, in detail

**DSN.** A filesystem path, optionally followed by engine options —
`app.bytdb`, `file:app.bytdb`, `/var/lib/app.bytdb?sync=never`. The
options are `sync=never` (leave WAL fsyncs to the OS),
`concurrent_writes=true` (OCC instead of the single-writer lock), and
`encryption_key=<64 hex digits>`. An unrecognized option is an error
rather than a silent no-op: a typo in a durability or encryption
setting should not be something you discover from its consequences.
bytdb has no in-memory mode, so a path is always a real file, created
on first open.

**One engine per file.** btypedb takes the database file for itself, so
every `*sql.DB` on a path shares one engine and each pooled connection
gets its own Session — one engine, many sessions, the same shape
pgwire serves. Opening the same path twice with different options is an
error rather than a silent adoption of whichever set got there first.
A program that already holds an `*bytdb.Engine` should use
`stdlib.OpenEngine(e)` instead of a DSN, since the file cannot be
opened twice; `stdlib.Engine(ctx, db)` goes the other way, reaching the
engine behind a `*sql.DB` for the ordered range and index scans SQL
does not express.

**Transactions.** `BeginTx` maps `database/sql`'s isolation levels onto
`BEGIN`'s — the four Postgres levels pass through (READ UNCOMMITTED
folding into READ COMMITTED), and the levels `database/sql` defines
beyond those are refused rather than silently downgraded. Under
`WithConcurrentWrites` a losing commit returns `bytdb.ErrTxConflict`,
matched with `errors.Is` or `stdlib.IsRetryable`, and retried from the
top; autocommit statements are retried inside bytdb and never surface
one.

**Types.** The date/time and UUID types ride on `int64` and `[]byte`
runtime representations so they sort in the key encoding, so the driver
decodes them back on the way out: a `timestamp` scans into a
`time.Time`, a `date` and a `uuid` into their text forms, `text[]` and
`jsonb` into their canonical literals. `ColumnTypeDatabaseTypeName` and
`ColumnTypeScanType` report both sides for the ORMs that ask.

## The wire protocol, in detail

The embedded API maps directly onto the protocol: `Prepare` is Parse,
`Stmt.Describe` answers Describe — parameter types are inferred from
the column each `$n` compares against or inserts into, and the result
shape (`Result`'s columns + types) is computed from the catalog
without executing — and `Stmt.Exec` is Bind/Execute. Both the simple
(`Q`, multi-statement) and extended protocols work, in text and
binary formats for all column types. Errors cross the wire
structurally from serr fields: the parser's byte offset becomes the
error Position (psql's `LINE 1: ... ^` caret), structured attributes
become DETAIL, and stable message texts map to SQLSTATE codes
(syntax_error, undefined_table, unique_violation, ...).

Auth is trust by default (user/database accepted and ignored); with a
credentials registry set, SCRAM-SHA-256 runs for real — RFC 5802 with
channel binding (SCRAM-SHA-256-PLUS) over TLS. `bytdbd` adds
`-max-conns` (a connection cap), `-sync always|never` (WAL fsync
policy), and query logging. Transaction blocks work as in Postgres:
each connection is a `sql.Session`, `ReadyForQuery` reports the real
status (idle / in transaction / failed), redundant `BEGIN`/`COMMIT`
raise `NoticeResponse` warnings, and a dropped connection rolls back
its open block. Savepoints work over the wire too — pgx's nested
transactions ride on them. Cancellation, portal suspension, and COPY
are not implemented. The end-to-end tests drive a real pgx v5
client — including pgx's statement cache and its simple protocol
mode, which renders every argument as a quoted literal and so
exercises the dialect's untyped-literal coercion.

## Concurrent writes

The default engine admits one writer at a time (SQLite-style), which
makes every transaction serializable for free. Opening with
`WithConcurrentWrites()` switches to optimistic concurrency: writers
build transactions in parallel against O(1) COW snapshots and
validate at commit, so a transaction fails — `bytdb.ErrTxConflict`
embedded, SQLSTATE **40001** on the wire — only when a committed
neighbor actually touched a key it depends on. Transactions then run
at snapshot isolation; `BEGIN ISOLATION LEVEL SERIALIZABLE` (or a
`SET TRANSACTION` / session default) opts a block up to full
serializability by also tracking reads. Autocommit statements absorb
up to three silent retries before a 40001 reaches the client;
explicit blocks are never replayed by the server — the client
retries, exactly as with Postgres. Identity draws and `nextval`
become non-transactional in this mode (gaps on rollback, as in
Postgres) so parallel inserts into one table don't collide on the
counter key, and DDL always makes progress — it never returns 40001.

Hot single rows are the worst case for optimism: if a counter row
thrashes even through the built-in retries, restructure the write
rather than spinning on 40001. The full contract — who retries what,
isolation-level semantics, sequence gaps, DDL — is in
[docs/concurrency.md](docs/concurrency.md).

## Replication

The storage log is append-only between compactions, which makes
incremental replication byte-range shipping — no page shadowing, no
checkpoint racing. The `replicate` package polls the engine's log
cursor (`Engine.LogState` / `Engine.ReadLogRange`) and uploads
whatever appended since its watermark as immutable chunk objects:

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

This is recovery, not high availability: a restored node comes up from
object-store state; it does not fail over live. `Replicator.ShipNow`
forces a synchronous ship (after a critical transaction, say), and
`Status()` feeds health endpoints. The `Storage` interface is four
methods, so any store with atomic PUT and ordered listing can stand in
for S3; the bundled S3 client is a dependency-free SigV4
implementation with bounded dial, TLS-handshake, and response-header
timeouts, so a black-holed endpoint fails a ship in seconds and the
next tick retries rather than wedging the replicator. Supply your own
`Config.HTTPClient` to override. Streaming `Engine.BackupTo(io.Writer)`
covers direct-to-bucket snapshots.

More in [docs/replication.md](docs/replication.md).

## Encryption at rest

The write-ahead log can be encrypted with a 32-byte key you supply.
Open with `WithEncryptionKey` and the on-disk log becomes AES-256-GCM
ciphertext, while rows stay plaintext in memory — the in-memory B-tree
orders by the decoded key, so queries, ordering, and range scans pay no
crypto cost.

Each record's value is sealed as `nonce ‖ AES-256-GCM(value)` with a
fresh random nonce and the op and key bound as additional data; the
`op|klen|vlen|…|crc32` framing stays outside the ciphertext, so
torn-tail detection and crash recovery run before any key is needed and
the CRC still catches bit-rot. Encrypted logs carry a v2 header whose
16-byte prefix is byte-identical to the plaintext header (so an older,
encryption-unaware binary rejects the file cleanly) plus a key-check
value that makes a wrong key fail fast, before any record is read.

**Value-only scope.** Column *values* are encrypted; the tuple-encoded
primary key stays cleartext on disk. It is aimed at databases whose PKs
are surrogate IDs/UUIDs — primary-key column values are not protected.
This protects data at rest (a stolen disk, backup, or object-store
copy), not a running process's memory.

Because replication and backup ship raw log bytes, `replicate` chunks
and `Backup`/`BackupTo` output are ciphertext automatically, and a
follower or a `Restore` target needs the **same key** to `Open` and
serve. Lose the key and the data — and every backup — is unrecoverable.

Reopening enforces the key, before any row is read: a wrong key returns
`btypedb.ErrWrongKey`, a missing key `btypedb.ErrKeyRequired`, and a key
on a plaintext database `btypedb.ErrNotEncrypted`. There is no in-place
conversion or online key rotation yet — migrate (or rotate) by copying
rows into a fresh database opened with the new option.

`bytdbd` takes the key out-of-band, never on the command line (which
would leak through `ps`): `-encryption-key-file <path>` or
`-encryption-key-env <NAME>`, holding 32 raw bytes, 64 hex characters,
or base64 of 32 bytes.

More in [docs/security.md](docs/security.md).

## Design notes

- **One writer at a time** (btypedb's default) means serializable
  isolation comes free, SQLite-style. The concurrent-writer need did
  appear, and it's the opt-in OCC mode above — validate-at-commit
  over COW snapshots rather than MVCC version chains, so the storage
  format and the single-writer default are untouched.
- **btypedb's comparator indexes are not used** — SQL indexes are
  key ranges, which makes them persistent and replayed from the WAL
  like all other data.
- Columns are typed, so cross-type key ordering never arises within a
  column; the tuple encoding still defines it (by type tag) so that
  corrupt or mixed data fails loudly rather than undefined-ly.

## Documentation

The `docs/` site goes deeper on everything here:
[architecture](docs/architecture.md) ·
[features](docs/features.md) ·
[benchmarks](docs/benchmarks.md) ·
[concurrency](docs/concurrency.md) ·
[replication](docs/replication.md) ·
[security](docs/security.md) ·
[testing](docs/testing.md) ·
[gotchas](docs/gotchas.md)
