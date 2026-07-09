# Session: RETURNING clause on INSERT, UPDATE, and DELETE (stage 3)

- **Session ID**: `ffad5ac3-eef5-4e03-8778-94d967177de5`
- **Date**: 2026-07-09
- **Prior context**: loaded `2026-0708-1439-sequences-and-identity-columns.md`; stage 3 (`INSERT ... RETURNING`) was the agreed next step after named sequences (stage 1) and identity/SERIAL columns (stage 2).
- **State at end**: RETURNING implemented on all three DML statements, full suites green with `-race` (main module, sql, tuple, pgwire), verified via live `psql` against `bytdbd`, committed and pushed as `cae0b78`.

## The design spine: engine truth, not reconstruction

The one load-bearing decision: **RETURNING reports the row the engine stored, not a SQL-layer reconstruction.**

- Only the engine knows a drawn identity value (allocation happens inside `insertRow` → `fillIdentity`), and only the engine's `coerce` knows the final stored representation (e.g. an int assigned to a float column stores as `float64` — SQL-layer `coerceLit` does *not* do that conversion, it only adapts quoted string literals).
- So `insertRow` and `updateRow` (`dml.go`) now return the final row values; `Txn.InsertReturning` / `Txn.UpdateReturning` (`txn.go`) expose them, and the old `Txn.Insert`/`Txn.Update` became thin wrappers. Only 4 call sites existed; nothing else needed touching.
- DELETE needed **no engine change**: `execDelete` already materializes matching rows before deleting; `collectPKs` grew a `keep func([]any)` callback so RETURNING keeps the pre-delete rows (a plain DELETE passes a no-op).

## SQL layer

- **AST** (`ast.go`): `Returning{RetItems []SelectItem, RetStar bool}` embedded by value in `Insert`, `Update`, `Delete`. Two helpers: `hasReturning()`, and `retSelect()` which wraps the clause as a synthetic `&Select{Star, Items}` — the trick that let everything downstream be reuse.
- **Parser** (`parser.go`): one shared `returningClause(*Returning)` called at the end of `insert()`, `update()`, `deleteStmt()`. Items parse via `selectListItem()` (so expressions, aliases, `t.*` come free). Aggregates rejected both in the lowered legacy shape (`item.Agg != AggNone`) *and* buried in expressions (`findAgg(item.Ex)`); window functions via `hasWindowExpr`. The `returning` keyword was already reserved and already in `noAlias` from a prior session.
- **Executor** (`exec.go`): `prepareReturning(tx, table, desc, ret, res)` → `*retProj` (nil when no clause), built *inside* the write transaction so the descriptor matches what the writes run against. It calls `projectSelect(env.sc, r.retSelect(), res)` — full projection machinery reused verbatim. `retProj.row(vals)` mirrors execSelect's combined-row convention: expression results append past the table's width where projection ordinals expect them.
- **Params** (`params.go`): `noteReturningVals` + `bindReturning`, mirroring bindSelect's item handling — `$n` inside RETURNING counts and binds.
- **Describe** (`describe.go`): `describeReturning` fills `StmtInfo.Cols/Types` via the same `projectSelect` — this is what makes the extended protocol work. Placeholders *inside* RETURNING expressions get no type inference (stay `""`); acceptable, pgx sends text.

## pgwire: zero changes

Everything keys off existing plumbing: simple query path sends RowDescription when `res.Cols` non-empty; extended path uses `StmtInfo.Cols` at Describe; `sendCommandComplete` already renders `INSERT 0 N` from `RowsAffected` alongside rows. This was checked before writing any code — worth repeating on future features.

## Verification

- `sql/returning_test.go` (10 tests): serial draws + explicit-value echo, `*` and `t.*` expansion, expressions/aliases, UPDATE post-update rows + zero-match keeps Cols, engine coercion (int→float returns `float64`), DELETE pre-delete rows, `$n` in RETURNING, Describe shape, aggregate/window/unknown-column rejections (with atomicity: failed RETURNING rolls back the insert), in-transaction ROLLBACK returns the drawn value to the counter.
- `pgwire/returning_test.go`: pgx `QueryRow(INSERT ... RETURNING id)` (the ORM idiom), multi-row streaming with `CommandTag() == "INSERT 0 2"`, UPDATE/DELETE round trips, `pgx.ErrNoRows` on no-match.
- Live `psql` (postgresql@16 at `/opt/homebrew/opt/postgresql@16/bin/psql` — bare `psql` is not on PATH): all four statement shapes byte-for-byte Postgres, tags included.

## Docs

- `docs/features.md`: new "RETURNING" section after Auto-increment. The `sql/parser.go:574-606` typeName line ref survived (edits were all below 574) — keep watching this on parser changes.
- `docs/gotchas.md`: removed `RETURNING` from the not-supported table.
- README + `sql` package doc updated.

## Gotchas / notes for next time

- `gofmt -l` flags `bench/main.go` — pre-existing, not from this session; left alone.
- The checks path in `execUpdate` still builds its own `nv` row via `coerceLit` for CHECK evaluation; it does not use the engine-returned row (checks run *before* the write). Fine today, but if CHECK-vs-stored drift ever matters, the int→float case is where it hides.
- Failed statement with RETURNING burns identity values (gap, never repeat) — same rule as stage 2.
- Candidate next steps: `lastval()`/`currval()` (some drivers probe them), stage 4 `CREATE SEQUENCE` if a real client demands it, or `ON CONFLICT`/upsert which is the other thing ORMs send.
