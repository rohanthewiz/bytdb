# Deep-look remediation plan: reliability, performance, convenience

**Status:** not started
**Date:** 2026-11-09
**Scope:** root engine, `sql/`, `pgwire/`, `stdlib/`, `replicate/`, `btypedb` (two items), docs, CI
**Baseline:** bytdb `6ccf3cc`, btypedb `2383665`; `go vet` and `go test -race ./...` green on both modules
**Source:** four parallel code audits (engine, SQL executor, SQL front end, wire/driver/replication) plus system-level measurement, each finding backed by a runnable repro

## Why this plan exists

The storage and transaction core held up: a model-based index-drift test, an
OCC stress test against DDL churn, 6,000 random planner queries against an
index-free twin and 20,000 random joins against a brute-force oracle all came
back clean. What the audits found is concentrated in three places:

1. **The SQL layer returns wrong results silently** in several common query
   shapes. Two root causes account for most of it: values lose their SQL type
   at runtime (a timestamp is just an `int64`, a uuid is just `[]byte`), and
   subqueries run through a separate, reduced execution path.
2. **A handful of outage and data-loss paths at the edges**: one replication
   sequence that makes a generation unrestorable, a process-killing stack
   overflow from SQL text, writer-lock waits that nothing can interrupt, two
   permanent write hangs.
3. **Real clients do not work cleanly.** pgx cannot ping the server, cannot
   bind ints into `IN ($1,$2)`, and cannot scan `coalesce(sum(x),0)`.

None of this is caught today because there is **no CI** in either repository
and no differential test against real Postgres. Phase 0 fixes that first, so
every later phase lands against a safety net.

## How to read this plan

- Items are numbered `D-01` … `D-62` (D for deep-look) so they cannot collide
  with the `N-xxx` IDs in `next-list.md`. **The next list has not been
  edited.** When work starts, file one `N-` item per phase (Next ID is
  `N-035`) and close it when the phase's items are done.
- **Verified** says who reproduced it:
  `both` = the reviewing agent and an independent re-run in the main session;
  `agent` = the agent's repro only;
  `read` = found by reading, no repro.
- **Effort**: S = under half a day, M = one to two days, L = most of a week.
- "Postgres gives" is stated from knowledge. Nobody ran Postgres during the
  audit; D-02 is what makes those claims checkable.
- Every item carries its minimal repro inline, because the audit's scratch
  modules live in a session-scoped temp directory and will not survive.

## Decisions needed before starting

These change what gets built. Each has a recommendation; the plan below
assumes the recommendation.

| # | Question | Recommendation |
|---|---|---|
| Q1 | `timestamp + 1` adds one microsecond today. Reject it (Postgres errors) or define it? | Reject with Postgres's `operator does not exist`. `date + int` stays and is typed date. |
| Q2 | Multi-statement simple `Query` and `pgx.Batch` autocommit per statement. Make them one implicit transaction, as Postgres does? | Yes. DDL cannot join a block here, so a batch containing DDL keeps per-statement commits and the docs say so. |
| Q3 | `SELECT ... FOR UPDATE` under `WithConcurrentWrites`: parse-and-ignore is correct in single-writer mode, but under OCC it promises a lock that is not taken. | Single-writer: no-op. OCC: mark the selected rows as serializable reads for that transaction, so a concurrent writer of those rows makes one side fail with 40001. |
| Q4 | Interim guard for subquery `ORDER BY`/`LIMIT` (D-09): ship a hard reject immediately, ahead of the real fix? | Yes, in Phase 1. A loud error is better than a wrong row while Phase 3 is in progress. |
| Q5 | Three items re-argue declined Non-goals or documented trade-offs: a top-N heap for unindexed `ORDER BY ... LIMIT` (gotchas documents the full sort), `DEFAULT gen_random_uuid()` (gotchas lists it as omitted), and tables without a primary key. | Take the first two (each is small and the first also removes a 54000 refusal). Leave the third declined. |
| Q6 | Lower the default `MaxResultRows` (10M) for `bytdbd`, since results are fully materialized per client? | Lower `bytdbd`'s default to 1M; leave the embedded default alone. |

---

## Phase 0 — Safety net

Nothing else should land before D-01 and D-03.

### D-01 · Port the audit repros into the tree as failing tests · S
Every reliability item below has an inline repro. Before fixing anything,
write each as a test in the owning package, marked
`t.Skip("D-xx: see ai_docs/todo/2026-1109-deep-look-remediation-plan.md")`.
Each fix then deletes one `Skip` line, which makes the diff self-evidently a
regression test. Tests that hang by design (D-12, D-13, D-21) need a
goroutine plus `select` on a timeout so a failure is a test failure, never a
stuck run.

### D-02 · Postgres differential harness · M
`sql/difftest/` (own Go module, so the pgx dependency stays out of the root
module, the way `bench/` does it). A corpus of `.sql` files: setup
statements, then queries. The runner executes each query against bytdb
in-process and against a real Postgres reached through `BYTDB_DIFF_PG_URL`,
and compares rows after normalizing types and unordered results. Skipped when
the variable is unset, so `go test ./...` stays hermetic.
- Seed the corpus from this plan: every "Postgres gives" claim becomes a line.
- A per-query `-- bytdb: <reason>` annotation records deliberate divergences
  (the ones in `docs/gotchas.md`), so the file is also the living list of
  them.
- This converts the hand-pinned `sql/semantic_gaps_test.go` approach into
  one that finds divergences nobody thought to pin.

### D-03 · CI · S
`.github/workflows/ci.yml` in bytdb and in btypedb.
- Matrix: `ubuntu-latest`, `macos-latest`; Go from `go.mod`.
- Jobs: `go vet ./...`; `go test -race -count=1 ./...` for root and
  `pgwire/`; `GOWORK=off` build and test of `pgwire/` (guards N-031's
  no-`replace` rule); `staticcheck`; `gofmt -l`.
- A `diff` job with a `postgres:16` service container running D-02.
- A nightly job running each fuzz target for 60 s (`-fuzz=... -fuzztime=60s`)
  and uploading any new corpus entry as an artifact.
- bytdb's workflow needs btypedb at the pinned version, so it runs with
  `GOWORK=off`. A second, allowed-to-fail job checks out btypedb `main`
  beside it to catch cross-repo breakage early.

### D-04 · Benchmark regression gate · S
Move the benchmarks written during the audit into `_test.go` files in their
packages (point select prepared/unprepared, 100k `count+sum`, `IN`-list on
PK, uuid PK lookup, unindexed `ORDER BY ... LIMIT`, hash join, wire 100k-row
scan in text and binary, `Open` of a 500k-row file). CI runs them at
`-benchtime=1x` as a smoke test only; the gate is a documented
`benchstat` before/after in each Phase 7 PR, because shared CI runners are
too noisy to gate on.

---

## Phase 1 — Small, isolated fixes (all S)

Independent of each other and of the larger phases. Each is one PR with its
D-01 test un-skipped. Order within the phase is by severity.

### D-05 · Replication: ambiguous PUT + log growth makes a generation unrestorable
**Verified:** both · `replicate/replicator.go:243-266`, `replicate/restore.go:191-200`
- **Mechanism:** the chunk key embeds `end = watermark + n`. If a PUT is
  stored but the caller sees an error, and the log grows before the retry,
  the retry uploads `[0,S2)` beside the stored `[0,S1)`. `contiguousChain`
  sorts by start and breaks on the second chunk with the same start, so the
  chain ends below `manifest.Size`. `Restore` then skips the generation and
  silently returns an older one (or `ErrIncompleteReplica` if none), while
  `Status().LastError` is nil and later ships succeed.
- **Repro:** a `Storage` wrapper whose first `Put` stores the object and
  returns an error; ship; insert more rows; ship; `Restore`. Observed: rows
  2–4 shipped, restore returned row 1 only.
- **Fix:** in `contiguousChain`, among chunks with `start <= next` take the
  one with the greatest `end`, and trim the overlap when assembling in
  restore. Two overlapping chunks of the same epoch are byte-identical over
  the overlap by construction (the log is append-only within an epoch), so
  trimming is safe; assert that with a length check, not a content compare.
- **Also:** correct `docs/replication.md`, which says a re-ship is an
  identical range.

