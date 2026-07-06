# Session: bytdb-milestone-13-transaction-blocks

- **Session ID**: `467e3fb2-67f2-41dc-813f-484c65a33386` (continues `2026-0704-1128-bytdb-milestone-12-expression-language.md`)
- **Date**: 2026-07-05
- **Repo**: `~/projs/go/bytdb`
- **Result**: milestone 13 complete — BEGIN/COMMIT/ROLLBACK transaction blocks at all three layers (engine, sql, wire), verified against psql 17.5 and pgx (incl. TxStatus bytes, notices, ErrTxCommitRollback). Both modules green under `-race`. Commit `f05f070`, pushed.

## Design: three thin layers, one invariant

1. **Engine (txn.go)**: `Engine.Begin(writable)` / `Txn.Commit()` / `Txn.Rollback()` — btypedb v0.3.0 already had manual `Begin/Commit/Rollback` (View/Update are just wrappers), so this is ~15 lines: `e.kv.Begin(writable)` + `catalogSnapshot()`. A writable tx holds btypedb's single-writer lock (`writerMu`) from Begin to end; read txs take no lock and never block.
2. **sql.Session (session.go)**: per-connection state `{db, sdb, tx, readOnly, aborted}` wrapping DB. `NewSession()` on DB; `Exec`/`ExecStmt` funnel to `run(st, args)` which intercepts `*TxnControl`, enforces the aborted state, and dispatches everything else either to `db.run` (autocommit) or `sdb.run` where `sdb = &DB{e, tx}`.
3. **Executor threading is just a field**: DB grew a `tx *bytdb.Txn`; the five `d.e.ReadTxn(...)`/`d.e.WriteTxn(...)` call sites (execSelect, execSelectAgg, execInsert/Update/Delete) became `d.read(...)`/`d.write(...)` which run the closure against `d.tx` when set. No signature changes anywhere — subqueries/joins already take tx through exEnv. `Stmt` re-execution, params, coercion all untouched (coerce reads the engine catalog, which can't change in-block since DDL is refused).

**The invariant that makes error handling correct**: any error inside a block sets `aborted`; from then on everything but COMMIT/ROLLBACK gets "current transaction is aborted..." (25P02), and COMMIT rolls back, returning `Result.Tag = "ROLLBACK"`. This is also what preserves statement atomicity without savepoints — a multi-row INSERT that dies halfway has rows staged in the open tx, but the failed state guarantees they can only ever be rolled back.

## Postgres semantics replicated

- BEGIN in a block / COMMIT-ROLLBACK outside one: warning + no-op. `Result` grew `Notice string` and `Tag string` (tag override) fields.
- `BEGIN [WORK|TRANSACTION]` + modes in any order with optional commas: `ISOLATION LEVEL ...` accepted and *ignored* (single-writer = serializable, satisfies every level), `READ ONLY`/`READ WRITE` honored (later wins), `[NOT] DEFERRABLE` swallowed. `START TRANSACTION` keeps its own tag; END→COMMIT, ABORT→ROLLBACK. `COMMIT/ROLLBACK [WORK|TRANSACTION] [AND NO CHAIN]`; `AND CHAIN` errors. `ROLLBACK TO SAVEPOINT` fails as trailing-token syntax error (savepoints deferred).
- Writes in a READ ONLY block: pre-checked in Session (`isWrite`) → "cannot execute X in a read-only transaction" (25006); btypedb's ErrTxNotWritable is the safety net.
- **DDL refused in-block** (`isDDL` → "X cannot run inside a transaction block", 25001): engine DDL runs its own kv.Update, which would deadlock behind the block's own writerMu — the guard isn't optional.
- Bare `DB.Exec("BEGIN")` errors ("transaction control statements require a Session").

## pgwire integration

- One Session per conn (created in Serve, `defer sess.Close()` → dropped connection rolls back and releases the writer lock — tested).
- `ready()` sends `sess.Status()` byte: 'I'/'T'/'E' (TxStatus constants are the wire bytes).
- simpleQuery + execute both route through `c.sess.ExecStmt`; `res.Notice` → NoticeResponse (`msgNoticeResponse = 'N'`, `noticeBody`: WARNING severity, 25001 already-in-progress / 25P01 no-transaction); `res.Tag` overrides the command tag in sendCommandComplete.
- sqlstate additions: 25P02 (aborted), 25006 (read-only), 25001 (cannot-run-in-block).
- Extended-protocol inErr/Sync unchanged — Sync doesn't end an explicit block; session state carries it, which is the correct protocol behavior.

## Concurrency notes (documented, not hidden)

- A writable block holds the engine writer lock for its whole life: other sessions' writes and one-shot DML **block** behind it (a second `BEGIN` on another conn blocks inside Begin until the first commits). Reads and read-only blocks never block — they're snapshots. This is inherent single-writer serializability, stated in sql.go/pgwire.go/README docs.
- Deadlock-on-disconnect resolved naturally: closing the socket unblocks the conn goroutine's read → defer Close → rollback → lock released (TestTransactionDisconnectReleases).

## Testing

- Engine: `TestManualTxn` — commit publishes, rollback discards, read tx is a stable snapshot + refuses writes (ErrTxNotWritable).
- sql `session_test.go`: commit/rollback visibility (uncommitted invisible to autocommit readers), aborted-block flow (25P02, COMMIT→Tag ROLLBACK, pre-error staged rows gone), partial multi-row INSERT can't leak, notices for redundant control, READ ONLY (snapshot doesn't see concurrent commit; write fails the block), DDL refusal, params + prepared stmts in blocks, `TestTxnControlParsing` (all forms/tags, rejects AND CHAIN / savepoints / `start work`).
- pgwire `txn_test.go`: pgx Begin/Commit/Rollback with second-conn visibility, TxStatus 'T'/'E'/'I', 25P02 as pgconn.PgError, `pgx.ErrTxCommitRollback` on COMMIT of failed block, OnNotice capture (exact WARNING texts + codes), database/sql Tx, disconnect releases writer lock.
- Manual psql 17.5: full scenario script — every tag, warning, and error matched Postgres behavior.

## State at end of session

- Milestones 1–13 done; `f05f070` pushed to origin/main. Deps unchanged (btypedb v0.3.0 already sufficed).

## Next (deferred)

- Savepoints (btypedb has no nested tx — would need statement-level undo or a staging layer).
- Wire: cancellation, COPY, portal suspension.
- Engine: DESC key columns, CHECK/NOT NULL, EXPLAIN.
- SQL: GROUP BY ordinals/expressions; expr items in aggregate queries; `= ANY(array)` for real.
- Someday: tag bytdb v0.x so pgwire's `replace` can become a versioned require.
