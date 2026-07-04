# Session: bytdb-milestone-9-prepared-statements-and-errtext

- **Session ID**: `2e091699-bf92-4f4a-9991-cde6c0f92b19`
- **Date**: 2026-07-04
- **Continues**: milestone 7 doc (`2026-0703-1249-bytdb-milestone-7-boolean-expressions.md`); milestone 8 (joins, `41c8602`) happened between sessions and had no doc — its README updates were folded into this session.
- **Repo**: `~/projs/go/bytdb` (github.com/rohanthewiz/bytdb)
- **Result**: milestone 9 complete — `131577d`: prepared statements ($1 parameters, Prepare/Stmt, variadic Exec). Follow-up `803e4df`: `bytdb.ErrText` client-facing error rendering over serr v1.4.0's new `UserFields()` (serr `59fcf40`, tagged). All green under `-race`; verified as an external consumer.

## Milestone 9 (`131577d`)

- **AST**: `Param int` marker type (1-based ordinal). `parser.literal()` turns a `tParam` token into `Param(n)` — so placeholders work everywhere a literal parses: Pred.Val in WHERE/ON/HAVING (flip normalization included: `$1 < col` → `col > $1`), INSERT rows, UPDATE SET. `$0` and `-$1` are parse errors; LIMIT/OFFSET stay literal-only (`nonNegInt` gives a specific "placeholders are not supported in LIMIT" error).
- **API** (sql.go): `Exec(query string, args ...any)` now variadic; `Prepare(query) (*Stmt, error)`; `Stmt.Exec(args...)` and `Stmt.NumParams()`. Both funnel into `run(st, args)` → `bindParams` → existing dispatch switch.
- **Binding** (new sql/params.go): `numParams(st)` = highest $n (walks Insert.Rows, Update.Set, and Pred leaves via `walkPreds` over Where/Having/every FromItem.On). `bindParams` requires `len(args) == numParams` exactly (repeats of the same $n allowed, gaps allowed), normalizes args via `bindArg` (int/int8..uint64→int64 with overflow checks, float32→float64; int64/float64/string/bool/[]byte/nil pass; anything else errors), then substitutes into a **copy** of the value-bearing parts (`bindBool` clones the BoolExpr trees; rows/Set maps copied). Zero-param statements pass through unchanged.
- **Why copy-then-bind works**: execution never mutates the parsed AST — verified in join.go before designing (per-outer-row index nested-loop preds are built as fresh `*Pred`s at join.go:261; `binds`/`havingRefs` maps are per-execution, keyed by pointer). So a `Stmt` holds the parsed tree and is safe for concurrent use.
- **Bind before plan**: binding runs before `planScan`, so bound values push down to point gets / bounded scans exactly like literals (`litFits` sees the concrete Go type). An unbound `Param` fails `litFits` and would compare as unknown — a safe fallback that the arity check makes unreachable.
- **nil arg = SQL NULL**: pushdown skips it (`pr.Val == nil` guard at plan.go:90), residual compare → unknown → no match. Postgres-consistent.

## ErrText follow-up (`803e4df` + serr `59fcf40` = v1.4.0)

- Session-long finding closed: serr `%v` printed only the message; want/got/pos fields were invisible to clients.
- **serr v1.4.0** (`~/goprojs/serr`): `UserFields()` on SErr — ordered caller-supplied key/val pairs, filtering the five reserved keys serr itself writes: `location`, `function`, `msg` (wrap messages), and `UserMsgKey`/`UserMsgSeverityKey` (SetUserMsg stores into the same fields list — easy to miss). Reserved keys are now exported constants (`LocationKey`, `FunctionKey`, `MsgKey`) used by the writers, so filter and writers can't drift. Built on the ordered `se.fields` slice, not the `FieldsMap*` maps (map-based `FieldsAsString`/`String()` are nondeterministic — their own test accepts two orderings).
- **Caveat**: the v1.4.0 tag also released the previously-unreleased pointer-receiver commit `5ece3e4` sitting on master. User was told; retag from a cherry-picked branch if that's a problem.
- **bytdb**: `ErrText(err)` in the **root** package (err.go) so both engine and sql consumers can use it (sql imports bytdb; no cycle): `message (k: v, k: v)` from `UserFields`, passthrough for plain/bare/nil errors. E2E test: `Exec` with missing arg renders `wrong number of parameters (want: 1, got: 0)`. go.mod bumped serr v1.3.0 → v1.4.0.
- Installed skill copy `~/.claude/skills/serr-structured-error-wrapper/SKILL.md` synced with the new UserFields section (mirrors repo's `ai_docs/SKILL.md`).

## Testing / verification

- Parser: `TestParseParams` — params in WHERE (incl. flipped), INSERT, SET, HAVING; errors for `$0`, `-$1`, LIMIT/OFFSET params. Old "$1 not supported" entry removed from `TestParseErrors`.
- Plan: `TestPlanBoundParamPushdown` — unbound Param pushes nothing; after `bindParams` it's a point get; the parsed tree still holds `Param(1)` (no mutation).
- E2E: `TestSQLParams` (int coercion, repeated $1, params in INSERT/UPDATE/DELETE, nil-param-never-matches, params in join ON and HAVING, arity/type errors, ErrText rendering), `TestSQLPrepare` (re-execution with different args, third run matches first — bindings don't stick; prepared INSERT reused; NumParams).
- Consumer demo (scratchpad module, `replace` → local repo): full flow incl. reopen persistence — prepared inserts, re-executed prepared selects, NULL params, arity error.
- README brought current: was stale from milestone 8 (still listed joins as deferred) — added joins + prepared statements + ErrText sections, roadmap checkboxes for 8 and 9.

## State at end of session

- bytdb main = `803e4df`, pushed. Milestones 1–9 done. serr master = `59fcf40` = v1.4.0, pushed. Deps: btypedb v0.3.0, serr v1.4.0, Go 1.26; sql package still zero new deps.

## Next (deferred list, in order)

- **`bytdb-pgwire` module** (separate go.mod): `Result{Cols, Types, Rows}` is the RowDescription/DataRow shape; `Stmt` maps onto Parse/Bind/Execute; errors should consume `UserFields()` structurally, NOT `ErrText` — `pos` → ErrorResponse Position (convert byte offset → 1-based char offset), message → Message, rest → Detail. Autocommit-only first; `pg_catalog` is the ORM long tail.
- Engine: DESC key columns (byte inversion), CHECK/NOT NULL, savepoints, EXPLAIN (plan struct is ready to print: get/index/from/stops/filter).