### D-06 · Unbounded expression chain kills the process
**Verified:** both · `sql/parser.go:3220-3261`, `3285-3311`, `2964`; recursion at `sql/expr.go:105`
- **Mechanism:** `exprAdd`/`exprMul`, the `::` postfix loop and the
  comparison loop parse iteratively, so `maxParseDepth` never trips, yet they
  build a left-nested AST as deep as the chain. `evalEx`, `bindExpr`,
  `walkExpr` and `exprType` recurse once per term. Go's stack-overflow is a
  fatal error that `recover` cannot catch.
- **Repro:** `"select 1" + strings.Repeat("+1", 1_000_000)` →
  `fatal error: stack overflow` (600,000 terms succeeds). pgwire's frame cap
  is 64 MiB, so a client can send it.
- **Fix:** count nodes produced by those loops against a per-statement
  budget (suggest 10,000 chained operators; error 54001 `statement too
  complex`, which is what Postgres uses for stack depth). Add the 1M-term
  case and a `::int` chain to `FuzzParse`'s seed corpus.

### D-07 · Error before execution does not abort an open transaction block
**Verified:** both · `sql/session.go:130-136`, `pgwire/conn.go:523-527`, `582-590`
- **Repro:** session: `BEGIN; INSERT INTO b VALUES (100); insrt typo; COMMIT`
  → commit succeeds, row persisted. Postgres: 25P02, then `ROLLBACK`.
- **Fix:** move the parse inside `runCtx` so a parse error takes the same
  failed-block path as a runtime error; add `Session.Fail()` and call it from
  pgwire on Parse/Describe/Bind errors while a block is open. Also return
  25P02 for `BEGIN` inside a failed block (currently a notice).

### D-08 · pgx liveness ping (`-- ping`) is a syntax error
**Verified:** both · `pgwire/split.go:39-45`, `pgwire/conn.go:508-527`, `581`
- **Repro:** `db.Ping()` through pgx's `database/sql` driver → `driver: bad
  connection`; five queries one second apart use five server connections.
  Also `select 1; -- trailing` errors after running the first statement.
- **Fix:** a part that is only comments and whitespace is empty: answer
  `EmptyQueryResponse` in simple query, and in `parse`. Put the
  comment-stripping in the lexer-aware splitter, not a regexp (nested block
  comments, N-034).

### D-09 · Interim reject for subquery `ORDER BY` / `LIMIT` / `OFFSET` (see Q4)
**Verified:** both · `sql/expr.go:1323-1470`, `471-515`, `1494-1545`, `1567-1590`
- **Repro** (amounts 10, 50, 30, 5, 99):
  `SELECT (SELECT amt FROM o ORDER BY amt DESC LIMIT 1)` → 10, Postgres 99;
  `id IN (SELECT id FROM o ORDER BY amt DESC LIMIT 2)` → all five ids;
  `EXISTS (SELECT 1 FROM o LIMIT 0)` → true.
- **Fix now:** at prepare, reject a subquery carrying `LIMIT`/`OFFSET` (and
  `ORDER BY` in `ARRAY(...)`), with 0A000 and a message naming the rewrite
  (a CTE or derived table, which already execute correctly). Phase 3 (D-27)
  removes the reject.

### D-10 · Upsert that changes a referenced key skips the parent-side FK check
**Verified:** both · `sql/exec.go:1001-1012`
- **Repro:** `p(id)`, `ch(pid REFERENCES p)`, one row each;
  `INSERT INTO p VALUES (1,'x') ON CONFLICT (id) DO UPDATE SET id = 100`
  succeeds and leaves `ch.pid = 1` dangling. Postgres: 23503.
- **Fix:** in the conflict-hit branch, compare `existing.Vals` with
  `stored.Vals` over `fkRefColsChanged` and queue `fkVerifyReferenced` for
  end of statement, exactly as `execUpdate` does at `1232-1252`. Factor the
  shared step into one helper so the two paths cannot drift again.

### D-11 · SERIALIZABLE misses write skew through a failed uniqueness probe
**Verified:** both · `dml.go:121-126,146`, `258,282`; contract at `txn.go:119-124`
- **Mechanism:** the probes are deliberately unmarked because "on the only
  path that commits, the probed key is itself written". But a failed insert
  leaves the transaction committable, so "this key exists" is a read that
  never enters the read set.
- **Repro:** two `BeginSerializable` transactions each get a duplicate-key
  error on the other's row, then each deletes the other's row; both commit.
- **Fix:** call `tx.MarkRead(key)` on the error branches only, immediately
  before returning the duplicate / unique error. The success path stays free,
  so bulk loads do not grow a read set.

### D-12 · DDL callbacks can deadlock the engine
**Verified:** both (default mode); agent (OCC) · `engine.go:408-449`, `458-466`
- **Mechanism:** `updateDDL` never sets `writerGID`, so the reentrancy guard
  does not fire for a write issued from `AddCheck`'s `validate`,
  `AlterColumnType`'s `Using`/`Validate`, or `AlterSequence`'s `mutate`.
  Under OCC the nested watermark commit forces the `UpdateExclusive`
  escalation, which then blocks on its own nested commit.
- **Repro:** `AddCheck` whose validate calls `e.NextSeq` hangs forever.
- **Fix:** set and clear `writerGID` inside `updateDDL`'s closure; under OCC
  mark the goroutine for the duration of `UpdateExclusive` so `seqPut` and
  the one-shots return the existing "would deadlock" error.

### D-13 · `CREATE SEQUENCE ... CACHE <huge>` spins
**Verified:** both · `seqalloc.go:454-468`, `seqobj.go:91-93`
- **Repro:** OCC mode, `CACHE 1125899906842624` (2^50): first `nextval` never
  returns, holding that sequence's allocator mutex at 100% CPU.
- **Fix:** compute the batch end arithmetically (clamp to the remaining steps
  before the bound, or to one cycle) instead of stepping; additionally cap
  `CACHE` at a sane maximum (suggest 1,000,000) at `CREATE`/`ALTER`.

### D-14 · `Serve` returns on a transient `Accept` error, stopping `bytdbd`
**Verified:** both · `pgwire/pgwire.go:336-345`
- **Fix:** retry temporary errors with capped exponential backoff (5 ms →
  1 s), as `net/http` does. Related: over-limit connections are accepted in
  order to send 53300, so `MaxConns` does not cap file descriptors; close
  them before the TLS handshake once a hard ceiling (suggest 2× `MaxConns`)
  is reached.

### D-15 · Write deadline armed before execution, never extended
**Verified:** agent · `pgwire/conn.go:229`, `305-310`, `890-914`
- **Repro (a):** `WriteTimeout=300ms`, statement waits 900 ms for the writer
  lock: the write commits, the reply hits an expired deadline, client sees a
  dropped connection for a committed write.
  **(b):** a client reading steadily at 3 MB/s is cut off once the total
  transfer exceeds `WriteTimeout`.
- **Fix:** arm at first write, not at dispatch; re-arm after each successful
  flush (a small writer wrapper). The deadline then bounds a stall, which is
  what it is for.

### D-16 · Prepared statement outlives a schema change
**Verified:** agent · `pgwire/conn.go:739-753`, `pgwire/values.go:96-104`
- **Repro:** prepare `SELECT n FROM t`; another connection runs `ALTER
  COLUMN n TYPE text` → client decodes text bytes as int8. After `ADD COLUMN
  ... DEFAULT 5` with `SELECT *` → index-out-of-range panic, connection
  closed.
- **Fix:** in `execute`, compare the fresh `res.Cols`/`res.Types` with
  `prep.info`; on mismatch send 0A000 `cached plan must not change result
  type`, which pgx handles by re-preparing.

### D-17 · Wire guard: value kind must match the declared column type
**Verified:** agent · `pgwire/values.go:157-190`
- Defence in depth for Phase 2: in `encodeValue`, when the Go value's kind
  disagrees with the declared OID, send XX000 naming the column rather than
  the wrong bytes. Phase 2 fixes the three known sources; this keeps any
  future one from becoming silent corruption on the wire.

### D-18 · `nextval`/`setval` in a row expression panics
**Verified:** agent (two reviewers independently) · `sql/alter.go:116-127`, `sql/check.go:170`, `sql/expr.go:1128,1151`
- **Repro:** `ALTER TABLE t ALTER COLUMN i TYPE int USING nextval('sq')`;
  `ALTER TABLE t ADD CONSTRAINT ck CHECK (nextval('sq') > 0)`.
- **Fix:** reject sequence functions in `validateRowExpr` (Postgres also
  refuses them in CHECK); nil-check `env.tx` in `evalFunc` as a backstop.

### D-19 · Engine API misuse that fails silently
**Verified:** both, except where noted
- **Closed `Txn`** (`txn.go:560-578`, `dml.go:217-219`, `365-367`): `Get`,
  `Scan`, `Update`, `Delete` on a finished transaction report "not found"
  with a nil error. Add a `done` flag set in `Commit`/`Rollback` and after
  the closure helpers return; every method returns `ErrTxClosed`.
- **Shared descriptor** (`ddl.go:911-919`, `txn.go:391-394`, `dml.go:16-19`):
  `Engine.Table`, `Txn.Table` and `Row.Desc` hand out the cached pointer; a
  caller's mutation is persisted by the next DDL. Return a deep copy from the
  exported accessors (internal callers keep the shared pointer).
- **Identity column updated upward** (`dml.go:227-249`): `Update id 1 → 3`
  does not bump the counter, so later draws collide, contradicting the
  documented collision rule. Bump in `updateRow` when an identity column is
  in `set`.
- **Read-only `Insert` under OCC** (`dml.go:102-108`; agent): burns an
  identity value and writes a durable watermark before failing. Check
  writability before `fillIdentity`.
- **`AddCheck` name collision** (`ddl.go:709-714`; agent): accepts a name
  already used by a foreign key on the table. Check both lists, as
  `fk.go:38-47` does.
- **`DropCheck`/`DropForeignKey` under a DDL retry** (`ddl.go:738-748`,
  `fk.go:200-210`; read): the captured `existed` is not reset when
  `updateDDL` re-runs the closure. Reset it at the top.

### D-20 · OCC allocator leaks
**Verified:** agent · `seqalloc.go:413-422`, `244-245`, `276-284`
- `nextval` on a missing sequence leaves an allocator in the map per distinct
  name (+226 B per call, reachable from SQL). Delete the entry when the
  reservation fails.
- An identity insert that loses to `DROP TABLE` re-creates the dropped
  table's counter key (harmless orphan; table IDs are not reused). After
  `errSeqReset`, verify the descriptor still exists inside the reserve
  transaction.

### D-21 · Smaller SQL correctness items
**Verified:** agent unless marked
- `COALESCE` evaluates every argument (`sql/expr.go:187-194`; both):
  `coalesce(1, 1/0)` errors, `coalesce(1, nextval('s'))` consumes a value.
  Short-circuit in `evalEx`, as `CASE` does.
- CHECK on an identity column never sees the drawn value
  (`sql/exec.go:1027-1031`): `id serial CHECK (id > 5)` stores 1. Check
  `stored.Vals` after the insert when the table has both.
- `count(DISTINCT x)` grows an unbudgeted set (`sql/agg.go:82-91`): charge
  `len(a.seen)` through the row budget in `accum.add`.
- `CREATE VIEW v AS SELECT $1` is accepted and later yields a raw param
  value: reject placeholders in a view body.
- `SET statement_timeout = 99999999999999999999` overflows the duration
  conversion (`sql/session.go:420`): range-check.
- A join probe whose outer value type changes mid-stream fails with "value
  does not fit column type" (`sql/join.go:479-508`): re-check the cached
  plan's bind type per row, as `corrsub.go`'s `bind` already does.
- A derived table with duplicate output names resolves `x.id` to the first
  silently: report ambiguity.
- `EXISTS (SELECT count(*) ... WHERE false)` is false; an ungrouped aggregate
  always yields one row, so it must be true.

### D-22 · Lexer and parser divergences
**Verified:** both for the first four, agent for the rest · `sql/lexer.go:174-222`, `sql/parser.go:3231`, `3268-3271`, `3902-3915`, `2815-2822`
- `SELECT 0x1F` → 0 and `SELECT 1_000` → 1 (the remainder becomes an alias);
  `1a` → 1. Postgres 16, which `server_version` advertises, gives 31 and
  1000 and rejects `1a`. Implement hex/octal/binary and underscore
  separators; reject trailing junk after a number.
- `E'\x41'` → `x41`, `E'\101'` → `101`, `E'é'` → `u00e9`: implement
  `\xHH`, octal, `\uXXXX`, `\UXXXXXXXX`. This one silently changes stored
  data.
- `-1::int`, `- - 1`, `1+-+1` are syntax errors: route the folded literal
  through `exprPostfix` and recurse on a repeated sign.
- `-9223372036854775808` becomes a float: negate before range-checking.
- `2 || 3 + 4` errors because `||` shares the additive level; give it its
  own level below additive (Postgres: "any other operator").
- `WHERE i = NULL` is a hard error; return no rows.
- `1 = ANY(NULL)` is false instead of NULL.
- `1 < 'a'`, `1 = true`, `1 < 2 < 3` return NULL silently; they should
  error. Folded into Phase 2 (D-23), which makes uncoercible comparisons
  errors generally.

---

## Phase 2 — One static typing pass (L)

This is the largest single piece of work and removes the most findings. Do it
as one design, landed in the steps below, each step shippable.

### The problem

A SQL value's type exists only at the catalog and in `exprType`. At runtime a
timestamp and a date are both `int64` and a uuid is `[]byte`. Correct
behaviour today depends on one pass, `coerceLiterals` (`sql/coerce.go`),
which only visits the top-level statement's `Pred` leaves, returns early when
FROM names a CTE or derived table, never enters `WITH` bodies, subqueries or
view bodies, and is not applied to CHECK expressions. Everything it misses
falls to `checkPred`'s fallback (`sql/exec.go:293-318`), which guesses the
type from the Go kind (`kindOf`), fails, and **swallows the error**, so the
comparison quietly yields unknown.

```
                today                                   after
  literal ─► coerceLiterals (top-level Pred only)   literal ─► typed bind pass
                │ miss                                         (every Expr, every
                ▼                                              nested SELECT)
          checkPred: kindOf(v) guess                              │
                │ fail                                            ▼
                ▼                                       typed AST: every node
          error swallowed → unknown → row dropped       carries its ColType;
                                                        mismatches error at bind
