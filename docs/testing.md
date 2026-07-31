# Testing

bytdb's test suite is built around one conviction: **a storage engine's
correctness claims are only as good as its crash tests**. The suite covers
five tiers, from unit tests up to SIGKILL-under-load and simulated power
loss.

## Current coverage

Measured with `go test -count=1 -cover ./...` (Go 1.26.1, 2026-07-30):

| Package | Coverage | What it is |
|---|---:|---|
| `bytdb` (engine) | **75.4%** | Catalog, DDL, DML, indexes, foreign keys, transactions |
| `bytdb/sql` | **87.4%** | Lexer, parser, planner, executor, sessions, syscat |
| `bytdb/tuple` | **97.3%** | Order-preserving key encoding |
| `bytdb/pgwire` | **87.9%** | Wire protocol server, SCRAM auth, TLS |
| `bytdb/replicate` | **80.4%** | Log shipping, generations, restore |
| `bytdb/replicate/s3` | **89.4%** | Dependency-free SigV4 S3 client |
| `btypedb` (storage) | **83.2%** | KV store, WAL, encryption, snapshots, TTL, compaction |

Across the repositories: **561 test functions** and **11 fuzz targets**.
(`pgwire/cmd/bytdbd` is a flag-parsing `main` and is intentionally untested.)

## The five tiers

```mermaid
flowchart TD
    t5["crash & power-loss<br/>SIGKILL a writing child · simulated power cuts ·<br/>every-prefix truncation · compaction crash points ·<br/>encrypted-log recovery"]
    t4["concurrency<br/>snapshots under writes · group commit + compaction ·<br/>concurrent wire connections · replicator under load"]
    t3["property & fuzz<br/>tuple order ↔ bytes.Compare · scan/plan equivalence ·<br/>parser, protocol, executor, and codec fuzzing"]
    t2["semantic gaps vs Postgres<br/>three-valued logic · NaN ordering · overflow ·<br/>error wording and SQLSTATEs · ORM/psql probes verbatim"]
    t1["unit & integration<br/>every statement type · DDL failure injection ·<br/>reentrancy guard · savepoints · TTL across reopen ·<br/>FK cascades · jsonb/array/timestamp codecs"]
    t5 --> t4 --> t3 --> t2 --> t1
```

### Crash safety (the tier that matters most)

- **`TestCrashRecovery`** (`btypedb/crash_test.go`) repeatedly SIGKILLs a child
  process that is writing 8-key batch transactions, then reopens and asserts
  the transactional invariant: every batch present after recovery has *all 8
  members with one identical value* — no torn or interleaved transaction ever
  survives — and the recovered store accepts new writes.
- **Power-loss simulation** (`powerfail_test.go`): a model filesystem where only
  fsync-promoted bytes survive a "cut", which then keeps an *arbitrary torn
  prefix* of in-flight bytes. `TestPowerLossEveryPrefix` replays recovery for
  every possible truncation point of the tail.
- **Compaction crash points** (`powerfailfs_test.go`): a cut during compaction
  leaves either the old or the new complete log, never a hybrid; leftover
  `.compact` temp files are proven dead.
- The bytdb layer adds its own `crash_test.go` plus **DDL failure injection**
  (`ddl_failure_test.go`): a hook simulates the WAL append failing at commit
  time, proving a failed `CREATE INDEX`/`ALTER` leaves no half-published
  schema.

### Property and fuzz tests

The 11 fuzz targets, by layer:

| Target | Invariant it defends |
|---|---|
| `FuzzTupleOrder` | `bytes.Compare(Encode(a), Encode(b))` ≡ semantic comparison — the load-bearing invariant of the whole design |
| `FuzzTupleRoundTrip` | decode ∘ encode is the identity |
| `FuzzParse` | no SQL text can panic the parser |
| `FuzzExec` | no parsed statement can panic the executor |
| `FuzzMessageParse` | no wire bytes can panic the protocol reader |
| `FuzzOpenRecord` | no log bytes can panic or corrupt WAL replay |
| `FuzzOpenEncryptedFile` | malformed encrypted logs fail cleanly, never crash or mis-decrypt |
| `FuzzJSONBCanon` | jsonb canonicalization is stable and round-trips |
| `FuzzTextArrayCanon` | text[] array-literal canonicalization round-trips |
| `FuzzTimestampRoundTrip` | timestamp codec round-trips across text/binary/key encodings |
| `FuzzUUIDRoundTrip` | uuid codec round-trips across text/binary/key encodings |

- `scan_property_test.go` and `sql/plan_property_test.go` cross-check scan and
  planner results against brute-force evaluation over generated data.
- `pgwire/panic_test.go` adds a panic fence test for the executor path.

### Fidelity to Postgres

`sql/semantic_gaps_test.go` and `sql/limits_test.go` pin the deliberate edge
behaviors: `NOT IN` with NULL collapsing to zero rows, NaN sorting last,
LIMIT/OFFSET overflow saturation, error wording (`operator does not exist`,
check-violation messages) matching Postgres closely enough that SQLSTATE
mapping works by message text. `pgwire/orm_test.go` and `sql/psql_test.go`
replay **verbatim** introspection queries captured from psql 17, GORM,
SQLAlchemy, and ActiveRecord.

## Running the suite

```sh
# root module (engine, sql, tuple, replicate) — go.work covers pgwire too
go test ./...

# with coverage
go test -count=1 -cover ./...

# the other modules
(cd pgwire && go test -cover ./...)
(cd ../btypedb && go test -cover ./...)

# fuzzing (continuous; ctrl-C when satisfied)
go test -fuzz=FuzzTupleOrder ./tuple
(cd sql && go test -fuzz=FuzzParse)
```

The crash and power-loss tests run as part of the normal suite — no special
flags — which is why `btypedb`'s suite takes ~15 s.
