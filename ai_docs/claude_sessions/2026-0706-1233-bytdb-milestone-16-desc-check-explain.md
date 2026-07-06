# Session: bytdb-milestone-16-desc-check-explain

- **Session ID**: `0a5cbac6-b280-4ff2-9d99-ad1cb0074a5c`
- **Date**: 2026-07-06
- **Repo**: `~/projs/go/bytdb` (btypedb untouched)
- **Result**: milestone 16 complete — DESC index columns, NOT NULL/CHECK constraints, EXPLAIN. Commit `25e5a0b`, pushed. All suites green under `-race` in both modules; verified against psql 17.5 (client driving bytdbd — no local PG server exists on this machine, only libpq tools).

## DESC index columns

CRDB's trick, kept entirely inside the tuple package: a descending element is its **ascending encoding inverted byte-for-byte**. Element encodings are prefix-free (fixed width, or escape-terminated with `00 01`), so whole-element inversion exactly reverses byte order and stays self-delimiting — ascending and descending elements mix freely in one key.

- `tuple.AppendDesc` / `tuple.DecodeOne(data, desc)`: implemented by threading a `mask` byte (0x00/0xFF) through `appendValue`/`appendEscaped`/`decodeOne`/`unescape` — escaping decisions on logical bytes, stored bytes XOR mask. Descending NULL (tag 0x01 → 0xFE) sorts **after** every value, the mirror of ascending; documented and tested.
- Engine: `IndexDesc.Desc []bool` (json `desc,omitempty`, nil = all asc) + `DescAt(i)`; `IndexCol{Name, Desc}` + `CreateIndexCols` (old `CreateIndex(cols ...string)` delegates). `indexEntry`/`indexBound` encode indexed values per-column direction via `appendDirected`; **the pk suffix and unique-form value stay ascending**. `rowFromIndexEntry` now walks the key element-by-element with `DecodeOne` (header asc, indexed cols by direction, remainder = pk-in-key vs unique-form-by-emptiness) instead of whole-key `tuple.Decode`.
- Planner (`planScan`): equality prefixes and `pathScore` are direction-agnostic (an eq region is contiguous either way; `indexBound` encodes bounds in key direction, so `p.from` stays semantic values). Range on a descending column **swaps roles**: the `hi` predicate (`col < / <=`) seeds `p.from` (inclusive; OpLT's equals discarded by residual filter — mirror of the asc OpGT comment) and the `lo` predicate becomes a stop — two new kinds `stopLE`/`stopLT` in `stopped()`. NULL hit on a desc-range stop → stop (NULLs sort last there); asc behavior unchanged.
- Parser: `(col [ASC|DESC], ...)`; `NULLS FIRST/LAST` parses to a clear rejection (our NULL placement is fixed by the encoding: asc-first / desc-last — note this is the opposite of PG's defaults, pre-existing for asc). `pg_get_indexdef` renders ` DESC` in the full form only (the per-column `colNo` form returns the bare name, as PG does).

## NOT NULL / CHECK

Split by who owns what: **NOT NULL lives in the engine** (`Column.NotNull`, enforced in `coerceRow` and `updateRow` with PG's exact message `null value in column "x" of relation "t" violates not-null constraint`); **CHECK lives in the descriptor as opaque expression text** (`TableDesc.Checks []CheckDesc{Name, Expr}`) and is enforced by the SQL layer, which owns the expression language — engine-API-only writers bypass checks, documented on `CheckDesc`.

- `CreateTableWithChecks` (CreateTable delegates); `AddColumn` accepts NOT NULL **only while the table is empty** — `hasRows` checked inside the same kv transaction that writes the descriptor (`writeDescIn` split out of `writeDesc`), message `column "c" of relation "t" contains null values`.
- Parser: column constraints loop (PRIMARY KEY, NOT NULL, NULL, `[CONSTRAINT name] CHECK (expr)`; DEFAULT/UNIQUE/REFERENCES rejected with pointed errors), table-level `[CONSTRAINT name] CHECK` and `CONSTRAINT name PRIMARY KEY` (name dropped). CHECK's source text is captured verbatim by slicing `parser.src` between the parens' token positions (parser gained `src`; `parseCheckExpr` re-parses stored text at write time).
- `sql/check.go`: `resolveChecks` validates at CREATE TABLE against a synthetic descriptor scope — no aggregates ("aggregate functions are not allowed in check constraints"), no subqueries, no placeholders, columns must exist — and names by PG convention (`t_col_check` / `t_check`, numeric-suffix dedup). `tableChecks` + `checkRow` evaluate per row on INSERT (skipped on arity mismatch so the engine's error wins) and UPDATE; **only triFalse rejects — NULL passes**, per SQL. `execUpdate` now collects full rows (not just pks), clones + applies SET, checks, then updates. `checkDropColumn` blocks dropping a referenced column (`because other objects depend on it`) — necessary because there is no ALTER TABLE DROP CONSTRAINT yet.
- Catalog: `attnotnull` and `is_nullable` include NotNull; `pg_class.relchecks` = len(Checks) (**psql only runs its check-constraint query when relchecks > 0**); `pg_constraint` now has real rows for checks (`checkOID = tableID*1000 + 900 + i`, above any realistic index ID in the same block); `pg_get_constraintdef(oid)` renders `CHECK ((text))`. psql's `\d` shows the full "Check constraints:" section.
- pgwire sqlstate: `violates not-null constraint` / `primary key column may not be NULL` / `contains null values` → 23502; `violates check constraint` → 23514.

## EXPLAIN

`sql/explain.go`: renders the plan **the executor actually runs**, as PG-shaped indented text (one `QUERY PLAN` text column; indent math: child arrow at `level*6-4`, details at `level*6+2`). No costs — bytdb has no cost model and inventing numbers would be worse than omitting them. `EXPLAIN ANALYZE` is **rejected** rather than faked (no instrumentation); `(FORMAT TEXT | COSTS | VERBOSE | ...)` options parse and are ignored, non-TEXT formats rejected.

- Node shapes: `Point Get on t` + `Key:`, `Index Scan using idx on t` + `Index Cond:`/`Filter:` (bounded primary scans show `using t_pkey`), `Seq Scan`, `Nested Loop [Left Join]` + `Join Filter:`, `Aggregate`/`HashAggregate` + `Group Key:` + HAVING as `Filter:`, `Sort` + `Sort Key: expr [DESC]`, `Limit`, `Unique`→`Append` for UNION, `Result` for FROM-less, `Insert on t` → `Values Scan on "*VALUES*"`, `Update/Delete on t` → scan child. Scalar subqueries render inline as `(subquery)`.
- Key mechanism: `plan` gained `pushed []*Pred` (`eq` map became `map[int]*Pred`), recorded wherever bounds/stops/gets are built — that is the Index Cond/Filter split, by pointer identity against `boolParts` (AND-chain flattening incl. Cond/Or/Not parts, unlike `conjuncts`). Join steps **replay** `runJoin`'s expr construction: template predicates synthesize with a placeholder value of the *source column's* type (so `litFits` pushes exactly when the real per-row value would), and an `explainer.tmpl map[*Pred]string` overrides their rendering to `(o.user_id = u.id)`. `predTmpl` gained `pr *Pred` for the claimed-set; WHERE parts no step claimed render as `Filter:` on the top node (which is where they actually run) — LEFT-JOIN right-table WHERE predicates land there correctly.
- Validation matches execution: `resolveAgg`/`projectSelect` run during explain so bad queries error identically. `exprText`/`litText`/`opText` render expressions PG-style (`!=` → `<>`, strings quoted, `~` regex ops).
- Plumbing: `Explain{Stmt}` statement; parser after all option forms; `numParams`/`bindParams`/`coerceLiterals` descend; `describe.go` refactored (`describeInto`) — EXPLAIN describes as one text `QUERY PLAN` column with the inner statement's params; command tag `EXPLAIN` (pgwire's default branch prints it bare).

## Gotchas worth remembering

- The desc unique-index test initially asserted a "dupe" row existed after its insert **failed** — the failed insert leaves nothing; expected rows must track it.
- `git add -A ':!.claude'` correctly keeps the local `.claude/` dir out of the repo.
- `pg_get_indexdef(oid, colNo)` must return the *bare* column name (no DESC) — psql uses the per-column form separately.
- INSERT check evaluation must skip wrong-arity rows so the engine's "wrong number of values" error surfaces instead of a misleading "column is not yet available here".

## State at end of session

- Milestones 1–16 done. bytdb `25e5a0b` pushed; btypedb still at `v0.4.0`.
- README status bumped to 1–16; Later list now: `count(distinct x)`, `= ANY(array)`, ALTER TABLE ADD/DROP CONSTRAINT.

## Next (deferred)

- Wire: cancellation, COPY, portal suspension.
- SQL: `count(distinct x)`; `= ANY(array)` for real; ALTER TABLE ADD/DROP CONSTRAINT (would let `checkDropColumn`'s block be escapable, as PG's CASCADE is); correlated subqueries referencing grouped columns in aggregate items.
- Someday: tag bytdb so pgwire's `replace` can become a versioned require; ORDER BY exploitation of index order (DESC indexes make `ORDER BY x DESC LIMIT n` a bounded scan).