```

### What breaks because of it (all become D-01 tests and D-02 corpus lines)

**Verified:** both for the first six, agent for the rest.
- `WHERE due > now()` with `due = '2099-01-01'` → 0 rows.
- `ts::date` → raw microseconds; so `ts::date = '2024-01-01'` matches
  nothing and pgx decodes year -2923871.
- `CASE WHEN at > '2020-01-01' THEN 'recent' ELSE 'old' END` → `old`.
- `CHECK (day >= '2000-01-01')` accepts `'1999-01-01'`. **A declared
  constraint is not enforced.**
- `id IN (SELECT id FROM t WHERE d = '2024-02-29')` → 0 rows; same inside a
  CTE, a derived table and a view body.
- `u IN ('...')`, `u = ANY($1)`, `ts IN ('...')`, `d IN ($1)` with a
  `time.Time` → 0 rows.
- `coalesce(ts, now()) < '2025-01-01'`, `(SELECT max(ts) ...) > '...'` → 0
  rows; `SELECT ts > '2024-01-01'` → NULL.
- `d > ts` compares days against microseconds.
- `ts::text` → `"1709208000000000"`, `u::text` → raw bytes, `'x' || d` →
  `"x19782"`, `1000000.0::text` → `"1e+06"`.
- `1::bool` returns `int64(1)` typed bool (pgx: `invalid length for bool`);
  `true::int` is unsupported.
- **Data corruption:** `ALTER TABLE t ALTER COLUMN ts TYPE date USING
  ts::date` stores microseconds as a date.
- Result typing falls back to text: `coalesce(...)`, `nullif(...)`, scalar
  subquery, `exists(...)`, `CASE` whose first THEN is NULL. pgx fails with
  `cannot scan text (OID 25) into *int64`.
- Parameter typing falls back to text: `id IN ($1,$2)`, `id = ANY($1)`,
  `$1::int`, `i + $1`, `lower(s) = $1`, any `$n` in a subquery. pgx fails
  client-side with `unable to encode 7 into text format`.
- `int 1` and `float 1.0` are distinct under GROUP BY / DISTINCT / UNION
  (`SELECT 1 UNION SELECT 1.0` → 2 rows).
- The hash join puts int, date and timestamp in one "numeric" class
  (`sql/join.go:336-348`).

### D-23 · Typed bind pass · L
A single pass, run once per statement after parse and view expansion, inside
the statement's own transaction (which also fixes the stale-catalog-snapshot
issue in `coerce.go:31`).
1. **Annotate.** Walk every `Expr` in the statement, including `WITH`
   bodies, derived tables, subqueries, RETURNING, CHECK expressions
   (`resolveChecks`) and view bodies (`materializeView`), and record each
   node's static `ColType`. `exprType` is the starting point; extend it for
   `coalesce`/`nullif`/`CASE` (first non-NULL branch, then unify), `ExSub`
   (its item), `exists` (bool), `date ± int` (date), and the function table.
2. **Coerce untyped operands.** An untyped literal or `$n` takes the type of
   its sibling operand (comparison, `IN` list, `ANY`, `BETWEEN`, arithmetic)
   or of an enclosing cast. This is where string literals become
   timestamps/dates/uuids, and where parameter OIDs come from.
3. **Insert conversions.** When two typed operands differ but are
   comparable (date vs timestamp, int vs float), insert an explicit
   conversion node (`date → timestamp` multiplies days to micros). No
   runtime guessing.
4. **Reject the rest at bind.** `1 < 'a'`, `1 = true`, `ts + 1` (per Q1),
   text vs number: `operator does not exist` (42883), with a position.
5. **Delete the fallback.** `checkPred` no longer calls
   `coerceLit(s, kindOf(v))`; an uncoercible string at runtime is `invalid
   input syntax` (22P02), never unknown.
6. **Resolve columns to ordinals** in the same walk (`ExCol` → the existing
   `exOrd` node). This is free here and is performance item D-44.

Design choices to hold to:
- The annotation lives on the AST node, not in a side map keyed by pointer,
  so a prepared statement's cached AST stays self-describing. `bindParams`
  clones the tree per execution already; the type slot is copied with it.
- A prepared statement re-runs the pass when the catalog version changes
  (D-40 introduces the version counter; until then, re-run per execution,
  which is what `coerceLiterals` costs today).
- `coerceLiterals` is removed, not kept alongside. Two passes that both
  claim to type literals is how the gaps arose.

### D-24 · Casts driven by the operand's static type · M
`castVal` (`sql/expr.go:811-942`) receives the source `ColType` from D-23.
Add timestamp↔date, typed→text rendering through the existing `Format*`
functions, int↔bool, and Postgres float formatting (`1000000`, not `1e+06`).
`valText` (`786-803`) uses the same renderer so `||` on a date is correct.
`CAST(x AS t)` (D-52) parses to the same node.

### D-25 · Result and parameter descriptions from the typed AST · S after D-23
`sql/describe.go:253-306` reads the annotations instead of re-deriving.
Fixes the pgx scan failures and the `IN ($1,$2)` encode failures. As a
belt-and-braces measure, describe a genuinely uninferable parameter as OID 0
(unknown) rather than 25 (text), which lets pgx choose its own encoding.

### D-26 · Hash keys by SQL type · S after D-23
GROUP BY, DISTINCT, UNION and hash-join keys encode through the node's
unified type, so `1` and `1.0` collapse and date/timestamp never share a
bucket class. NaN as a grouping key gets a defined encoding instead of
`tuple: NaN is not encodable` (see D-36 for storage).

**Exit test for the phase:** every bullet in "What breaks" passes in D-02
against Postgres, `FuzzExec` runs clean for 10 minutes, and the pgx probes in
D-37 all pass.

---

## Phase 3 — Subqueries through the real SELECT path (M)

### D-27 · Run subqueries with `runSelectBody`
**Where:** `sql/expr.go:1323-1470` (`runScalarSub`), `471-515`
(`runSubColumn`), `1494-1545` (`runArraySub`), `1567-1590` (`runExistsSub`).
These four are a second, reduced executor. They ignore ORDER BY / LIMIT /
OFFSET, reject GROUP BY / HAVING / UNION, and mishandle an expression over an
aggregate.
- **Uncorrelated:** execute once through `runSelectBody` on a tx-bound DB
  copy and memoize in the existing `subMemo`. The CTE path already does
  exactly this; share the code.
- **Correlated:** keep the per-outer-row plan cache from `corrsub.go`, but
  run the body through the same executor with the outer row bound, so sort,
  limit, grouping and aggregates-in-expressions all come from one
  implementation.
- Removes the D-09 interim reject.
- Fixes, with tests: the latest-row-per-customer correlated form;
  `(SELECT coalesce(sum(amt),0) FROM o WHERE cust = 9)` → 0 not NULL (both
  verified); `(SELECT count(DISTINCT x) ...)`; `(SELECT count(*)+1 ...)`.
- Unlocks for free (Convenience): `IN (SELECT ... GROUP BY/HAVING)`,
  `IN (SELECT max(..))`, UNION inside a subquery, `EXISTS (... GROUP BY)`.

### D-28 · Hash set for `x IN (uncorrelated subquery)` · S
`evalAnyAll` (`sql/expr.go:342-381`) probes the memoized `[]any` linearly:
449 ms for 100k outer rows × 500 values against a 30 ms scan. Memoize a set
keyed like `hashJoinKey` plus a "contains NULL" flag for three-valued
results. Estimated gain about 10×. Lands naturally with D-27.

---

## Phase 4 — Statement atomicity and snapshot discipline (M)

The invariant to restore: **one statement reads one snapshot and observes
none of its own writes**, and NO ACTION constraints are checked at end of
statement, as `sql/fk.go`'s header and the docs already claim.

### D-29 · UPDATE and DELETE evaluate against the pre-statement state · S–M
**Verified:** agent · `sql/exec.go:1172-1247`, `1302-1318`
- **Repro:** `UPDATE t SET x = (SELECT max(t2.x) FROM t t2 WHERE t2.grp =
  t.grp) + 1` → 31, 32, 33; Postgres 31, 31, 31. `DELETE FROM t RETURNING
  (SELECT count(*) FROM t)` → 3; Postgres 4.
- **Fix:** two passes over the already-materialized `rows`: first evaluate
  every row's SET expressions and CHECKs, then write. For DELETE compute
  RETURNING rows before the delete loop. This is also the natural place to
  charge the row budget (closes N-029).

### D-30 · One snapshot per statement · S
**Verified:** agent · `sql/sql.go:1027`, `sql/exec.go:1383-1393`, `sql/views.go:96-125`
- **Repro:** `SELECT count(*) FROM t UNION ALL SELECT count(*) FROM t` under
  a concurrent inserter disagreed in 20 of 3,000 runs.
- **Fix:** add `len(s.Union) > 0` to the single-transaction wrap condition;
  for DML call `withViews` inside the write transaction on a tx-bound DB
  copy. D-23 already moves literal typing into the statement's transaction.

### D-31 · Child-side FK checks at end of statement · M
**Verified:** agent · `sql/exec.go:1044-1048`, `1226-1231`, `sql/fk.go:62-80`
- **Repro:** `INSERT INTO node VALUES (1, 2), (2, NULL)` fails (reversed
  order works); `UPDATE node SET id = id + 10, parent = parent + 10` fails;
  `UPDATE p SET id = CASE WHEN id = 1 THEN 2 ELSE 1 END` fails though key 1
  still exists afterwards. Postgres accepts all three.
- **Fix:** queue `fkVerifyRow` calls and run them after the write loop; in
  `fkVerifyReferenced`, first check whether the old key tuple still exists in
  the parent. Bound the queue with the row budget.
- **Performance rider (D-45):** skip the per-row check entirely when no
  `fk.Cols` ordinal is in `setOrds`.

### D-32 · Implicit transaction for multi-statement Query and pipelines · M (Q2)
**Verified:** both · `pgwire/conn.go:521-545`, `739`
- **Repro:** `update acct ...; insert <dup>` in one simple Query, and the
  same as a `pgx.Batch`: the update stays committed after the batch fails.
- **Fix:** when the session is idle, open an implicit block at the first
  statement of a multi-statement Query, or at the first Execute after a
  Sync, and commit at the end / at Sync; roll back on error. Explicit
  `BEGIN`/`COMMIT` inside the batch behave as in Postgres. DDL carve-out
  documented in `docs/gotchas.md`.

---

## Phase 5 — Interruptible writer-lock waits (M, cross-repo)

### D-33 · Context-aware writer lock
**Verified:** both · `sql/session.go:175-177`, `txn.go:230-243`; btypedb `tx.go` Begin
- **Repro:** hold a write block open; a second `BeginTx`/`ExecContext` with
  a 200 ms context is still blocked 3 s later. Over the wire,
  `statement_timeout=200` never fires, the client gives up, and the INSERT
  then commits once the lock frees.
- **Consequence:** request timeouts do not bound lock waits (up to the
  5-minute idle-in-transaction default), and abandoned writes land later.
  `docs/security.md` says `statement_timeout` "bounds every statement".
- **Fix, in order:**
  1. **btypedb:** `BeginCtx(ctx, writable)` and `UpdateCtx`. The writer
     mutex becomes a one-slot channel semaphore so acquisition can `select`
     on `ctx.Done()`. Release tag (v0.10.0).
  2. **bytdb engine:** `BeginCtx`, `WriteTxnCtx`, and ctx variants of the
     one-shot writes and DDL. The existing methods call them with
     `context.Background()`, so nothing breaks.
  3. **sql:** thread ctx into `txnControl` and `d.write`; re-check ctx after
     acquiring, before executing, so a statement cancelled while waiting does
     not run.
  4. **stdlib / pgwire:** pass the request ctx through; `statement_timeout`
     and `CancelRequest` now cover the wait.
- This also answers the engine-API gap "no context or timeout on anything
  that takes the writer lock". Consider `SET lock_timeout` once the plumbing
  exists (small).

---

## Phase 6 — Replication and server hardening

### D-34 · Replication: generation ordering, visibility, bounds
- **Order generations by a counter, not wall-clock names** (S; read ·
  `replicator.go:393-397`, `restore.go:94`): a backwards clock step or a
  replacement host makes the live generation sort older than a stale one.
  Store `seq = max seen + 1` in the manifest and sort on it; keep the
  timestamp in the name for humans.
- **Expose lag** (S; read · `replicator.go:89-95`, `226-241`): after every
  restart or compaction, new writes are unprotected until the whole log
  re-uploads, and `Status` cannot show it. Add `Sealed bool` and `LagBytes
  int64`; export both as metrics (D-58).
- **Bound a stalled transfer** (S; read · `s3/s3.go:41-55`,
  `replicator.go:155`): per-operation context timeout scaled to chunk size,
  so a black-holed PUT cannot hold `shipMu` until TCP gives up.
- **Chunk count growth** (M; read · `replicator.go:243-266`,
  `restore.go:220-240`): a steadily written, rarely compacted database
  accumulates one object per active tick (up to ~17k/day) and restore does
  sequential GETs. Merge small chunks on a threshold, or roll a generation
  from a `BackupTo` snapshot on a schedule; parallelize restore GETs (4–8
  workers, ordered assembly).
- **Temporary credentials** (S; read · `s3/s3.go:58-79`, `238-278`): add
  `SessionToken` and send `X-Amz-Security-Token`, so IAM roles and STS work.
  Fix `List` returning a partial listing when a page is truncated with an
  empty continuation token (`s3.go:205`): treat as an error.

### D-35 · Graceful shutdown · M
**Verified:** read · `pgwire/pgwire.go:386-398`, `pgwire/cmd/bytdbd/main.go:178-185`
`Close` closes sockets and returns without waiting; `bytdbd` then closes the
engine under in-flight statements. Add `Server.Shutdown(ctx)`: stop
accepting, send FATAL 57P01 to idle sessions, wait for active statements
until ctx expires, then close. `bytdbd` calls it on SIGTERM with a
configurable grace period (`-shutdown-grace`, default 10 s).

### D-36 · NaN and negative zero in float columns · S–M
**Verified:** agent · `tuple/tuple.go:365-372`, `dml.go:797`
NaN cannot be stored in any float column (`tuple: NaN is not encodable`),
and `-0.0` is stored as `+0.0` while `InsertReturning` reports `-0.0`.
Allow NaN in value position and give it a defined sort position in key
position (Postgres sorts NaN above all numbers; the existing tests already
pin "NaN sorts last"). Remove the dead NaN branch at `alter.go:497` or make
it live. This changes a persistent encoding only by adding a case, so no
format-version bump is needed; confirm with `FuzzTupleOrder`.

---

## Phase 7 — Performance

Each item's PR carries a `benchstat` before/after from the D-04 benchmarks.
Reviewer numbers are from an Apple M3; main-session numbers from an M1 Pro.

### D-37 · Pushdown for UUID, TIMESTAMP and DATE · S · est. 100–1000× on affected queries
**Verified:** both · `sql/plan.go:302-320` (`litFits`), `sql/join.go:307`
`litFits` returns false for every type but int/float/text/bool, so no
conjunct on these columns reaches a point get or bounded scan, and
`indexServes` then vetoes the hash join too. `EXPLAIN SELECT * FROM ev WHERE
id = '<uuid>'` shows `Seq Scan`; a 3000 × 3000 join on a uuid PK took 1.75 s
against 8 ms for the int-keyed twin; every FK check on a uuid parent is a
parent scan. Extend `litFits` (int64 for timestamp/date, 16-byte `[]byte`
for uuid). Do this **after D-23**, so the literal is typed by one mechanism;
verify `Txn.Get`/`ScanIndex` bound encoding and add all three types to
`sql/plan_property_test.go`'s generator.

### D-38 · `IN`-list, OR-of-equalities and `= ANY(array)` pushdown · M · est. ~1000× for key lookups
**Verified:** both · `sql/plan.go:106-125`, `sql/expr.go:305-337`
`id IN (1,2,3)` on a primary key is a `Seq Scan`; 10 literals cost 50 ms
against 5 µs for a point get. This is what ORM eager loading emits. When a
top-level conjunct is `col IN (constants)` or `= ANY(constant array)` on the
leading column of the chosen path, run N point gets or bounded scans over a
sorted, de-duplicated key list (sorted so output order still serves ORDER
BY). For the residual filter, pre-build a hash set for constant lists.
`EXPLAIN` shows it as `Index Scan ... (id = ANY ...)`.

### D-39 · Lazy column decode on scans · M · est. 3–5× on `count(*)` / `sum(col)`
**Verified:** agent, by profile · `dml.go:809-838` (`decodeRow`), `tuple/tuple.go:131-200`
The 100k-row `count(*), sum(amount)` benchmark is 32–37 ms, of which the
engine-only scan is 29 ms. `decodeRow` is 77% of statement time and 88% of
allocated objects: it runs two `tuple.Decode` calls and boxes the key prefix,
every column tag and every value into a fresh `[]any` per row, whether or
not the query references the column.
- The SQL layer already knows the referenced ordinals after D-23 (filter
  binds, projection, aggregate arguments, sort keys). Pass them to a new
  engine entry point `ScanRangeCols(..., ords []int, fn)` that skips
  unreferenced columns without allocating. `count(*)` with no WHERE needs
  zero columns.
- Inside the decoder: decode `(colID, value)` pairs with `DecodeOne`
  straight into the destination slice (no intermediate pair slice), and build
  strings in one allocation (`unescape` allocates, then `string(raw)` copies
  again).
- **Correct `docs/benchmarks.md`**, which attributes the gap to "row-at-a-time
  interpreted execution". The interpreter is a few percent; decoding is the
  cost.

### D-40 · Point reads: stop re-deriving per execution · S then M · 5 µs → about 2 µs
**Verified:** agent, by profile (two reviewers) · `sql/sql.go:1014/1027`, `sql/views.go:96,258`, `sql/coerce.go:31`
A prepared point SELECT is 3.9–4.9 µs and 117–128 allocations against a
1.0–1.3 µs engine `Get`. Share of the prepared path: `usesViews` ~10%,
`withViews` ~11% (each `Engine.View` opens its own KV snapshot), literal
coercion ~16% (another snapshot), `bindParams` clone ~7%, `prepareFrom` 9%,
projection 7%; the fetch itself is 24%.
1. **(S)** An engine-level "any views exist" counter and one merged view
   walk: about −20%.
2. **(S)** A catalog version counter on the engine, bumped by every DDL
   commit. This is the key that D-23 re-typing, the plan cache and D-47 all
   validate against.
3. **(M)** Cache the bound scope, plan and projection on `Stmt`, keyed by
   catalog version: about −15%, plus D-23's typing no longer re-runs.
4. **(S)** An LRU parse cache keyed by SQL text for `Exec(string)`, the
   stdlib driver's unprepared path and pgwire's simple protocol: about −29%
   on that path.

### D-41 · Wire row encoding · S–M
**Verified:** agent, by benchmark · `pgwire/conn.go:890-914`, `831-837`, `pgwire/values.go:114-191`
11 allocations per row in binary and 16 in text; a 100k × 5 scan is 31.6 ms
in the session and 56.8 / 82.3 ms over the wire. An `appendValue(dst, v,
fmt, type)` writing into one reused per-connection buffer with the length
back-patched measured 172 ns → 18 ns per row (binary) and 410 → 245 ns
(text). The remaining text allocations are `FormatTimestamp`; add an append
variant. Raise the 4 KiB write buffer to 64 KiB in the same PR (about 5% on
its own).

### D-42 · Unix-domain socket for `bytdbd` · S
**Verified:** agent, by benchmark
A point read over TCP loopback is 24–27 µs, of which 15–19 µs is the bare
loopback round trip; over a Unix socket the same exchange is 11–13 µs.
`Serve` already accepts any listener. Add `-socket <dir>` using the
`.s.PGSQL.<port>` name so `psql -h <dir>` and pgx `host=<dir>` work.

### D-43 · Top-N for unindexed `ORDER BY ... LIMIT` · S (Q5)
**Verified:** agent · `sql/exec.go:481-517`
Collects and stable-sorts every row: 60 ms against 27 ms for the same scan.
A bounded heap of `OFFSET + LIMIT` rows in the `execSelect` callback is
about 40 lines, roughly halves the time, and removes the 54000 row-cap
refusal that `docs/gotchas.md` currently documents for this shape. Keep
ordering stable for ties (heap entries carry an arrival sequence).

### D-44 · Per-row expression overheads · S each
**Verified:** agent, by benchmark
- Column references resolve by name per row (`sql/expr.go:87-102` →
  `join.go:67-93` → linear `ColIndex`): 7 ns for the first column, 88 ns for
  the 40th. Done by D-23 step 6.
- `||` and `-` chains call `staticJSONB` on both operands at every node,
  quadratic in chain length (`sql/expr.go:231-241`, `743-773`): 101 ms vs
  35 ms over 20k rows. Decide jsonb-ness once at bind (a flag on `ExArith`).
- One 96-byte `exEnv` allocation per row per `Cond` filter
  (`sql/join.go:149`): reuse a caller-owned env as `execSelect` does.
- `stepNext` copies every row even with one step (`sql/join.go:513-517`):
  pass `rowVals` through when `len(partial) == 0`. About 10%.
- `LIKE` re-translates the pattern per row behind a global `RWMutex`
  (`sql/exec.go:241-251`, `sql/expr.go:571-628`): precompile for a literal
  or bound pattern. About +100 ns/row today. Add a `LIKE 'prefix%'` range
  pushdown on text keys while there.

### D-45 · Engine write-path allocations · S each
**Verified:** agent, by benchmark
- Every descriptor lookup copies the whole descriptor JSON
  (`engine.go:767`, `bs := string(blob)` before the cache compare):
  `Engine.Get` is 1429 B / 30 allocs against `Txn.Get`'s 581 B / 18. Compare
  `string(blob) == ent.blob` inline (the compiler elides the allocation) and
  materialize only on a miss.
- Key building re-encodes constant prefixes into zero-capacity buffers
  (`engine.go:856-917`, `index.go:330`): cache per-table and per-index
  prefix bytes on the parsed `TableDesc`; start key buffers at capacity 64.
- `Update` recomputes old and new entries for every index even when no
  indexed or key column changed (`dml.go:269-286`): 55 allocs / 2052 ns with
  two indexes vs 31 / 1448 with none. Skip an index whose columns `set` does
  not touch when the primary key is unchanged.
- `Txn.ReferencingFKs` rescans the catalog per call (`fk.go:233-255`), once
  per UPDATE/DELETE statement: 174 µs / 5,027 allocs at 500 tables. Memoize
  per `Txn`.
- UPDATE re-verifies every FK per row even when no FK column is in SET
  (`sql/exec.go:1226-1231`): skip (rides with D-31).
- `curGID()` (`runtime.Stack`, 1.35 µs) runs on every `WriteTxn` in OCC
  mode, where the marker is never read (`txn.go:155-160`, `239`): guard with
  `if !e.occ`. Note D-12 makes the marker meaningful during DDL under OCC, so
  do D-12 first and keep the store on that path.
- Default-mode identity draws stage a counter write per row
  (`seq.go:31-47`, `identity.go:64-72`): 1622 ns / 116 WAL bytes per row vs
  901 ns / 59. Keep the counter in the `Txn` and write once at commit and at
  savepoint marks (M); build the error-text name lazily (S).
- `CreateIndex` backfill decodes with a cloned descriptor that lacks the
  ID→ordinal table (`index.go:84,101`; read): decode with `old`.

### D-46 · Read scaling for one-shot `Engine.Get` · M
**Verified:** agent (measurement); cause by reading
`Engine.Get` is 1101 ns serial and 940 ns/op across 8 goroutines, because
each call opens a KV transaction that takes `db.mu` exclusively at Begin and
Rollback. `Txn.Get` inside one `ReadTxn` per goroutine scales (577 → 208 ns).
Have the engine reuse one shared read snapshot per commit version (atomic
pointer swapped on commit), and document `ReadTxn` for read-heavy loops in
the meantime.

### D-47 · Catalog virtual tables rebuilt in full, twice, per query · S then M
**Verified:** agent, by profile · `sql/syscat.go:57-61`
With 200 tables: `information_schema.columns` for one table costs 861 µs /
1.97 MB, `pg_attribute ⋈ pg_class` 3.0 ms / 5.2 MB. Cost is linear in schema
size, so per-table introspection at ORM startup is quadratic.
(S) Use the shape-only lookup (`catalogShapes`) in typing and describe, which
halves it. (M) Memoize rows per catalog version (D-40 step 2), or push
`table_name = 'x'` into the builder.

### D-48 · Startup replay · M, btypedb
**Verified:** main session
Open replays the whole log single-threaded: 928 ms for 1M rows with one
index (2.2M records, 169 MB), about 0.4 µs per key and 3.5 allocations per
record. A 50M-row database is roughly 45 s of downtime per restart.
- `readRecord` allocates a fresh body per record and hashes with a
  heap-allocated `crc32.NewIEEE()`; use `crc32.Update` over a reused read
  buffer and allocate only the retained key/value bytes.
- The compacted head of the log is in key order. Detect a sorted run during
  replay and bulk-load it bottom-up instead of inserting key by key. Needs a
  look at whether `tidwall/btype` exposes a bulk-load or append-hint path;
  if not, a hinted set (remember the last leaf) gets most of the gain.
- A two-stage pipeline (reader + CRC on one goroutine, apply on another) is
  the fallback if bulk-load is not available.
- Document the restart cost per million rows in `docs/gotchas.md` either way.

### D-49 · Memory per row · L, later
**Verified:** main session
284 bytes of heap per row for a ~45-byte row with one secondary index. Each
entry is a separately allocated key string and value slice plus 40 bytes of
headers in the node. Options (arena-packed leaves, values as strings to drop
8 bytes of header, prefix-compressed keys) are all structural changes in
btypedb. **Not scheduled**; recorded so the number is on file. Add it to
`docs/gotchas.md`'s sizing section now so users can budget.

### D-50 · Streamed results · L, later
**Verified:** agent (allocation measured, consequence extrapolated) · `pgwire/conn.go:739-750`
Results are fully materialized before the first byte is sent: 730 B/row in
the engine plus 300–430 B/row of wire garbage. A row-callback execution path
for SELECT streamed to the socket is the real fix and is large. Cheap
mitigation now: Q6 (lower `bytdbd`'s default row cap) and D-41. **Not
scheduled** beyond the mitigation.

---

## Phase 8 — Convenience

### D-51 · Session and catalog surface that clients and tools send · M
**Verified:** agent (rejections confirmed by 76 probes; which tool sends
which statement is from the reviewers' memory, so confirm each against the
tool before prioritizing by it)
- **Advisory locks:** `pg_advisory_lock`, `pg_try_advisory_lock`,
  `pg_advisory_unlock` as an in-process mutex map keyed by the bigint,
  released on session close. Believed required by golang-migrate.
- **Reset statements:** `DISCARD ALL`, `DEALLOCATE ALL`, `DEALLOCATE name`.
- **Bare keywords:** `current_user`, `session_user`, `user`,
  `current_schema`, `current_catalog` (only the `()` forms work today).
- **Functions:** `pg_backend_pid`, `current_setting`, `set_config`,
  `current_schemas`, `pg_is_in_recovery`, `txid_current`, `to_regclass`,
  `pg_get_serial_sequence`, `has_table_privilege`, `quote_ident`,
  `pg_typeof`.
- **SET forms:** `SET NAMES`, `SET ROLE`, `SET SESSION AUTHORIZATION`,
  dotted GUCs (`SET app.user_id = '5'`, readable through
  `current_setting`), `SET TRANSACTION READ ONLY`.
- **SHOW before SET:** `server_version_num`, `application_name`,
  `lock_timeout`, `max_connections`, `is_superuser`,
  `default_transaction_read_only`, `bytea_output`, `extra_float_digits`,
  `lc_collate`, `idle_in_transaction_session_timeout`.
- **Catalog tables:** `pg_tables`, `pg_indexes`, `pg_views`, `pg_settings`,
  `pg_description`, `pg_sequences`, `information_schema.schemata`, `.views`,
  `.referential_constraints`, `.constraint_column_usage`; add `relnatts` to
  `pg_class` and `numeric_precision`, `numeric_scale`, `datetime_precision`,
  `is_identity` to `information_schema.columns`.
- **Catalog rows that mislead:** `pg_class.relhasindex` is false for a
  table with only a primary key (`syscat.go:462`; psql `\d` is believed to
  gate its "Indexes:" query on it); `varchar(20)` reports as `text` with
  typmod -1 (`syscat.go:500`); `oid::regclass` yields the number and
  `'T'::regclass` / `'pg_class'::regclass` fail (`expr.go:848-868`); a serial
  default renders an unquoted mixed-case sequence name.
- **Protocol:** a minor version above 3.0 is closed silently
  (`pgwire/conn.go:438-439`); answer with `NegotiateProtocolVersion`.
- Extend `pgwire/orm_test.go` with the verbatim connect-time and
  migration-time statements of pgx, lib/pq, GORM, golang-migrate, goose and
  atlas, captured from the tools, which settles the attribution question.

### D-52 · Everyday SQL that fails to parse · S each unless marked
**Verified:** agent (two reviewers); `CAST`, `abs`, `INSERT ... SELECT` both
- `CAST(x AS t)`; parameterized cast targets (`::varchar(10)`,
  `::numeric(10,2)`, `::timestamp(3)`, `::character varying`); typed
  literals `date '…'`, `timestamp '…'`.
- `INSERT ... SELECT` (M): feed `execSelectWith`'s rows into the
  `execInsert` row loop.
- `SELECT ... FOR UPDATE / FOR SHARE [SKIP LOCKED | NOWAIT]` per Q3.
- `IS [NOT] DISTINCT FROM`, `IS [NOT] TRUE/FALSE`.
- `ORDER BY ... NULLS FIRST|LAST` (one flag in `orderCmp`, `exec.go:757`).
- `GROUP BY <output alias>` (ORDER BY already resolves aliases,
  `agg.go:358`).
- `INTERSECT` / `EXCEPT` (`dedupRows` plus a set probe); parenthesized UNION
  arms; `VALUES (...)` as a query.
- `JOIN ... USING (col)`, `LIMIT ALL`, `FETCH FIRST n ROWS ONLY`, row-value
  `(a, b) IN (...)`.
- `UPDATE ... FROM`, `DELETE ... USING`, `UPDATE t AS x` (M):
  `prepareFrom`/`runJoin` can produce the target rows.
- `SET col = DEFAULT`; dollar-quoting; adjacent string literal
  concatenation; `schema.table.column`; unquoted non-ASCII identifiers.

### D-53 · Scalar functions and aggregates · S each
**Verified:** agent
Each is one `case` in `evalFunc` plus a `funcTypes` entry (typed, per D-23).
Today an unknown function fails at row evaluation, not at prepare; make it a
prepare-time 42883.
- Math: `abs round ceil floor trunc mod power sqrt sign greatest least
  random`.
- String: `substring/substr left right trim/ltrim/rtrim/btrim replace concat
  concat_ws position/strpos split_part repeat reverse lpad rpad initcap
  starts_with md5 ascii chr octet_length`.
- Date/time: `extract`/`date_part`, `date_trunc`, `to_char`, `to_timestamp`,
  `make_date`, `AT TIME ZONE`.
- Aggregates: `string_agg`, `array_agg` (`text[]` exists), `bool_and`,
  `bool_or`, `count(*) FILTER (WHERE …)`.
- jsonb: `jsonb_typeof`, `jsonb_array_length`.
- `length(bytea)`, `length(123)`, `upper(1)` return NULL silently today;
  they become correct or a type error under D-23.

### D-54 · Migration and dump DDL · S each
**Verified:** agent
- Parse and ignore, like `ALTER ... OWNER TO`: `CREATE SCHEMA [IF NOT
  EXISTS]`, `CREATE EXTENSION [IF NOT EXISTS]`, `COMMENT ON`, `ANALYZE`,
  `LOCK TABLE`, `CREATE INDEX CONCURRENTLY`, `USING btree`, `ADD CONSTRAINT
  ... NOT VALID`, `ALTER TABLE ONLY`.
- Implement: unnamed `CREATE INDEX ON t (c)`, `DROP TABLE a, b`, `DROP ...
  CASCADE` where nothing depends on it, `ALTER TABLE IF EXISTS`, `DROP
  COLUMN IF EXISTS`, `DEFAULT 'x'::text`, `DEFAULT (0)`, `CREATE TABLE AS`.
- `DEFAULT gen_random_uuid()` (Q5): one more marker in `defaultLiteral`
  (`parser.go:1037`) beside `now()`. Makes `id uuid DEFAULT
  gen_random_uuid() PRIMARY KEY` work, which matters more once D-37 makes
  uuid keys fast.

### D-55 · Wider input formats · S–M
**Verified:** agent · root `ParseTimestamp` / `ParseDate`
Timestamps: `'2024-02-29 12:00'` (no seconds), a trailing `UTC` or named
zone, `'epoch'`, `'infinity'`, `'now'`. Dates: `'2024-2-9'`, `'20240229'`,
`'today'`. Booleans: `'yes'`, `'on'`, `'no'`, `'off'`. UUID: `'{…}'`.
Float infinity is spelled `+Inf` on the wire (`pgwire/values.go:167`);
Postgres spells it `Infinity`.

### D-56 · Go embedding API · S each
**Verified:** agent
- **Sentinel errors:** `ErrDuplicateKey`, `ErrUniqueViolation`,
  `ErrNoSuchTable`, `ErrNotNull`, wrapped where the `serr.New` strings are
  today (`dml.go:134,155,259,283,586`, `engine.go:790`), so callers use
  `errors.Is`. The SQLSTATE mapping in `pgwire/errors.go` can then match on
  them instead of message text.
- **Readable errors:** `err.Error()` is only `"syntax error"` / `"no such
  column"` / `"unknown function"`; position and name live in serr fields.
  Fold the key fields into the message (`syntax error at or near "insrt"
  (line 1, col 1)`).
