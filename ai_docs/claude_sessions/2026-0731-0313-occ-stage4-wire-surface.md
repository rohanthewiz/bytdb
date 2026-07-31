# Session: OCC Stage 4 — wire surface: SQLSTATE 40001, autocommit retry, SQL isolation levels

- **Session ID:** `a1f68c77-8866-4a50-9417-b9a9c27c1e5a`
- **Date:** 2026-07-31, 03:13
- **Deliverable:** bytdb branch **`occ-stage2`** commit `b74fc5f` (on top of
  Stage 3's `436500b`). No btypedb changes — Stage 3's `occ-stage1` branch
  already had everything the wire surface needs.
- **Design:** `ai_docs/plans/2026-0730-concurrent-writes-occ.md` (Stage 4 section)
- **Prior session:** `2026-0731-0022-occ-stage3-serializable.md`
- **This completes all four stages of the concurrent-writes plan.**

## What was built

Stage 4 of the concurrent-writes plan: the client-facing surface. A
transaction that loses optimistic validation now reaches wire clients
as Postgres's canonical serialization failure; autocommit statements
absorb bounded retries first; and the SQL dialect's isolation-level
forms actually select isolation instead of parsing to nothing.

### Engine surface (err.go, engine.go, txn.go)

- `bytdb.ErrTxConflict` — alias re-export of btypedb's sentinel, so
  the sql and pgwire layers match with `errors.Is` without importing
  btypedb (same value, so `Is` works against either spelling).
- `Engine.ConcurrentWrites() bool` — layers above use it to describe
  isolation honestly and to skip read tracking where the single-writer
  default is serializable for free.
- `Txn.TrackReads()` — mid-transaction upgrade to SERIALIZABLE, for
  the one caller that learns the level after Begin (SET TRANSACTION as
  a block's first statement). Reads before the call are not
  retroactively tracked; the SQL layer enforces "before any query".

### sql: isolation levels honored (parser.go, ast.go, session.go, vars.go)

- `TxnControl.Isolation` captured by the parser ("" = defer to session
  default — distinct from an explicitly requested weaker level).
  Shared `isolationLevel()` helper normalizes to Postgres's lowercase
  spellings; READ UNCOMMITTED folds to read committed, as in Postgres.
- `BEGIN ISOLATION LEVEL SERIALIZABLE` → `Engine.BeginSerializable`,
  gated on writable + `ConcurrentWrites()` (plain `Begin` elsewhere:
  weaker levels, read-only blocks, and the single-writer mode all
  already get at least what was asked).
- `SET TRANSACTION ISOLATION LEVEL x` parses (also the equivalent
  parameter form `SET transaction_isolation = ...`); the Session
  applies it only inside a block and only before the first query
  (`txStmts` flag, set when a block statement reaches the executor) —
  after that it fails with Postgres's exact wording. Outside a block
  it warns ("SET TRANSACTION can only be used in transaction blocks"),
  since drivers issue it unconditionally.
- `SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL x`
  (JDBC's spelling) lowers to `default_transaction_isolation`. The
  default makes bare BEGINs serializable AND autocommit writes too
  (new `DB.serial` field → `WriteTxnSerializable` in `DB.write` and
  the SELECT-writes-sequences path): two overlapping single statements
  can write-skew just like two blocks. `RESET ALL` clears it.
- `SHOW transaction_isolation` is live, not static: `isoDefault(occ)`
  reports "repeatable read" under concurrent writes (Postgres's name
  for snapshot isolation), "serializable" in single-writer mode or in
  an opted-up block. `execShow` gained an iso-overlay parameter; the
  two isolation entries stay in `showDefaults` only so SHOW ALL
  enumerates them.

### sql: autocommit retry (sql.go)

- Lives in `DB.run` — NOT pgwire — so embedded `sql.DB`/Session users
  get it too: up to `autocommitRetries = 3` silent re-runs on
  `ErrTxConflict` when `d.tx == nil`, checking the statement's ctx
  between attempts. Safe precisely because autocommit: no intermediate
  result ever reached the client, so the statement is fully replayable.
- Blocks are never retried, and that's structural, not a check: a
  block statement stages writes without committing, so its conflict
  can only surface at COMMIT — which the session never re-runs.
- Test seam `testInjectConflicts atomic.Int64`: converts that many
  successful autocommit results into ErrTxConflict (inside the
  per-attempt closure, so retries see it too — first version only
  injected on attempt #1 and the exhaustion test caught it). Injection
  happens after real effects apply, so tests must use idempotent
  statements.

### pgwire: 40001 (errors.go, pgwire.go)

- `errorBody` checks `errors.Is(err, bytdb.ErrTxConflict)` before
  message matching → `conflictBody()`: SQLSTATE **40001**, message
  "could not serialize access due to concurrent update", hint "The
  transaction might succeed if retried." — all three are the contract
  pools and retry middleware match on. (Postgres uses a different
  message for serializable read-dependency failures; one ErrTxConflict
  can't distinguish, so the SI wording serves both.)
- sqlstate: "must be called before any query" → 25001; noticeBody:
  "can only be used in transaction blocks" → 25P01.
- Package doc + `DefaultIdleTxTimeout` comment made mode-aware (under
  occ no lock is held, but the timeout stays: an abandoned block pins
  its snapshot and would lose its eventual commit anyway).

## Semantics pinned / documented

- Who retries: server ×3 for autocommit; never for blocks; caller for
  embedded Engine one-shots and WriteTxn; nobody for DDL (updateDDL
  always makes progress, never returns 40001).
- Serializable guarantee spans only the transactions that ask for it;
  mixed SI/serializable can still skew (as in Postgres).
- No isolation downgrade mid-block: once tracking, a weaker requested
  level just validates reads nobody required (sound, conservative).

## Tests (all green, plain + `-race`, both modules)

- sql `isolation_test.go` (9): write-skew committed under plain OCC
  blocks (SI baseline) and conflicted under serializable ones through
  real Sessions; SET TRANSACTION first-statement equivalence; late SET
  refused + block failed + stray-SET notice; SESSION CHARACTERISTICS
  and default_transaction_isolation (both spellings) make bare BEGINs
  conflict, RESET ALL restores, bogus level errors; SHOW by mode
  (bare DB + session + in-block, occ and single-writer); serializable
  forms as no-ops in default mode; deterministic retry injection
  (budget exactly exhausted → succeeds; +1 → surfaces; block leaves
  the injected counter untouched); 6×25 concurrent increments with
  client-style re-run on conflict conserve exactly.
- pgwire `serializable_test.go` (4, real pgx against an occ server):
  same-row block conflict → 40001 with message+hint asserted
  field-by-field, failed COMMIT leaves TxStatus 'I', hinted retry
  wins; write skew over the wire (SI both-commit baseline, then
  serializable 40001); SHOW + SESSION CHARACTERISTICS + late SET
  TRANSACTION → 25001 over the wire; 4-conn × 20 autocommit
  contention conserves. Plus the 25001 case pinned in the
  sqlstate-contract table.

## Docs

- New `docs/concurrency.md` (+ mkdocs nav): the two modes with a
  mermaid commit-flow diagram, the who-retries contract table, a
  client 40001 retry-loop example, serializable opt-in in Go and SQL
  with its rules, sequence gap semantics, DDL's never-conflicts
  guarantee, and mode guidance quoting the measured numbers
  (SI 53.6µs / serializable 66µs / single-writer 150µs, 8 writers).
- `docs/architecture.md` ("default concurrency model" + pointer),
  `docs/features.md` (isolation bullet rewritten), sql package doc
  statement list, and the Session doc comment updated — no page or
  comment claims the single writer unconditionally anymore.

## State / next steps

- bytdb `occ-stage2` @ `b74fc5f`, local-only at commit time (pushed by
  this session's wrap-up). btypedb `occ-stage1` unchanged from Stage 3.
  bytdb main and all tags untouched.
- The 4-stage OCC plan is complete. Remaining decisions are release
  ones: merge occ-stage1 → btypedb main + tag, merge occ-stage2 →
  bytdb main, bump pins, tag root + pgwire (root first, as always).
- Deliberate leftover, documented: embedded Engine one-shot writes
  surface ErrTxConflict raw — the caller's retry loop, same contract
  as WriteTxn.
- Untracked `.cats-todo/` remains the user's local tooling; left
  uncommitted.
