# Session: bytdb-milestone-14-group-ordinals-and-savepoints

- **Session ID**: `de6dff2b-da5a-4014-bab0-883dba5af079` (continues `2026-0705-2156-bytdb-milestone-13-transaction-blocks.md`)
- **Date**: 2026-07-05 → 2026-07-06
- **Repos**: `~/projs/go/bytdb` and `~/projs/go/btypedb`
- **Result**: milestone 14 complete — GROUP BY ordinals + savepoints at all layers (btypedb kv, engine, sql, wire), verified against psql 17.5 and pgx nested transactions. btypedb `ca31f64` tagged **v0.4.0** and pushed; bytdb `88796f4` pushed with both go.mods bumped to the tag. All suites green under `-race`.

## Part 1: GROUP BY ordinals (bytdb only, small)

- AST: `GroupBy []ColRef` → `[]GroupItem{Col ColRef; Pos int64}` (Pos > 0: 1-based select-list position).
- Parser `groupItem()`: integer number → ordinal (`n < 1` errors "GROUP BY position is not in the select list" immediately); float or string literal → "non-integer constant in GROUP BY", matching PG's special-cased message; else colRef.
- `resolveAgg`: an ordinal indexes `s.Items` and must land on a **plain column** — aggregate → "aggregate functions are not allowed in GROUP BY"; literal/expression/t.* → existing not-supported errors; out of range → position error. Then it joins the normal ord/dup flow, so `GROUP BY city, 1` dupes are caught and ordinals mix freely with names.
- Docs: sql.go + README prose; "GROUP BY ordinals" removed from Later (expressions remain).

## Part 2: savepoints — the COW insight

The session's key design find: **btypedb's Tx already works on a private O(1) copy-on-write `dbState` plus an append-only `pending` WAL batch**, so a savepoint is just `{state.copy(), len(pending), nops}` — O(1) create, O(1) rollback, and rollback truncates the pending batch so the eventual commit logs exactly the surviving writes (no undo-log bloat, no inverse records). This beat the alternatives from the milestone-13 deferral note (statement-level undo, staging layer) on every axis.

### btypedb v0.4.0 (savepoint.go + tx.go)

- `Savepoint[K,V]{state, pendingLen, nops, active}`; Tx gains `saves []*Savepoint` (oldest first).
- `Tx.Savepoint()` / `RollbackTo(sp)` / `Release(sp)`. RollbackTo installs `sp.state.copy()` (sp stays valid for repeated rollbacks) and destroys later marks; Release keeps changes, destroys sp and everything after. `findSave` returns `ErrSavepointInvalid` for released/rolled-past/foreign savepoints.
- **Refcount rule respected**: `dbState.copy()`/`release()` mutate shared COW bookkeeping and must run under `db.mu` (short critical sections). Commit/Rollback call `releaseSaves()` under the same lock — no leaked snapshots regardless of how the tx ends. Read-only txs allow savepoints (PG does; harmless — state never mutates).
- Tests: WAL truncation proven by close/reopen replay; nesting destroys later marks; release; foreign/closed-tx errors; secondary-index trees and TTL/exp trees restored by rollback (they're all inside dbState, so it's free).

### bytdb engine (txn.go)

`type Savepoint = btypedb.Savepoint[string, []byte]` alias + three one-line wrappers. **No catalog mark needed**: DDL is refused inside blocks, so `t.tables` can't change between mark and rewind. Engine test covers index maintenance through rollback via ScanIndex.

### sql layer

- Parser: `SAVEPOINT name`, `RELEASE [SAVEPOINT] name` in statement(); `txnEnd` grows `ROLLBACK [WORK|TRANSACTION] TO [SAVEPOINT] name` (TO only when Kind is rollback, so `COMMIT TO x` stays a syntax error; ABORT TO parses — minor PG divergence, PG rejects it). TxnControl gains kinds `TxnSavepoint/TxnRelease/TxnRollbackTo` + `Name`; tags SAVEPOINT / RELEASE / ROLLBACK flow through the existing `command()` path.
- Session gains `saves []sesSave{name, sp}`; `savepointControl` semantics (all PG-verified):
  - Outside a block: **errors** (not warnings) — "X can only be used in transaction blocks".
  - Failed block: SAVEPOINT/RELEASE refused with 25P02; **ROLLBACK TO recovers** — rewind + `aborted = false`.
  - Names shadow (resolve newest-first); RELEASE unshadows the earlier same-named mark.
  - Unknown name → `savepoint "x" does not exist`, and it **fails the block** like any other error (a healthy block goes to E; test initially assumed otherwise and was wrong).
  - Correctness argument documented in session.go: every savepoint predates the block's first error (failed blocks refuse SAVEPOINT), so ROLLBACK TO always rewinds past a failed statement's partial writes — statement-level atomicity needs no implicit per-statement savepoints.
  - BEGIN/COMMIT/ROLLBACK/Close all clear `s.saves`.

### pgwire

- sqlstate additions: "can only be used in transaction blocks" → 25P01; savepoint + "does not exist" → 3B001.
- Tests: tag assertions, TxStatus 'T'→'E'→'T' recovery over the wire, 25P01/3B001 as pgconn.PgError, and **pgx nested transactions** (outer.Begin → inner Begin/Rollback/Commit ride on SAVEPOINT/ROLLBACK TO/RELEASE) — both rollback and release paths.
- Manual psql 17.5 scenario: ordinals, aborted-block recovery via rollback to savepoint, release-then-reference error, savepoint-outside-block error — all tags/messages matched PG.

## Versioning dance (cross-repo)

Dev loop: `go work use ../btypedb` (go.work is committed — temporary). Finish: commit btypedb → tag v0.4.0 → push → `go work edit -dropuse` → `GOPRIVATE=github.com/rohanthewiz/btypedb go get ...@v0.4.0 && go mod tidy` in **both** bytdb modules (pgwire/go.mod had its own v0.3.0 indirect pin — easy to miss) → full suites against the tag → commit + push. GOPRIVATE skips proxy/sumdb lag right after tagging; go.sum hashes are content-addressed so nothing to redo later.

## State at end of session

- Milestones 1–14 done. bytdb `88796f4`, btypedb `ca31f64` + tag `v0.4.0`, all pushed. go.work back to two modules.

## Next (deferred)

- Wire: cancellation, COPY, portal suspension.
- Engine: DESC key columns, CHECK/NOT NULL, EXPLAIN.
- SQL: GROUP BY expressions; expr items in aggregate queries; `= ANY(array)` for real.
- Someday: tag bytdb itself so pgwire's `replace` can become a versioned require.