- **Argument binding** (`sql/params.go:493-527`, `dml.go:622-671`): accept
  named types by reflect kind (`type ID int`, `type UUID [16]byte`),
  `[]string` for `text[]`, `driver.Valuer`, pointers, `uint`/`uint64` in
  range, `int32` into float columns, `int` into timestamp columns.
- **Scripts:** export the statement splitter from `pgwire/split.go` into
  `sql/` and add `DB.ExecScript`; `stdlib` `Exec` with several statements
  uses it (`stdlib/stdlib.go:474`), so migration runners that pass a whole
  file work.
- **Row access:** `Row.Col` panics on the zero `Row` a miss returns
  (`dml.go:22-27`); make it return nil. Add `Row.Map()` and typed getters.
  Make `Update`'s `pkVals` variadic-consistent with `Get`/`Delete` through a
  new method rather than a signature change.
- **Catalog accessors that swallow errors** (`ddl.go:890-919`,
  `seqobj.go:178-214`, `view.go:73-107`): add error-returning variants so
  "closed engine" is distinguishable from "does not exist".
- **Missing one-shots:** `Engine.Truncate`, `Engine.NextVal`/`SetVal`, and a
  key-only `Count`/`Exists` that does not decode rows.
- **A retry helper for `ErrTxConflict`** stays declined (N-005).
- Fix stale comments at `txn.go:161-167,191-196` and `engine.go:415` that
  say btypedb's `Update` has no recover; it has one.

