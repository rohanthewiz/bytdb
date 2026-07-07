# Plan: bytdb reliability hardening

- **Created**: 2026-07-07
- **Origin session**: `4e8277bd-97b6-49db-a20f-964848615a1a` (after milestone 21, main at `c91d991`)
- **Goal**: bring bytdb to maximum reliability — fix confirmed defects, close robustness gaps, and maximize test coverage (fuzz + property + crash + concurrency).
- **How this was produced**: coverage profiling (root 82.0%, pgwire 80.7%, 175 tests, **zero fuzz targets**) plus four parallel code audits — durability/crash-safety, panics/error-paths, concurrency, and test-gaps. Every Tier-1 finding below was re-verified against the code; #1 was reproduced empirically.

## Baseline: what is already solid (don't re-litigate)

- **btypedb (v0.4.0) durability** is real and fault-tested on its own side: CRC-32 framed WAL records (`wal.go:59-114`), torn-tail truncation + seek on open (`db.go:199-241`), all-or-nothing `opBatch` group replay (`tx.go:124-131`, `wal.go:143-171`), `SyncAlways` fsync-before-ack by default with group commit (`db.go:41-49`, `tx.go:146`), directory fsync on create/rename, crash-safe compaction (temp+Sync+atomic Rename), and its own `TestCrashRecovery`/`TestPowerLoss*` fault-FS tests.
- **bytdb multi-key writes are atomic across restart**: `insertRow`/`updateRow`/`deleteRow` stage row + all index entries in one `kv.Update` → one WAL batch (verified, no gap). Drop paths use `DeleteRange` in one txn.
- **Decode of garbage is an error, not a panic**: `decodeRow` validates arity/parity/tag-type; `tuple.Decode`/`DecodeOne` bounds-check every read. NaN cannot corrupt the store — `tuple.normalizeFloat` returns an error and all callers check it.
- **Disconnect mid-transaction does NOT leak the writer lock**: `defer c.sess.Close()` → `tx.Rollback()` runs on any return from `conn.run` (verified LIFO defer order).
- **UPDATE/DELETE collect-before-mutate** (no mutation during a live scan); **concurrent inserts during CreateIndex** are correct and tested (`index_test.go:376`).
- **Test suite is genuinely thorough** across engine, planner, expr, joins, order, coercion, aggregates, three-valued logic, sessions, syscat, and real pgx-over-TCP pgwire flows. Gaps below are targeted, not broad.

---

## Tier 1 — Confirmed defects (fix first, each with a regression test)

