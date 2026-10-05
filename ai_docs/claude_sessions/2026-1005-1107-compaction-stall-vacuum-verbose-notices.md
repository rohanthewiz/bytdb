# Compaction writer stall, VACUUM VERBOSE, notice severities

- **Session:** `a9c903b8-69f1-41eb-ad6f-5a2a6d79c664`
- **Date:** 2026-10-05
- **Scope:** The user asked for next-list N-023 and the rest of Open
  (N-024, N-025). All three closed. N-023 needed a btypedb release,
  **v0.9.0**, which also carries the API N-024 needed. Released as bytdb
  and pgwire **v0.21.0**.

## 1. N-023: compaction's writer pause (btypedb `902e317`)

- **The proposed fix wasn't enough.** N-023 suggested syncing the snapshot
  before phase B. A probe (1 KB values, writer looping `Set` under
  `SyncNever`, darwin) made the worst stall *worse* with only that change.
  Instrumenting phase B showed why: the tail it copied under the lock was
  ~400 MB at a 900 MB database, because the tail is every append made while
  the snapshot streams. The snapshot fsync itself took ~20 ms by then (the
  OS had written most of it back).
- **Fix: an unlocked catch-up loop** (`compact.go`). After
  `writeSnapshot`, Compact repeatedly reads `walSize` under `RLock`,
  copies `[copied, walSize)` from the live log into the temp file, and
  fsyncs. It stops once at most `compactCatchUpSlack` (1 MiB) is left, or
  after `compactCatchUpRounds` (4), so a writer that outruns the disk can't
  keep it going. Phase B splices and fsyncs only that residue, and skips
  the fsync when there is none. Reading `db.file` without `db.mu` is the
  `BackupTo` / `ReadLogRange` pattern: `compactMu` pins the handle, and the
  bytes below a locked read of `walSize` are immutable.
- **Two policy side effects, both neutralized:**
  - Appends made during the catch-up used to wait for phase B and land in
    the new file as growth. Now they land in the spliced tail, which made
    `baseSize` swallow them and pushed back the next auto-compaction.
    `baseSize` is now `header + snapshot + (streamEnd - tailStart)`, so they
    still count as growth.
  - A burst that ends during the catch-up has no later append to run
    `maybeAutoCompact`. A successful Compact (and the auto-compaction
    goroutine, after clearing `compacting`) now re-checks the policy.
    It doesn't re-check after a failure, to avoid spinning on a persistent
    error.
  - `TestAutoCompact` failed 6/20 runs before these fixes. It now waits for
    chained runs to settle before `Close` (`waitAutoCompactIdle`), since
    Close stops new runs and would otherwise test that race.
- **Measured** (900 MB, `SyncNever` writer). Phase-B lock hold: v0.8.0
  340–540 ms, v0.9.0 16–32 ms. Writers can still stall 100–450 ms outside
  the lock when they outrun the disk, probably from the kernel throttling
  writes. A lock can't fix that. Under `SyncAlways`, each write pays its own
  F_FULLFSYNC, and old and new were about the same (30–90 ms).
- **Tests:** `TestCompactSyncsSnapshotBeforePausingWriters` wraps the fs so
  each temp-file Sync records its unsynced byte count and whether `db.mu`
  was held. It covers three cases:
  - no tail: no sync under the lock;
  - a small tail: one locked sync of exactly the tail;
  - a tail over the slack: caught up unlocked, no locked sync.

  `TestPowerLossCompaction` still passes at every cut, and the whole suite
  passes with `-race`.
- btypedb README's compaction paragraph is updated to match.

## 2. btypedb `LogStats` (`2383665`) and release v0.9.0

- `LogStats() (LogStats, error)`: `Epoch`, `Size`, `BaseSize`, read under one
  `RLock`, so a compaction can't pair a new base with the old size.
  Test: `TestLogStats`.
- Tagged **`v0.9.0`** on `2383665` (minor: new API). Pushed; resolves on
  proxy.golang.org. bytdb's suite was run against the local tree first
  (`go.work` already uses `../btypedb`).
- bytdb bumped to v0.9.0 (`d9631d3`). pgwire's and bench's indirect pins
  were tidied too, because they `replace` bytdb with `../`, so a
  `GOWORK=off` build would otherwise fail. `Engine.Compact`'s doc now says
  why the pauses stay brief and what they were before.

## 3. N-024: LogBaseBytes and VACUUM VERBOSE (`b841789`)

- `Stats.LogBaseBytes` (`json:"log_base_bytes"`) and `/metrics`
  `bytdb_log_base_bytes`, read with `LogEpoch`/`LogBytes` from `LogStats`.