### D-57 · stdlib driver · S each
**Verified:** agent
- `[]string` → `text[]` and `[16]byte` → `uuid` through the value converter
  (`stdlib/stdlib.go:523-534`); `date` columns scan into `time.Time`.
- DSN options for memory ceiling, row cap, write cap, compaction and
  `:memory:`.
- Resolve symlinks in the path before the engine-sharing lookup
  (`stdlib.go:327`), so two spellings of one file share an engine instead of
  hitting `ErrLocked`.
- `encryption_key` in the DSN ends up in logs; add
  `encryption_key_file=` and document it as the preferred form.

### D-58 · `bytdbd` operability · S each, M for replication
**Verified:** agent, by reading `pgwire/cmd/bytdbd/main.go`
- `-max-conns` help says "0 = unlimited" (`main.go:56-57`); the code and
  `docs/security.md` say 0 means 100. Fix the help text.
- Flags for `WriteTimeout`, `ReadTimeout`, `IdleTimeout`, concurrent writes,
  auto-compaction thresholds, `-socket` (D-42), `-shutdown-grace` (D-35).
- **Replication from the daemon** (M): `-replicate-s3 …`, `-restore-from …`
  flags, so replication no longer requires writing a Go program.
- A health endpoint on the metrics listener; replication metrics
  (`LagBytes`, `Sealed`, last ship time, last error).
