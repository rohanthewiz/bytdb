# Session: INSERT/UPDATE/DELETE ... RETURNING (stage 3)

- **Session ID**: `8d67e20a-6e80-4dd7-82ab-b16a07ce6f4e`
- **Date**: 2026-07-08
- **Prior context**: loaded `2026-0708-1439-sequences-and-identity-columns.md`; stage 3 (RETURNING) was the agreed next step after named sequences (stage 1) and identity/SERIAL columns (stage 2).
- **State at end**: RETURNING implemented across engine, sql, and wire; full suites green with `-race` in both modules; verified end-to-end via live `psql` against `bytdbd`. Stage 4 (`CREATE SEQUENCE`, if ever) is the only deferred piece.

## Design

- **The engine returns the stored row; the SQL layer projects it.** RETURNING must show the row *as stored* — identity columns filled, values coerced — which only the engine knows. So `insertRow`/`updateRow` return the final row internally, surfaced as `Txn.InsertReturning` / `Txn.UpdateReturning` (`Txn.Insert`/`Update` are now thin wrappers), plus an `Engine.InsertReturning` one-shot: the embedded-API way to read back a generated ID.
- **DELETE needed no engine change**: execDelete already materializes matching rows before deleting (writes under a live scan); with RETURNING it keeps the full rows instead of just PKs and projects each pre-delete image after a successful delete.
- **A RETURNING list is a select list** — resolved with SELECT's existing `projectSelect` machinery against a single-table scope (`d.tableEnv`), evaluated per affected row. Expressions, aliases, `*`, `t.*`, placeholders, even correlated subqueries work for free; aggregates and window functions are rejected at parse ("... are not allowed in RETURNING", as Postgres).
- **pgwire needed zero code changes.** Simple query already keys row-sending off `Result.Cols`; extended protocol off `StmtInfo.Cols` from `Describe`. Command tag stays `INSERT 0 n` with rows riding along — exactly Postgres's shape.
- `lastval()`/`currval()` from the stage-3 notes were **not needed** — drivers using RETURNING don't call them; left out deliberately.

## Changes

Engine (root module):
- `dml.go`: `insertRow` → `([]any, error)` (stored row); `updateRow` → `([]any, bool, error)` (post-update row). New `Engine.InsertReturning`.
- `txn.go`: `Txn.InsertReturning`, `Txn.UpdateReturning` (false + zero Row when the row doesn't exist); `Insert`/`Update` delegate to them.

SQL:
- `ast.go`: `Returning{Star, Items []SelectItem}`; `Ret *Returning` on Insert/Update/Delete (nil = no clause).
- `parser.go`: `returningClause()` called at the end of `insert()`, `update()`, `deleteStmt()`. "returning" was already reserved in `noAlias` (prepped last session). Rejects aggregates/window functions.
- `returning.go` (new): `resolveReturning` (projection + res.Cols/Types via `projectSelect`) and `retProj.row` (per-row expr eval + projection).
- `exec.go`: all three executors fill `res.Rows` when RETURNING present; **zero-match DML still announces Cols** (wire clients expect a RowDescription either way). RowsAffected unchanged.
- `params.go`: `noteReturningVals` (numParams) + `bindReturning` (bindParams) — placeholders bind inside RETURNING expressions and count toward arity.
- `describe.go`: `describeReturning` fills `StmtInfo.Cols/Types` — the piece that makes ORMs' Parse/Describe round work. Placeholders appearing only in RETURNING stay untyped → present as text on the wire.
- `coerceLiterals` needed no change: `c := *s` clones carry `Ret` along, and select-list items don't literal-coerce anyway.

Docs: `sql/sql.go` package comment (statement list + RETURNING paragraph), README statement list, `docs/features.md` new "RETURNING" section, removed RETURNING from `docs/gotchas.md`'s not-supported table.

## Tests

- `returning_test.go` (engine): stored row from one-shot and Txn (identity drawn, int→int64 coercion), failed insert returns no row; UpdateReturning post-update image and missing-row case.
- `sql/returning_test.go`: multi-row insert ids in order, `RETURNING *` with NULL-identity fill, `t.*` + expressions + aliases, update/delete images, zero-match shape, placeholders in RETURNING (+ prepared re-bind), Describe shape (and row-less without RETURNING), rejections (aggregate/window/unknown column/empty list), in-block RETURNING with rollback (ids burn, data gone).
- `pgwire/returning_test.go` (pgx): QueryRow insert-returning (extended protocol: Describe before any row exists), multi-row with tag `INSERT 0 2`, update/delete round-trip, empty → `pgx.ErrNoRows`, simple-protocol mode. Note pgx API: exec-mode constant goes in args (`c.QueryRow(ctx, q, pgx.QueryExecModeSimpleProtocol)`), not as the sql param.

## Verification

- `go test -race ./...` green: main module, sql, tuple, pgwire.
- Live: `bytdbd` on :5599, real `psql`: `INSERT ... RETURNING id, name` returned drawn ids 1,2 with tag `INSERT 0 2`; explicit id 10 via `RETURNING *`; `UPDATE ... RETURNING id, name, upper(name) AS shout`; `DELETE ... RETURNING *`; no-match DELETE printed `(0 rows)` + `DELETE 0`.

## Gotchas / notes for next time

- `docs/features.md`'s `sql/parser.go:574-606` reference survived — this session's parser edits (insert/update/delete tails, ~770+) are all after `typeName`. Keep watching line refs on parser changes.
- `bench/main.go` fails `gofmt -l` — pre-existing, not touched.
- projectSelect's "SELECT * requires a FROM clause" error can't fire from RETURNING (the DML scope always has its one table).
- Result contract: DML with RETURNING fills Cols/Types/Rows *and* RowsAffected; pgwire sends rows whenever Cols is non-empty and tags from the command word — that combination is what makes RETURNING free at the wire layer.