### T1.1 — [CRITICAL] Parser stack overflow = remote server kill
- **Where**: `sql/parser.go` — recursive descent `exprOr→…→exprPrimary` (paren case ~1619-1636), `expression()` at 1340; **no depth guard anywhere** (grepped `depth`/`nesting`/`MaxDepth`).
- **Reproduced**: `SELECT (((…1…)))` with ~2,000,000 parens (a ~4 MB query, under pgwire's 64 MiB cap) → `fatal error: stack overflow`. This is a **fatal, unrecoverable** runtime error — `recover()` cannot catch it, so T1.2 does not save the server here. (200k parens parse fine; Go grows the stack to 1 GB first.)
- **Fix**: thread a depth counter through the parser (increment on entry to `expression`/`exprPrimary`, decrement on return); return a normal parse error past a bound (Postgres `max_stack_depth` ≈ 100; pick ~1000 to be safe). Reject before recursing.
- **Test**: a 100k-paren query returns an error; server survives.

### T1.2 — [CRITICAL] No `recover()` in per-connection goroutine
- **Where**: `pgwire/pgwire.go:100-118`, loop `pgwire/conn.go:61-104`. Only defers are `nc.Close()`/`delete(conns)`/`sess.Close()`. Zero `recover()` in non-test code.
- **Impact**: any panic reachable from parse/plan/exec on one connection terminates the **entire process**, killing all other clients. Amplifier for every other latent panic.
- **Fix**: wrap the body of `conn.run()` (or each dispatch in the message `switch`) in `defer func(){ if r := recover(); r != nil { send ErrorResponse XX000; log; teardown just this conn } }()`.
- **Test**: a statement that forces a panic on one connection leaves the server accepting new connections.

### T1.3 — [HIGH] DDL publishes in-memory catalog before commit is durable
- **Where**: `index.go:127-129` (CreateIndex), `index.go:165-167` (DropIndex), `ddl.go:123-125` (DropTable), `ddl.go:306-317` (`writeDescIn` → AddColumn/DropColumn/AddCheck/DropCheck). All do `e.tables[table]=desc` **inside** the `kv.Update` closure, which runs before `Commit`.
- **Impact**: if the commit's log append (`tx.go:132`) or fsync (`tx.go:146`) fails, `Update` returns an error but `e.tables` already advertises the new schema → phantom index maintenance, wrong `ScanIndex` results, in-memory/on-disk divergence until restart.
- **Important subtlety** (why the naive fix is wrong): the in-closure publish is **deliberate and correct for the concurrent-insert race** — `writerMu` is held through commit, so a one-shot `Insert` can't interleave and resolve a stale descriptor. `CreateTable` publishes *after* `Update` (safe only because a new table has no concurrent inserts). So **do NOT** simply move the publish after commit.
- **Fix**: capture `old` and restore it under `e.mu` on any `kv.Update` error (rollback the in-memory publish). Or reload the descriptor from kv on commit error. Longer-term: a btypedb commit hook that swaps the catalog atomically with the state swap, or move the catalog into the kv keyspace (single source of truth — would also fix T2.2).
- **Test**: inject a commit/writeLog failure during `CreateIndex`; assert `e.Table(t).Index(name) == nil` afterward and a fresh `ScanIndex` errors "no such index" rather than returning empty.

### T1.4 — [MEDIUM] Silent int64 arithmetic overflow
- **Where**: `sql/expr.go:548-566`, integer branch of `arith` — raw `li+ri`, `li-ri`, `li*ri`, no overflow check (only `/` and `%` guard, against zero only).
- **Impact**: `SELECT 9223372036854775807 * 2` → `-2`; `… + 1` → `MinInt64`. Postgres raises "bigint out of range"; bytdb silently corrupts the result.
- **Fix**: `math/bits.Add64`/`Mul64` (or checked helpers) → `serr.New("bigint out of range")` on overflow.
- **Test**: the two queries above should error, not wrap.

### T1.5 — [MEDIUM] UPDATE evaluates CHECK against the raw uncoerced SET value
- **Where**: `sql/exec.go:735-744` — check row built with `nv[setOrds[col]] = v` (raw `s.Set` value) while `tx.Update` coerces. `UPDATE … SET price='5'` (quoted) into an int column with `CHECK (price > 0)` evaluates the check against the string `"5"`.
- **Fix**: coerce the SET values to their column types before building the check row (mirror what `tx.Update`/`updateRow` does).
- **Test**: coerced-literal SET still satisfies/violates the check correctly; `SET col = NULL` makes a `CHECK (col > 0)` pass (NULL is not "definitely false").

---

## Tier 2 — Robustness gaps

### T2.1 — [MED-HIGH] Idle-in-transaction holds the global writer lock with no timeout
- `session.go:143-153` (BEGIN → `e.Begin(!ReadOnly)`), `pgwire/conn.go:61-104`. A client that sends `BEGIN` (read-write) then stops sending keeps `writerMu` forever; every other write/DDL blocks indefinitely. No `idle_in_transaction_session_timeout`, no socket read deadline.
- **Fix**: idle-in-transaction deadline (set a read deadline while `Status()==TxActive`, roll back + error on expiry) and/or a lock-acquisition timeout. At minimum document the DoS surface.
- **Test**: conn A `BEGIN; INSERT`; conn B `INSERT` must not block past the configured timeout.

### T2.2 — [MED] Read Txn can see a catalog newer than its data snapshot (non-atomic dual snapshot)
- `txn.go:35-39` (`ReadTxn`), `txn.go:48-54` (`Begin(false)`): the kv snapshot is taken first, then `catalogSnapshot()` reads `e.tables` — two non-atomic acquisitions, read path holds no `writerMu`. A concurrent `CreateIndex` between them hands the reader a descriptor whose index exists over a data snapshot that predates the backfill → `ScanIndex`/`ScanIndexRev` scans an empty range → **silently returns missing rows**. (Writers are safe: `Begin(true)` holds `writerMu` across both snapshots.)
- **Fix**: make the two snapshots atomic for reads — hold `e.mu.RLock` spanning both `kv.Begin(false)` and the `e.tables` clone, and have DDL hold `e.mu` across commit; or move descriptors into the kv keyspace (also fixes T1.3).
- **Test**: open a read Txn, from another goroutine CreateIndex+backfill, then ScanIndex on the read txn — assert it sees either the pre-index descriptor consistently or the full index, never an index with zero entries.

### T2.3 — [LOW-MED] Engine self-deadlock footgun
- `txn.go:22-30`: calling `e.Insert`/`Update`/`Delete`/DDL inside a `WriteTxn`/`Begin(true)` re-locks the non-reentrant `writerMu` on the same goroutine → permanent global deadlock. Guarded at the SQL `Session` layer (`session.go:119-128`, tested) but not at the engine API. Documented, not enforced.
- **Fix**: detect reentrancy and return an error instead of deadlocking, or document more loudly.

### T2.4 — Smaller items
- **`Offset+Limit` overflow** truncates results (`exec.go:373` — `s.Offset+s.Limit` overflows negative → collection stops after row 1). Saturating/overflow-safe compare. [LOW]
- **NaN ordering** differs from Postgres (`exec.go` `compareVals`/`orderCmp` via `cmp.Compare` sorts NaN smallest; PG sorts it largest). Not a panic, not a corruption (store rejects NaN keys). [LOW]
- **No SIGINT/SIGTERM handler** in `pgwire/cmd/bytdbd/main.go` — final `Close` fsync skipped on signal. Safe only under default `SyncAlways`; unsafe if anyone opens with `SyncEverySecond`/`SyncNever`. Add `signal.Notify` → `e.Close()`. [LOW]
- **`binary.BigEndian.Uint64(raw)` on the seq key** without a length check (`ddl.go:85`) — panics if the value is ever < 8 bytes. Add `len(raw)==8` guard. [LOW]
- **Descriptor has no `Version` field** (`engine.go:74-82`): a single undecodable descriptor bricks `Open` for all tables (`engine.go:187-201`). Add versioning; consider collecting all bad descriptors + a repair/read-only open mode. [LOW-MED]
- **Commit can return an error while the write is already visible** to concurrent readers (btypedb swaps state before fsync). Document on `WriteTxn`/`Insert` so callers don't treat an error as "definitely not applied." [LOW]

---

## Tier 3 — Test coverage (maximize reliability via automated checks)

Zero fuzz targets today; only one concurrency test in the whole tree (`index_test.go:385`); no evidence the suite runs under `-race` regularly; all reopen tests `Close()` cleanly first (no crash-recovery coverage at the bytdb level).

### Fuzz targets (add — cheapest high-yield)
1. **`FuzzTupleRoundTrip` + `FuzzTupleOrder`** (`tuple/tuple_test.go`) — the codec is the correctness foundation. Round-trip: Decode arbitrary bytes never panics; encode∘decode∘encode is byte-stable. Order: `sign(bytes.Compare(enc(a),enc(b))) == sign(semanticCompare(a,b))`, incl. NULLs-last and desc. Seed: existing `TestKnownOrder`/`TestRoundTrip` fixtures + boundary numerics (0, -0.0, NaN, ±Inf, MinInt64, MaxInt64), empty string/bytes, strings containing tuple type/terminator bytes, mixed-type tuples.
2. **`FuzzParse`** (`sql/parser_test.go`) — `Parse` must never panic, always error-or-AST. **Would have caught T1.1.** Seed: every query-string literal already in `sql/*_test.go` + malformed fragments (unterminated strings, dangling operators, deep parens).
3. **`FuzzMessageParse`** (`pgwire/`) — fuzz frontend `{type,length,payload}` frames; short/hostile client yields a protocol error, never panic/OOB.

### Property tests (add)
4. **Planner equivalence**: index scan ≡ seq-scan + filter + sort, over random rows + random WHERE (eq/range/composite-prefix/ASC/DESC). Highest-leverage single test — turns the whole planner into a checked equivalence; catches bound-swap/prefix/tie-break regressions. (`sql/plan_property_test.go`)
5. **`ScanRangeRev(x) ≡ reverse(ScanRange(x))`** over random ranges — generalizes the fixed `reverse_test.go` cases (relevant after milestone 21).

### Crash / fault tests (none exist at bytdb level)
6. Open with `SyncNever`, write, reopen **without** Close (simulate kill) → committed rows survive / no half-write corruption.
7. Truncate / append-garbage to the db file → `Open` drops torn tail, discards mid-batch, gives a clear error.
8. Multi-index insert → kill → reopen → row and all index entries all-present-or-all-absent.
9. DDL commit-failure (fault FS) leaves `Table()` unchanged (locks in T1.3).

### Concurrency tests (run under `-race`)
10. Read-Txn + CreateIndex (T2.2 missing-rows window).
11. Idle-`BEGIN` on conn A must not block conn B past a timeout (T2.1); disconnect-mid-tx releases the writer lock (regression-guard the safe path).
12. Server survives a panicking statement on one connection (T1.2).
13. Concurrent DDL + DML at the SQL/pgwire layer (only engine-level insert-vs-CreateIndex is covered today).

### Semantic gaps (fixed-example tests)
14. **UPDATE that scans the same secondary index it mutates** (`UPDATE t SET age=age+1 WHERE age>30`, index on age, multiple matches) — highest-value untested hazard; catches rows visited twice / skipped / infinite re-scan.
15. **`NOT IN` / `IN` with a NULL element/subquery row** — `x NOT IN (1,NULL)` must return zero rows (three-valued branch `expr.go:289-291` untested).
16. **Multi-row DELETE/UPDATE via a secondary-index range** — confirms bulk DML plans an index scan and cleans index entries (all DML WHERE tests today are full-scan or PK point-get).
17. **LIMIT/OFFSET corners**: `LIMIT 0`, `OFFSET` past end, and `LIMIT $1 OFFSET $2` (params — `Limit`/`Offset` are plain int64 in the AST, likely unsupported; pin the behavior).
18. **Statement-level atomicity**: a multi-row UPDATE/DELETE that violates a unique index/CHECK on row 3 of 5 rolls back rows 1-2 entirely.
19. Cross-type comparison/coercion matrix at eval time (`int_col = 2.0`, int-vs-float columns, `text_col < int_literal` should error).
20. RIGHT/FULL JOIN rejection is a clear error; chained LEFT JOINs with NULL propagation; scalar subquery returning 2+ rows from a WHERE/UPDATE; correlated subquery driving DELETE.
21. pgwire: empty query string; explicit Describe-of-statement param OID inference; rebinding a cached prepared statement with new params/NULL; PortalSuspended / row-limited Execute.

### ALTER × index interactions
22. `DROP COLUMN` on a column in a secondary index (block or drop the index); `ADD COLUMN` on a table that already has secondary indexes (entries stay valid).

---

## Suggested execution order

1. **T1.1 + T1.2** — smallest diffs, highest severity (a ~4 MB query kills the server). Add depth-limit + recover, with tests 2 & 12.
2. **T1.3, T1.4, T1.5** — correctness bugs, each with its regression test (9, plus overflow + CHECK-coercion tests).
3. **Fuzz targets (1-3)** — would have caught T1.1 automatically; cheap and permanent.
4. **Property + crash + concurrency suites (4-13)** — the structural safety net.
5. **T2 robustness (idle timeout, atomic snapshot, versioning)** and **remaining semantic tests (14-22)** as follow-on milestones.

Run everything under `go test -race ./...` in both modules and wire `-race` into the routine test command.