- SIGHUP reloads the auth file and the TLS certificate.

---

## Phase 9 — Documentation corrections

Do these with the phase that makes them true, not as a batch at the end.

| Doc | Change | With |
|---|---|---|
| `docs/benchmarks.md` | The scan gap is row decoding, not interpreted execution; re-run numbers | D-39 |
| `docs/replication.md` | A re-ship is not always the identical range; document overlap tolerance, `Sealed`/`LagBytes`, counter ordering | D-05, D-34 |
| `docs/security.md` | `statement_timeout` did not bound lock waits; now it does | D-33 |
| `docs/gotchas.md` | Remove the unindexed `ORDER BY … LIMIT` row-cap edge and the `gen_random_uuid()` default omission; add restart cost and bytes per row; add the batch-with-DDL carve-out; add NaN handling | D-43, D-54, D-48, D-49, D-32, D-36 |
| `docs/concurrency.md` | Failed uniqueness probes are validated reads; `FOR UPDATE` semantics per mode | D-11, D-52 |
| `docs/testing.md` | CI, the differential tier, the benchmark practice | D-02, D-03, D-04 |
| `docs/features.md`, bytdb skill | New SQL surface and functions | D-51 … D-54 |
| `docs/stdlib.md`, `docs/cli.md` | New DSN options and daemon flags | D-57, D-58 |

