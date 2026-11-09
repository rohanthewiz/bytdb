# Deep-look audit of bytdb and the remediation plan

- **Session:** `7839f476-61b1-40c0-a57b-3911ff60eda9`
- **Date:** 2026-11-09
- **Scope:** read-only audit of bytdb for reliability, performance and
  convenience, then a written plan. No product code changed in bytdb or
  btypedb. Output: `ai_docs/todo/2026-1109-deep-look-remediation-plan.md`
  and nine new Open items on the next list.
- **Baseline:** bytdb `6ccf3cc`, btypedb `2383665`. `go vet ./...` and
  `go test -race -count=1 ./...` were green for the root and `pgwire/`
  modules before the audit started.

A false start is worth recording: the first request named rweb by mistake.
A survey of `~/projs/go/rweb` had begun (layout, next list, the first 700
lines of `Server.go`) when the user corrected it to bytdb. Nothing was
changed there and nothing from it carries forward.

## 1. How the audit was run

bytdb is about 37,700 lines of non-test Go and has been through several
audits already (`ai_docs/production-readiness-audit.md`, the 2026-07-22
production comb), so the brief was to find what earlier passes missed.

Four reviewers ran in parallel, one per area, each told to read
`docs/gotchas.md`, `docs/concurrency.md` and the next list's Non-goals and
Roadmap first, and to treat anything documented there as not a finding:

| Reviewer | Scope |
|---|---|
| Engine | Root package and `tuple/` |
| SQL executor | `exec.go`, `plan.go`, `join.go`, `agg.go`, `window.go`, subquery, upsert, FK, views |
| SQL front end | Lexer, parser, expressions, coercion, params, session, syscat |
| Edges | `pgwire/` and its commands, `stdlib/`, `replicate/` |

**Evidence rules.** Every finding needed `file:line` and a mechanism. A bug
had to be proved with a runnable repro in a scratch module outside the
repositories (a `go.mod` with `replace` directives, run with `GOWORK=off`),
and each finding was labelled CONFIRMED, LIKELY or SPECULATIVE. Each reviewer
also returned a "checked and fine" list. Performance claims had to be
measured.

**Independent re-verification.** The main session re-ran each reviewer's
headline repros, or re-derived them with fresh probes, before reporting
them. The plan marks every item `both`, `agent` or `read` accordingly. Not
re-run in the main session: the stale-prepared-statement and write-deadline
repros, the UPDATE own-writes and two-snapshot UNION repros, per-row FK
checks, and the reviewers' benchmark numbers (theirs are from an Apple M3;
main-session measurements are from an M1 Pro).

**"Postgres gives" claims are from knowledge.** Nobody ran Postgres. The
plan's differential harness (D-02) is what makes them checkable.

**System-level measurement** (main session), since no numbers were on file:

| Measurement | 200k rows | 1M rows |
|---|---|---|
| Bulk load, `SyncNever` | 3.5 µs/row | 4.3 µs/row |
| Heap per row (4 columns, one secondary index) | 298 B | 284 B |
| Log file | 30 MB | 150 MB |
| `Open` (full replay, after 20% update churn) | 162 ms | 928 ms |
| `Compact` | 41 ms | 139 ms |
| `count(*), sum(amount)` | 53 ms | 267 ms |

Replay is single-threaded at about 0.4 µs per key with 3.5 allocations per
record (`btypedb/wal.go` `readRecord` allocates a body per record and a
`crc32.NewIEEE()` hasher).

Also found by inspection: neither bytdb nor btypedb has any CI
(no `.github/`), and there is no differential test against Postgres.

## 2. What the audit found

The storage and transaction core held up. Index maintenance passed a
model-based drift test, OCC writers passed a stress test against DDL churn,
6,000 random planner queries matched an index-free twin, and 20,000 random
joins matched a brute-force oracle. The findings cluster in three places.

### 2.1 Silent wrong results in SQL

Two root causes account for most of them.

**Values lose their SQL type at runtime.** A timestamp and a date are both
`int64`; a uuid is `[]byte`. `coerceLiterals` (`sql/coerce.go`) types only
the top-level statement's `Pred` leaves; everything else falls to
`checkPred`'s fallback (`sql/exec.go:293-318`), which guesses from the Go
kind, fails, and swallows the error. Reproduced:

- `WHERE due > now()` on a 2099 date returns 0 rows.
- `ts::date` returns raw microseconds.
- `CASE WHEN at > '2020-01-01' ...` takes the wrong branch.
- `CHECK (day >= '2000-01-01')` accepted `'1999-01-01'`: a declared
  constraint is not enforced.
- The same date predicate inside an `IN (SELECT ...)` matches nothing
  (the reviewer reports the same for CTEs, derived tables and views).
- Reviewer-confirmed: `ALTER COLUMN ts TYPE date USING ts::date` stores
  microseconds as a date.

**Subqueries run through a separate, reduced executor**
(`sql/expr.go:1323-1590`) that ignores `ORDER BY`, `LIMIT` and `OFFSET`.
`SELECT (SELECT amt FROM o ORDER BY amt DESC LIMIT 1)` returns 10 where the
answer is 99; `IN (SELECT ... LIMIT 2)` matches all five rows;
`EXISTS (... LIMIT 0)` is true; `(SELECT coalesce(sum(amt),0) ...)` returns
NULL on empty input.

Others reproduced: a syntax error inside `BEGIN` does not abort the block and
`COMMIT` then persists the earlier statements; an upsert that changes a
referenced key skips the parent-side FK check and orphans child rows;
SERIALIZABLE misses write skew when the read is a failed duplicate-key
insert (`dml.go:121-146`).

