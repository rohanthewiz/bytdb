# Session: ON CONFLICT upsert, constant DEFAULTs, lastval()/currval()

- **Session ID**: `8d67e20a-6e80-4dd7-82ab-b16a07ce6f4e` (continuation of the reconciliation session, same day)
- **Date**: 2026-07-09
- **Prior context**: post-reconciliation main (`92463db`); asked "what's next", agreed on the write-path ORM-enablement list, executed all three with a commit per task.
- **State at end**: three features committed (`a0fd8db` upsert, `97f7ece` defaults, `1d24572` lastval/currval), full suites green with `-race` both modules, all three verified interacting in one live `psql` session. Pushed together with this doc.

## Task 1 — INSERT ... ON CONFLICT (`a0fd8db`)

**Zero new engine machinery.** Conflicts are found by probing: PK via `tx.Get`, unique index via `ScanIndex` from the exact key + compare-first-row (`sql/upsert.go: probe`). DO UPDATE rides `Txn.UpdateReturning`.

- Arbiter inference: target column set matched set-wise (order-insensitive) against PK cols, then unique indexes; no target (DO NOTHING only, parser-enforced) = PK + every unique index. Postgres wording on no match.
- **Name resolution trick**: DO UPDATE SET/WHERE evaluate over a two-table scope `[target, excluded]` (same descriptor, excluded at offset w). Bare column refs would be ambiguous, but Postgres reads them as the target — so the parser qualifies bare refs with the target table name at parse time (`qualifyExprCols`/`qualifyBoolCols`). Crucially **walkExpr treats `*ExSub` as a leaf**, so subqueries keep their own resolution and correlation still works through the env chain.
- excluded = the proposed row post-literal-coercion, pre-identity (a NULL identity stays NULL in excluded — the draw never happened; documented, nobody SETs from it).
- Cardinality rule: `touched` set of tuple-encoded PKs (inserted-by-statement + conflict-updated; a PK-moving SET marks both keys) → "cannot affect row a second time".
- CHECKs on the update path mirror execUpdate (coerced literal setVals + evaluated SetEx into a candidate row, single-table checkEnv).
- Counts: only real inserts + updates (`INSERT 0 1` after a DO NOTHING skip); WHERE-filtered pairs neither count nor RETURN.
- Parser: `setClause()` factored out of `update()` and shared. `ON CONSTRAINT` and index predicates (`ON CONFLICT (c) WHERE ...`) rejected pointedly.
- Params/coercion/describe all extended: Conflict.Set typed by column, Where via the two-table scope in both coerceLiterals and describe.

## Task 2 — constant DEFAULT column values (`97f7ece`)

- Stored as **SQL literal text** on `bytdb.Column.Default` (the checks precedent: engine stores/reports, SQL layer applies). `renderLit`/`parseStoredLiteral` round-trip; DDL-time `coerceLit` validates against the column type ('5' adapts into int).
- Applied in execInsert: column-list path starts `vals = slices.Clone(defaults)` then overlays named values; `DEFAULT` keyword in VALUES = `defaultMarker{}` resolved per row (**clone before replacing** — full-arity rows alias the parsed AST; prepared statements would corrupt); `INSERT ... DEFAULT VALUES` parses as empty column list + one empty row.
  - **Gotcha found by test**: the fill path must key off `s.Cols != nil`, not `ords != nil` — an empty column list (DEFAULT VALUES) leaves ords nil.
- Defaults land before CHECK evaluation and upsert probing, so both see stored-truth values (excluded carries defaults).
- `DEFAULT NULL` = no default (as PG). Rejections: `now()`/`CURRENT_TIMESTAMP` et al with pointer to epoch ints, non-constant expressions, placeholders, identity+DEFAULT conflict (both orderings).
- `ADD COLUMN ... DEFAULT` empty-table only (engine-enforced, like NOT NULL): PG backfills, we don't.
- `information_schema.columns.column_default` reports the stored text.
- Pre-existing test `TestSQLCheckValidation` asserted "DEFAULT is not supported" — updated to assert the `now()` rejection.

## Task 3 — lastval()/currval() (`1d24572`)

- `seqSession` (mutex-guarded: bare DB may Exec concurrently) holding last draw + per-name map. **Plumbing**: `DB.seq` pointer; `New()` creates one (bare DB = one shared session); `NewSession()` clones the DB struct with a fresh seqSession (previously Session shared the DB outside blocks!); the in-block clone at session.go threads the same pointer.
- execInsert records draws: identity column AND pre-insert `vals[i] == nil` (explicit ids are not draws), under `table_col_seq` — the same name information_schema shows in column_default.
- Eval: `evalFunc` cases; **`funcTypes` map needed entries too** — without them the wire declared the column as text (OID 25) and pgx refused to scan int64 (caught by the wire test).
- Errors: PG's exact wording, mapped to SQLSTATE 55000 in pgwire/errors.go. currval on a never-drawn name gives the 55000 wording whether or not the sequence exists (sequences are implied, not cataloged — documented divergence from PG's 42P01).
- Draws survive rolled-back blocks (session-local state; the counter itself rewinds but the readback stands — matches PG observable behavior).

## Verification

- `go test -race ./...` green in both modules after each task; committed per task as requested.
- Live `psql` finale exercising all three interacting: defaults + serial in `RETURNING *`, upsert version-bump via `excluded`, `DO NOTHING` → `INSERT 0 0`, `lastval()/currval()` readback, and DEFAULT VALUES correctly rejected on a defaultless TEXT PK.

## Gotchas / notes for next time

- `sysFuncCall` folds zero-arg functions at parse — lastval() must NOT go in `sysFuncs`; it's session-dynamic and lives in `evalFunc` + `funcTypes`.
- When adding a function that returns non-text, remember **both** `evalFunc` and `funcTypes` — the wire type comes from the latter.
- `noAlias`/`itemEnd` keyword maps: nothing needed this time ("on", "default", "where" already fine in context), but check on future clause additions.
- Postgres semantics worth remembering: bare names in DO UPDATE = target row (not ambiguous); DO NOTHING skips don't count in the tag; lastval survives rollback.
- Remaining from the arc: stage 4 `CREATE SEQUENCE` (no demand yet); pg_attrdef still empty so psql `\d` Default column stays blank.
