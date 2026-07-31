# Session: OCC benchmarks re-run + head-to-head vs 6 engines; README gains Concurrent writes section

- **Session ID:** `819fa68d-114d-4da4-ac84-31a285a6439b`
- **Date:** 2026-07-31, 08:46
- **Deliverable:** README.md + docs/concurrency.md updated on bytdb
  branch **`occ-stage2`** with re-measured OCC numbers and a
  same-machine head-to-head comparison table. No code changes.
- **Prior session:** `2026-0731-0752-yaegi-wasm-compat-check.md`

## Part 1: OCC benchmarks re-run (2026-07-31, Apple M3, 8 writers, SyncNever, median of 3)

Re-ran the Stage 2/3 benchmark suite (`-cpu 8 -count 3`):

| workload (per txn)                       | single-writer | OCC snapshot isolation | OCC serializable |
|------------------------------------------|---------------|------------------------|------------------|
| single-row identity insert (bytdb)       | 11.4µs        | 11.2µs (parity)        | —                |
| 400 reads + identity insert (bytdb)      | 151µs         | 64µs (2.4×)            | 80µs (1.9×)      |
| 2000 reads + 5 writes (btypedb storage)  | 511µs         | 142µs (3.6×)           | —                |
| 20 reads + 5 writes (btypedb storage)    | 18.2µs        | 26.4µs (0.7× — slower) | —                |

vs Stage-3 doc values: single-writer unchanged (150→151µs), OCC came
in slower (SI 53.6→64µs, serializable 66→80µs), so the headline moved
from ~2.3–2.8× to **~1.9–2.4×**. Benchmarks:
`BenchmarkIdentityInsert{Parallel,HeavyTxn}` + `BenchmarkSerializableHeavyTxn`
(bytdb), `BenchmarkParallelUpdates{Light,Heavy}{Serialized,OCC}` (btypedb).

## Part 2: README caught up with OCC (it still described the pre-OCC world)

Stage 4 updated `docs/` but not README.md. Changes:

- **New `## Concurrent writes` section** (after the pgwire section):
  opt-in model, isolation semantics, retry contract (autocommit ×3,
  blocks never), sequence gaps, DDL never-conflicts, the mode
  benchmark table above, the head-to-head table below, pointer to
  `docs/concurrency.md`.
- **SQL transactions paragraph**: no longer says isolation levels
  "parse and are ignored" — no-ops only in default mode; under
  `WithConcurrentWrites` blocks are SI with serializable opt-in
  (`BEGIN ISOLATION LEVEL SERIALIZABLE` / `SET TRANSACTION` first
  statement / session default) and 40001 on conflict; writer lock
  wording scoped to default mode.
- **Status paragraph**: gained the one-sentence OCC mention.
- **Roadmap** "beyond the milestones": new **Concurrent writes (OCC)**
  bullet.
- **Design notes**: "MVCC … explicitly out of scope" replaced —
  the need appeared; validate-at-commit over COW snapshots, storage
  format and single-writer default untouched.
- **docs/concurrency.md** "Choosing a mode": re-measured, dated
  numbers (64/80/151µs, up-to-3.6× storage layer, light-workload
  caveat 26.4 vs 18.2µs).

## Part 3: head-to-head vs Badger, SQLite (×2 drivers), Bolt, Redis, DuckDB

Scratch module (session scratchpad, `dbbench/` — go.mod with `replace`
directives to local bytdb/btypedb, `GOWORK=off`) running the same two
workload shapes on every engine, durability off everywhere, 8 parallel
writers, medians of 3, Apple M3:

| engine                             | single-row insert | 400 point reads + insert |
|------------------------------------|-------------------|--------------------------|
| Badger v4 (LSM, SSI)               | 2.7µs             | 140µs                    |
| SQLite mattn/cgo (WAL, sync OFF)   | 4.9µs             | 818µs                    |
| SQLite modernc pure-Go             | 6.3µs             | 1,592µs                  |
| **bytdb OCC**                      | 11.5µs            | **63µs**                 |
| **bytdb single-writer**            | 11.7µs            | 161µs                    |
| BoltDB v1.5 (NoSync)               | 28.6µs            | 84µs                     |
| Redis 7.2 (localhost, no persist)  | 38.2µs            | 153µs                    |
| DuckDB (file, PK/ART index)        | 73.7µs            | 9,662µs                  |

**Headline: bytdb OCC wins the read-heavy transaction shape outright**
— ahead of Bolt's zero-copy mmap reads and Badger (its closest
architectural cousin). Badger's batched commit pipeline owns cheap
inserts. Methodology per engine (fairness details):