---

## Sequencing and releases

```
Phase 0  safety net ──┬─► Phase 1  small fixes ─────────────► release A
  D-01 D-03 first     │
  D-02 D-04 alongside ├─► Phase 2  typed bind pass ──┬──────► release B
                      │     D-23 → D-24 D-25 D-26     │
                      │                               ├─► D-37 uuid/ts pushdown
                      │                               ├─► D-39 lazy decode
                      │                               └─► D-53 typed functions
                      ├─► Phase 3  subqueries (after D-23; removes D-09)
                      ├─► Phase 4  atomicity ─────────────────► release C
                      ├─► Phase 5  ctx lock (btypedb tag first)
                      ├─► Phase 6  replication + server
                      ├─► Phase 7  performance (D-40 version counter early:
                      │            D-23 re-typing and D-47 both key on it)
                      └─► Phase 8  convenience (independent; fill gaps)
```

- **Release A** (patch-level, soon): Phase 0 + Phase 1. Every item is a
  small, isolated fix; several close outage or data-loss paths. Suggested
  split if A is too big for one tag: D-05 … D-08 and D-14 first.
- **Release B** (minor): Phase 2 + Phase 3 + D-37. This changes behaviour:
  comparisons that silently returned nothing will now either match or
  error. Release notes must list the newly rejected shapes (`1 < 'a'`,
  `ts + 1`) and the CHECK constraints that start being enforced. **Existing
  databases may hold rows that violate a date/timestamp CHECK**, admitted
  while the check was inert. Ship a one-off validation helper (`VACUUM
  (VERIFY)` or a `bytdb` shell `.check` command that re-evaluates every
  CHECK over every row and reports violations) with this release; do not
  fail `Open` on them.
