---
name: bytdb-fast-memory-based-db
description: Use bytdb as a fast, embedded, in-process SQL database for Go apps — datasets that fit in memory, durable via WAL (optionally encrypted at rest), queryable through Go APIs, a database/sql driver, or the Postgres wire protocol.
---

# bytdb: fast memory-based embedded database

bytdb is a relational layer over the ordered key-value store
[btypedb](https://github.com/rohanthewiz/btypedb) — the SQLite niche,
not the CockroachDB niche: embedded and in-process, for datasets that
fit in memory, with WAL-backed, fsync-before-ack durability. Reach for
it when a Go app needs fast local SQL without an external server, or a
Postgres-compatible endpoint without running Postgres.

```bash
go get github.com/rohanthewiz/bytdb        # engine + sql + stdlib driver (zero serving deps)
go get github.com/rohanthewiz/bytdb/pgwire # only if serving the wire protocol
```

## Four ways in

**1. Engine API** — typed rows, primary keys, ordered scans, no SQL:

```go
e, err := bytdb.Open("app.db") // one file; WAL durability
defer e.Close()

_, err = e.CreateTable("events", []bytdb.Column{
    {Name: "org", Type: bytdb.TString},
    {Name: "seq", Type: bytdb.TInt},
    {Name: "note", Type: bytdb.TString},
}, "org", "seq") // composite primary key

err = e.Insert("events", "acme", 1, "signup")
row, ok, err := e.Get("events", "acme", 1)
for row, err := range e.ScanRange("events", []any{"acme"}, []any{"acmf"}) {
    _ = row.Col("note") // ordered by key, one contiguous range
}
```

**2. SQL API** — the usual way. `db.Exec` runs any supported
statement; `$1` params bind trailing arguments; `Prepare` for hot
paths:

```go
import bsql "github.com/rohanthewiz/bytdb/sql"

db := bsql.New(e)
_, err = db.Exec(`CREATE TABLE users (id serial PRIMARY KEY, name text,
                  joined timestamptz DEFAULT now(), tags text[], meta jsonb)`)
res, err := db.Exec(`INSERT INTO users (name, meta) VALUES ($1, $2)
                     RETURNING id`, "ada", `{"role":"admin"}`)
id := res.Rows[0][0].(int64)

res, err = db.Exec(`SELECT name FROM users WHERE meta->>'role' = $1`, "admin")
```

**3. database/sql driver** — the stdlib interface, in-process (no
server, no socket, no wire encoding), so sqlx, ORMs, and anything
written against `*sql.DB` works:

```go
import (
    "database/sql"
    _ "github.com/rohanthewiz/bytdb/stdlib"
)

db, err := sql.Open("bytdb", "app.db") // DSN = path [+ options], e.g.
                                       // "app.db?concurrent_writes=true"
```

One path is one engine per process: every `*sql.DB` on it shares the
engine, and each pooled connection gets its own session. A program
already holding a `*bytdb.Engine` should use `stdlib.OpenEngine(e)` —
the file cannot be opened twice. `stdlib.IsRetryable(err)` spots
commit conflicts under concurrent writes.

**4. Postgres wire protocol** — psql, pgx, GORM, `database/sql`
connect as if to Postgres:

```go
err := pgwire.NewServer(bsql.New(e)).ListenAndServe("127.0.0.1:5433")
```

or standalone: `go run github.com/rohanthewiz/bytdb/pgwire/cmd/bytdbd
-db app.db -addr 127.0.0.1:5433` and connect with
`postgres://any@127.0.0.1:5433/any?sslmode=disable`.

## What the SQL dialect covers

Postgres-flavored and deliberately small. Full DDL (tables, ASC/DESC
indexes, sequences, views, ALTER ... ADD/DROP COLUMN / RENAME /
constraints; `ALTER TABLE ... OWNER TO` is accepted as a no-op — bytdb
has no roles, so pg_dump/goose migration DDL runs unmodified, even
inside transaction blocks), INSERT with `ON CONFLICT` upsert and
RETURNING,
SELECT/UPDATE/DELETE with a planner that pushes WHERE conjuncts to
point gets and bounded index scans, joins (nested-loop with index
rebinding; hash join when no index serves an equijoin), aggregates +
GROUP BY/HAVING, window functions with full frame support, WITH CTEs,
derived tables, UNION, `SELECT DISTINCT`, `LIKE`/`ILIKE`, `BETWEEN`,
`ANY`/`ALL`, regex operators,
CASE, casts, correlated subqueries, EXISTS, EXPLAIN, transaction blocks
with savepoints, TRUNCATE, SET/SHOW. `$n` placeholders bind in
WHERE/ON/HAVING, INSERT/UPDATE values, `BETWEEN` bounds, and
`LIMIT`/`OFFSET` counts.

Types: `int`, `float`, `text`/`varchar(n)` (length enforced), `bool`,
`bytea`, `timestamp[tz]`, `date`, `uuid`, `text[]`, `jsonb` — jsonb
with `-> ->> #> #>> @> <@ ? ?| ?& || -` and canonical-text equality.

Constraints: PRIMARY KEY, NOT NULL, CHECK, UNIQUE, and foreign keys
(MATCH SIMPLE; NO ACTION/RESTRICT or `ON DELETE CASCADE` — cascades
are transitive and still blocked by a NO ACTION constraint
downstream). Errors carry Postgres wording and SQLSTATEs.

`DEFAULT` takes constants plus `now()`/`current_date` (evaluated once
per INSERT statement). `serial` draws from a durable per-column
counter; read generated keys back with `RETURNING id`.

## Concurrent writes (opt-in)

The default engine is single-writer: serializable by construction,
writes queue behind the writer lock, reads never block. Opting in
lets write transactions run concurrently:

```go
e, err := bytdb.Open("app.db", bytdb.WithConcurrentWrites())
```

Writes then use optimistic concurrency control at snapshot isolation:
each transaction runs against its own snapshot and validates at
commit — first committer wins, and a losing COMMIT fails with
`bytdb.ErrTxConflict` (SQLSTATE 40001 over the wire, with Postgres's
retry hint) for the client to retry from BEGIN. Autocommit statements
are retried internally (3 attempts) and rarely surface conflicts.
`BEGIN ISOLATION LEVEL SERIALIZABLE`, `SET TRANSACTION`, or
`default_transaction_isolation` opts a transaction up to full
SERIALIZABLE via read-set validation. Two side effects of the mode:
sequence/identity draws become non-transactional and gap-tolerant
(Postgres semantics — in the default mode they are transactional and
gapless-on-rollback), and DDL escalates to a brief exclusive stall
instead of validating, so it always makes progress and never returns
40001.

## Backup and replication

`e.Backup(path)` writes a consistent point-in-time copy without
blocking anyone (restore = open the copy); `e.BackupTo(w)` streams the
same thing to any `io.Writer` — pipe it straight into an object-store
upload for snapshot backups.

For continuous recovery, the `replicate` package ships the storage log
to any S3-compatible object store (Linode Object Storage, IDrive e2,
MinIO, AWS) litestream-style, with a stdlib-only S3 client included:

```go
import (
    "github.com/rohanthewiz/bytdb/replicate"
    "github.com/rohanthewiz/bytdb/replicate/s3"
)

store, err := s3.New(s3.Config{
    Endpoint: "https://us-east-1.linodeobjects.com", Region: "us-east-1",
    Bucket: "db-replicas", AccessKey: key, SecretKey: secret,
})
r := replicate.New(e, store, replicate.Options{
    Interval: 5 * time.Second, // the data-loss window
    Prefix:   "sites/mysite",  // many databases can share one bucket
})
r.Start()
defer r.Close() // final flush — close before e.Close()

// Cold start / disaster recovery, before bytdb.Open:
info, err := replicate.Restore(ctx, store, "sites/mysite", "app.db")
```

`r.ShipNow(ctx)` forces a synchronous ship after a critical write;
`r.Status()` feeds health endpoints. This is recovery, not live
failover: a restored node comes up from object-store state with at
most one Interval of loss. The `Storage` interface is four methods
(Put/Get/List/Delete), so a fake or another store slots in easily.

## Encryption at rest

Encrypt the write-ahead log with a 32-byte key you supply — the file on
disk (and therefore every replica chunk and backup) is AES-256-GCM
ciphertext, while rows stay plaintext in memory, so queries, ordering,
and scans pay no crypto cost:

```go
key := loadKey() // 32 bytes from env / file / KMS — bytdb never sources or persists it
e, err := bytdb.Open("app.db", bytdb.WithEncryptionKey(key))
```

Value-only scope: each row's column values are sealed, but the
primary-key bytes stay cleartext on disk — aimed at databases whose PKs
are surrogate IDs/UUIDs, not sensitive natural keys. Because replication
and backup ship raw log bytes, `replicate` chunks and `Backup`/`BackupTo`
output become ciphertext automatically; a follower or a `Restore` target
needs the *same* key to `Open` and serve — **lose the key and the data,
and every backup, is unrecoverable**.

Reopening enforces the key, all before any row is read: wrong key →
`btypedb.ErrWrongKey`, missing key → `btypedb.ErrKeyRequired`, a key on a
plaintext db → `btypedb.ErrNotEncrypted`. There is no in-place conversion
or online key rotation yet — migrate (or rotate) by copying rows into a
fresh db opened with the new option.

`bytdbd` takes the key out-of-band, never on argv:
`-encryption-key-file <path>` or `-encryption-key-env <NAME>` (32 raw
bytes, 64 hex chars, or base64 of 32).

## Gotchas

- **One writer at a time — by default**: a writable transaction block
  holds the writer lock from BEGIN to COMMIT — keep blocks short.
  Reads never block. Under `WithConcurrentWrites` blocks don't queue,
  but a long block risks losing commit validation (40001) instead —
  keep them short either way, and be ready to retry from BEGIN.
- **DDL cannot run inside a transaction block**; every schema change
  is its own transaction. TRUNCATE is DML and does roll back.
- **ALTER TABLE ADD COLUMN with DEFAULT / NOT NULL** requires an
  empty table (no backfill machinery, by design).
- `ON UPDATE CASCADE` and `SET NULL/DEFAULT` FK actions are rejected
  at parse — only `ON DELETE CASCADE` exists. Index the child FK
  columns or cascade probes are full scans per deleted parent key.
- **INSERT VALUES entries are full expressions** (`nextval('s')`
  works) but column references don't resolve there.
- `now()` is statement-frozen, not transaction-frozen as in Postgres.
- Sequence/identity allocation is **transactional in the default mode**
  (a rollback reuses the value) and gap-tolerant Postgres-style under
  `WithConcurrentWrites` (a rollback burns it).
- Errors are [serr](https://github.com/rohanthewiz/serr) structured
  errors: log with `logger.LogErr(err, "context")`, render user-facing
  text with `bytdb.ErrText(err)`.
- Not implemented: RIGHT/FULL joins, triggers, NUMERIC(p,s), arrays
  beyond `text[]`, jsonb indexing, COPY, live HA/failover (replication
  is async recovery, see above). Datasets must fit in RAM.

## Verifying changes

When developing bytdb itself, use the `verify` skill: it serves a
scratch db over the wire protocol and drives it with a pgx client.