### 2.2 Outage and data-loss paths

- **Replication** (`replicate/replicator.go:243-266`,
  `restore.go:191-200`): a PUT that is stored but reports an error, followed
  by log growth, leaves two chunks with the same start. `contiguousChain`
  breaks on the second, the generation becomes unrestorable, and `Restore`
  silently returns an older one while `Status().LastError` is nil.
- **Stack overflow from SQL text**: a 1M-term `1+1+1...` chain kills the
  process. The iterative operator loops in `sql/parser.go` never trip
  `maxParseDepth` yet build an AST as deep as the chain.
- **Writer-lock waits ignore context, `statement_timeout` and cancel.** A
  200 ms context was still blocked after 3 s, and the abandoned write
  commits later.
- **Permanent write hangs**: an engine write from a DDL callback
  (`engine.go:408-449` never arms the reentrancy guard), and
  `CREATE SEQUENCE ... CACHE 2^50` under concurrent writes.
- **`Serve` returns on a transient `accept` error**, which exits `bytdbd`.

### 2.3 Real clients do not work cleanly

- pgx's liveness check sends `-- ping`, which is a syntax error: `db.Ping()`
  fails and five queries a second apart used five connections.
- Parameter and result types fall back to text outside top-level
  predicates, so pgx cannot encode ints into `IN ($1,$2)` or scan
  `coalesce(sum(x),0)`.
- Multi-statement queries and `pgx.Batch` are not atomic.
- `CAST(x AS t)`, `INSERT ... SELECT`, and most scalar functions (`abs`,
  `round`, `substring`, `date_trunc`, ...) are missing.

### 2.4 Performance

| Finding | Measured |
|---|---|
| UUID, timestamp and date keys never push down (`sql/plan.go:302-320` `litFits`) | `Seq Scan` for a UUID PK lookup; UUID join 1.75 s vs 8 ms int-keyed |
| `IN (...)` / `= ANY` on a key is a full scan | 50 ms vs 5 µs point get |
| `decodeRow` decodes and boxes every column of every row | 77% of `count(*), sum()` time |
| Point reads re-derive everything per call; no plan cache, four snapshots | 5 µs today, about 2 µs reachable |
| Wire row encoding | 11–16 allocs/row; 172 ns → 18 ns with a reused buffer |
| Unix socket | 24 µs → 12 µs per point read over the wire |

`docs/benchmarks.md` attributes the scan gap to interpreted execution. The
profile says otherwise: the interpreter is a few percent and row decoding is
most of it.

## 3. The plan

`ai_docs/todo/2026-1109-deep-look-remediation-plan.md`: 58 items, D-01 to
D-58, each with location, inline repro, fix, effort and who verified it. The
D- prefix keeps them clear of the next list's N- IDs.

| Phase | Content |
|---|---|
| 0 | Safety net: repros as skipped failing tests, CI, Postgres differential harness, benchmark practice |
| 1 | Small isolated fixes (18 items, all S) |
| 2 | One static typing pass over the whole statement; replaces `coerceLiterals` |
| 3 | Subqueries through `runSelectBody` |
| 4 | Statement atomicity and one snapshot per statement |
| 5 | Context-aware writer lock (needs a btypedb tag first) |
| 6 | Replication and server hardening |
| 7 | Performance |
| 8 | Convenience: client/tool surface, SQL, functions, Go API, stdlib, `bytdbd` |
| 9 | Doc corrections, each done with the phase that makes it true |

**Design choices recorded in the plan:**

- **Phase 0 lands before anything else**, so every later fix un-skips an
  existing failing test.
- **The typing pass annotates the AST node**, not a side map, so a cached
  prepared statement stays self-describing. `coerceLiterals` is removed
  outright: two passes that both claim to type literals is how the gaps
  arose. The runtime fallback becomes an error (22P02), never "unknown".
- **Pushdown for UUID/timestamp/date waits for the typing pass**, so the
  literal is typed by one mechanism.
- **A catalog version counter** (D-40) is introduced early in Phase 7
  because the typing pass, the plan cache and the syscat cache all key on it.
- **Existing databases may hold rows that violate a date or timestamp
  CHECK**, admitted while the check was inert. The plan ships a validation
  helper with that release and does not fail `Open` on them.

**Six decisions are waiting on the user** (Q1–Q6 at the top of the plan,
each with a recommendation): reject `timestamp + 1`; implicit transaction
for multi-statement queries and batches; `FOR UPDATE` semantics per write
mode; an interim hard reject for subquery `ORDER BY`/`LIMIT`; whether to
take two items that revisit documented trade-offs; a lower default result
row cap for `bytdbd`.

**Caveats carried in the plan:**

- Which tool needs which missing statement (golang-migrate and advisory
  locks, goose and `pg_tables`, GORM and `pg_description`) is from the
  reviewers' memory. D-51 says to capture each tool's real statements before
  prioritizing by it.
- D-49 (memory per row) and D-50 (streamed results) are recorded but not
  scheduled.

## 4. State left behind

- **In the repo:** the plan, this doc, and the next-list edits. Nothing
  else.
- **Outside the repo:** the runnable repro modules are in a session-scoped
  scratchpad (`engine-audit`, `sqlexec-audit`, `sqlfront-audit`,
  `edge-audit`, `sys-measure`) and will not survive. The plan carries each
  repro inline so they can be rewritten; the user was offered a copy into
  the repo and had not answered when this doc was written. N-035 covers
  porting them.
- **btypedb:** untouched. Two plan items need changes there (D-33, D-48).

## Next

Closed: None. Declined: None. Raised: N-035, N-036, N-037, N-038, N-039,
N-040, N-041, N-042, N-043. Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