- **Release C** (minor): Phase 4 + Phase 5. Needs btypedb v0.10.0 tagged
  first for D-33; follow the `release` skill's lockstep steps for root and
  `pgwire/`.
- Phases 6–8 ship incrementally after that, in whatever order demand sets.

### Cross-repo work (btypedb)

| Item | Change | Needs a tag before |
|---|---|---|
| D-33 | `BeginCtx` / `UpdateCtx`, semaphore writer lock | Release C |
| D-48 | Replay: reused buffers, bulk-load of sorted runs | any time |
| D-46 | Shared read snapshot per commit version (may be engine-side only) | any time |
| D-03 | CI workflow | now |

### Risk notes

- **D-23 is the one change that can regress working queries.** Mitigations:
  land D-02 first with a broad corpus; keep `coerceLiterals` behind an
  internal switch for one release so a field report can be bisected; run
  `sql/plan_property_test.go` and `FuzzExec` extended to the new types.
- **D-32** changes commit timing for batches. A client relying on
  per-statement commits inside one simple Query will see different failure
  behaviour. Release-note it.
- **D-33** replaces a mutex with a channel on the hottest lock in the
  system. Benchmark the uncontended single-writer path (`bench/`
  head-to-head) before and after; the budget is no measurable regression.
- **D-36** touches the persistent tuple encoding. It only adds a case, but
  run `FuzzTupleOrder` and `FuzzTupleRoundTrip` for an extended period and
  confirm files written before the change still open.

## Checked and found sound (do not re-audit)

Recorded so the next review can skip them.
- Index maintenance across unique NULL/value form switches, descending and
  composite unique indexes, PK moves, savepoint rollbacks, `DropColumn`
  ordinal shifts, backfills, type changes on indexed columns, renames and a
  reopen (model-based test, 6 seeds).
- OCC writers against create/drop index and add/drop column churn: 1,179
  commits, 290 conflicts, no drift.
- Planner pushdown and ORDER BY elision: 6,000 random queries against an
  index-free twin, zero mismatches (int/float/text only; D-37 extends this).
- LEFT/INNER joins, ON vs WHERE conjuncts: 20,000 random joins against a
  brute-force three-valued oracle, zero mismatches. Hash-join key equality
  across int/float/NULL.
- Three-valued logic; integer overflow, `MinInt64 / -1`, division by zero;
  float rounding and NaN ordering; LIKE/ILIKE escapes on multi-byte text.
- Session state: runtime errors abort a block, savepoints, READ ONLY blocks,
  timeout expiry leaves no partial writes. OCC autocommit retry loses and
  doubles nothing.
- Extended-protocol state machine, cancellation, disconnect while holding
  the writer lock, SCRAM (read only), binary/text codecs for plain columns.
- stdlib `database/sql` contract; replicate epoch changes, prune, restore
  locking and atomic rename; SigV4.
- Descriptor cache staleness, counter allocators, `writerGID` ordering,
  tuple `PrefixEnd`, memory guard and write cap accounting, DDL atomicity.
- Not audited: the `bytdb` interactive shell (`pgwire/cmd/bytdb/`); TLS and
  SCRAM were read, not exercised.