- **bytdb**: engine API (no SQL parse), identity draws; occ writes hit
  fresh keys / reads never written → WriteTxn cannot conflict.
- **SQLite**: `database/sql` + prepared stmts, ids = rowid,
  `_txlock=immediate` (avoids unretryable SQLITE_BUSY_SNAPSHOT on
  read→write upgrade), WAL + synchronous=OFF + busy_timeout. Heavy
  cost = serialized writers × ~2–4µs/statement driver overhead × 400.
  Raw C API would close some of the gap.
- **Bolt**: NoSync, bucket NextSequence ids, big-endian uint64 keys.
- **Badger**: SyncWrites(false), `badger.Sequence` ids (leased,
  non-transactional — same trade as bytdb occ identity), ErrConflict
  retried as workload (only fingerprint false positives possible).
- **Redis**: throwaway server `--port 6399 --save '' --appendonly no`;
  insert = INCR + SET (2 RTTs); heavy = 1 MULTI/EXEC pipeline of 400
  GETs + SET — atomic but **not** an isolated read-then-write txn.
- **DuckDB**: go-duckdb/v2, file-backed shared instance, BIGINT
  PRIMARY KEY (ART) so point reads are indexed, `nextval` ids,
  conflict-containing errors retried. ~24µs/point-lookup is the OLAP
  niche mismatch, expected.

Noise notes: bytdb occ heavy had one 120µs sample (vs 58/63); sqlite
mattn heavy ranged 703µs–1.33ms. Medians quoted.

The head-to-head table + interpretation was added to the README's
Concurrent writes section. Harness lived in the session scratchpad
(not committed); full raw output preserved below.

## Raw head-to-head output

```
goos: darwin  goarch: arm64  cpu: Apple M3  (-cpu 8 -count 3 -benchtime 1s)
BenchmarkInsert_badger-8                  411739   2923 ns/op | 392750 2622 | 437967 2662
BenchmarkHeavy_badger-8                     8667 139657 ns/op |   8688 167251 |  8732 138075
BenchmarkInsert_bolt-8                     42704  29837 ns/op |  42127 28086 | 43833 28649
BenchmarkHeavy_bolt-8                      14305  84323 ns/op |  14259 82609 | 14570 83586
BenchmarkInsert_bytdb_single_writer-8     115504  11682 ns/op | 106270 11807 | 113404 11572
BenchmarkInsert_bytdb_occ-8               114952  11245 ns/op | 112244 11506 | 122550 11698
BenchmarkHeavy_bytdb_single_writer-8        7726 156514 ns/op |   6470 160735 |  6477 162209
BenchmarkHeavy_bytdb_occ-8                 21872  57561 ns/op |  10000 119705 | 19954 62843
BenchmarkInsert_duckdb-8                   16936  73700 ns/op |  16830 78414 | 18140 64827
BenchmarkHeavy_duckdb-8                      128 9802643 ns/op |   121 9600663 |   130 9661993
BenchmarkInsert_redis-8                    31033  39155 ns/op |  31898 38200 | 32160 37829
BenchmarkHeavy_redis-8                      8024 154238 ns/op |   8150 153086 |  7969 152886
BenchmarkInsert_sqlite_mattn-8            215073   4861 ns/op | 211149  4929 | 217963  5119
BenchmarkInsert_sqlite_modernc-8          191169   6253 ns/op | 184272  6254 | 192362  6338
BenchmarkHeavy_sqlite_mattn-8               1507 1334978 ns/op |  1512 702747 |  1305 818264
BenchmarkHeavy_sqlite_modernc-8              670 1546658 ns/op |   651 1592791 |   648 1601435
```

## Context from earlier in the session

- Design-space framing (also useful for docs later): default mode =
  SQLite/Bolt single-writer model; OCC mode = Badger/FoundationDB
  validate-at-commit (serializable read-tracking ≈ FDB read-conflict
  ranges); client contract deliberately mirrors Postgres (40001,
  message/hint, SET TRANSACTION semantics); layering = CRDB-over-Pebble.
- With real durability on (SyncAlways), all engines converge toward
  fsync-dominated ~ms commits (group commit amortizes); the deltas
  above compress accordingly — the tables measure transaction
  machinery, not disks.

## State / next steps

- bytdb `occ-stage2`: this session commits README.md +
  docs/concurrency.md + this doc (pushed by wrap-up). btypedb
  `occ-stage1` unchanged.
- Release decisions still pending (from Stage 4): merge occ-stage1 →
  btypedb main + tag, merge occ-stage2 → bytdb main, bump pins, tag
  root then pgwire.
- Untracked `.cats-todo/` remains the user's local tooling; left
  uncommitted.