- `Vacuum.Verbose`: the legacy `VERBOSE` keyword, or `(VERBOSE [value])`
  where `false|off|0|no` means off. `execVacuum` reads `LogState` before and
  after `Engine.Compact`, and appends `vacuumReport`'s line after any skip
  warnings: `compacted the storage log: B bytes before, A after (R
  reclaimed)`. "reclaimed" is dropped when concurrent writes left the log
  no smaller.
- Tests: `TestParseVacuum` (verbose column), `TestVacuumVerbose`,
  `TestEngineCompact` base assertions, `TestMetricsHandler`.
- Docs: README (monitoring paragraph), `docs/features.md`,
  `docs/gotchas.md`, the bytdb skill, and the `Result.Notice` doc.

## 4. N-025: notice severities (`5722154`)

- `noticeBody` classifies each line (the table is in its doc comment):
  - `already exists, skipping`: NOTICE 42P07, or 42701 for a column;
  - `does not exist, skipping`: NOTICE 00000;
  - the VERBOSE report: INFO 00000;
  - transaction notices: unchanged WARNING 25001/25P01;
  - everything else, including VACUUM's non-table skip: WARNING 01000, as
    in Postgres.
- `sendNotice` sends one NoticeResponse per `\n`-separated line.
- Test: `TestNoticeSeverities` (pgx, 13 statements).

## 5. Verification (`verify` recipe)

bytdbd on a scratch db with `-metrics-addr`, driven by a pgx v5.10.0 client:

| statement | notices |
|---|---|
| `create table if not exists t …` | `NOTICE 42P07 relation "t" already exists, skipping` |
| `alter table t add column if not exists v text` | `NOTICE 42701 column "v" of relation "t" already exists, skipping` |
| `drop table if exists ghost` | `NOTICE 00000 table "ghost" does not exist, skipping` |
| `vacuum verbose w, t` (w a view) | `WARNING 01000 skipping "w" --- …`, then `INFO 00000 compacted the storage log: 689991 bytes before, 837 after (689154 reclaimed)` |
| `vacuum (verbose) t` (again) | `INFO 00000 … 837 bytes before, 837 after` |

`/metrics` showed `bytdb_log_base_bytes 16` → `837` across the compaction,
with `bytdb_log_bytes` 689991 → 837.

## 6. Release v0.21.0

The user asked for the release after the work above landed. Minor bump: the
release adds behavior (a `Stats` field and metric, VERBOSE, notice
severities). Followed the `release` skill:

- `go vet` and `go test ./...` passed at the root and in `pgwire/`.
- Root tag **`v0.21.0`** on `26091ee`. Message: "btypedb v0.9.0
  (compaction's writer pause no longer grows with the database);
  Stats.LogBaseBytes and /metrics bytdb_log_base_bytes; VACUUM VERBOSE
  reports bytes reclaimed". Pushed with the four commits above; resolves
  on proxy.golang.org.
- `pgwire/go.mod` pin `v0.20.0` → `v0.21.0`, then `bench` `go mod tidy`
  raised its pin to `v0.21.0`. Both build with and without `GOWORK=off`,
  and pgwire tests pass. Committed as `7a52e9e` "pgwire, bench: bump bytdb
  to v0.21.0".
- pgwire tag **`pgwire/v0.21.0`** on `7a52e9e`. Message names the notice
  severity change. Pushed; resolves on the proxy.
- No next-list item was waiting on this release.

Consumers that want the shorter compaction pause (dbc, cats, gonotes, …)
get it by bumping to bytdb v0.21.0, or btypedb v0.9.0 directly.

## Files touched

- btypedb: `compact.go`, `compact_test.go`, `README.md`.
- bytdb: `go.mod`/`go.sum` (root, `pgwire/`, `bench/`), `engine.go`,
  `stats.go`, `stats_test.go`, `compact_test.go`, `sql/ast.go`,
  `sql/parser.go`, `sql/exec.go`, `sql/sql.go`, `sql/vacuum_test.go`,
  `pgwire/errors.go`, `pgwire/conn.go`, `pgwire/notice_test.go`,
  `README.md`, `docs/features.md`, `docs/gotchas.md`,
  `.claude/skills/bytdb-fast-memory-based-db/SKILL.md`,
  `ai_docs/todo/next-list.md`; `pgwire/go.mod` and `bench/go.mod` again for
  the v0.21.0 pin.

## Next

Closed: N-023, N-024, N-025. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
