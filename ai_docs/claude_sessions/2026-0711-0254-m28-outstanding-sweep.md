# Session: Milestone 28 — the outstanding-list sweep

- **Session ID**: `4324b175-e93e-4101-9058-ef64ae5ac674`
- **Date**: 2026-07-11
- **Prior context**: continued from the m27 session doc (frame EXCLUDE)
  on post-`7e8e44b` main. User said "continue with milestone 28: window
  + GROUP BY, DISTINCT in window calls, standalone CREATE SEQUENCE,
  pg_attrdef rows, and order-aware index selection" — the entire
  outstanding list from m27, as one milestone.
- **State at end**: all five features implemented, tests green in both
  modules, `go vet`/`gofmt` clean, verified end to end over the wire
  (19 checks, simple + extended protocols). Committed and pushed at
  session close.

## 1. Window + GROUP BY

Windows now evaluate after grouping and HAVING (Postgres' order), so
`RANK() OVER (ORDER BY SUM(sal) DESC)` ranks whole groups.

- The bridge is `aggQuery.rowMode` (sql/agg.go): when set, `rewrite`
  emits `exOrd{ord}` positional reads instead of `exGroupRef`/
  `exAccRef` — group keys at ords `0..len(keys)`, accumulator results
  after. Each HAVING-surviving group materializes as a synthetic row
  (keyVals ++ acc values), and the *unchanged* m19 window machinery
  runs over those rows (frames, EXCLUDE, value functions, all of it).
- `exOrd` is a new expr node (sql/expr.go): reads `env.row[ord]`
  directly, carries its type. Never comes from the parser.
- New `execSelectAggWindow` (sql/window.go): rewriteWindows → resolve
  agg in row mode → `q.rewriteWindow` maps each window's Arg/Offset/
  Default/Partition/OrderBy through `q.rewrite` (raw columns get the
  classic "must appear in the GROUP BY clause" error — PG's wording
  for window args too) → shared `aggScanGroups` (factored out of
  execSelectAgg) → materialize + HAVING row-wise → computeWindow →
  sort/offset/limit/project with `env.win` set. Dispatch in
  runSelectCore (sql/exec.go:1137) replaced the old "not supported"
  rejection.
- Parser: the "aggregate calls cannot be nested" check moved to after
  OVER is ruled out, because `SUM(SUM(x)) OVER (...)` is legal (inner
  = group agg, outer = window agg). Aggregate-window args reject
  nested *windows*; `accumFor` rejects windows inside aggregate args
  ("aggregate function calls cannot contain window function calls").
- `exprType`'s ExAgg case now handles expression arguments (mirrors
  accumType: SUM over non-int → float, MIN/MAX → arg type) so
  `sum(sum(x)) over ()` types correctly.
- EXPLAIN: `windowNode` factored out; agg queries with windows render
  WindowAgg **above** HashAggregate.
- resolveAgg gained a rowMode param — callers in describe.go:259 and
  explain.go:182 pass false.

## 2. DISTINCT in window aggregates (bytdb extension)

`COUNT(DISTINCT x) OVER (...)` dedups within each row's frame. PG
rejects this ("DISTINCT is not implemented for window functions");
DuckDB and bytdb support it — documented as a deliberate divergence.

- `ExWindow.Distinct` bool; parser accepts DISTINCT before OVER
  (removed the old rejection at parser.go~2080); `windowText` renders
  it; `assignAggFrame`'s newAcc allocates the `seen` map (the accum
  dedup infrastructure from m17 did the rest). The anchored
  UNBOUNDED-PRECEDING fast path reuses one accumulator whose seen set
  grows with the frame — correct because that frame only grows.

## 3. Standalone sequences

- **Engine** (new `seqobj.go`): first-class sequence objects — one
  JSON `SeqDesc` per sequence holding options (Type/Start/Increment/
  Min/Max/Cycle/Cache) AND state (Last/Called), keyed at
  `(sysSeqTableID, sqlSeqIndexID=3, name)` — separate from named
  counters (1) and identity counters (2). IDs come from the table-ID
  counter so pg_class oids never collide. API: Create/Drop/
  AlterSequence, Engine/Txn `Sequence`, `Sequences`, `Txn.NextVal`
  (cycle/bounds with PG wording, int64-wraparound-safe), `Txn.SetVal`.
  Sequences share the relation namespace: CreateSequence checks
  descKey, CreateTable now checks sqlSeqKey (ddl.go).
- **SQL** (new `sql/sequence.go` + parser/ast): CREATE SEQUENCE
  [IF NOT EXISTS] with AS smallint|integer|bigint, INCREMENT [BY]
  (negative descends: min=typeMin, max=-1, start=max — PG defaults),
  MIN/MAXVALUE, NO MIN/MAXVALUE, START [WITH], CACHE n (stored/
  reported, behaves as 1), CYCLE/NO CYCLE, OWNED BY NONE (column
  ownership rejected); ALTER SEQUENCE (options + RESTART [[WITH] n]);
  DROP SEQUENCE [IF EXISTS] [RESTRICT] (CASCADE rejected). IF [NOT]
  EXISTS produce Result.Notice skip messages.
- **nextval/setval** (sql/expr.go): evaluate via env.tx; accept a name
  or an oid (`'s'::regclass` — the regclass cast itself now resolves
  sequence names too); record into the session seqSession so lastval/
  currval work (setval repositions currval, as in PG). funcTypes: both
  TInt.
- **Write-txn routing** (sql/sql.go run, case *Select): a SELECT whose
  expression tree calls nextval/setval (checked by
  `selectWritesSequences` — items, ORDER BY, GROUP BY, WHERE/HAVING,
  join ONs, subqueries, UNION arms) runs inside `e.WriteTxn` via a DB
  copy with `dw.tx` set. Session blocks keep their own txn (READ ONLY
  blocks then fail the write — correct).
- **Catalog** (sql/syscat.go): pg_class relkind 'S' rows (relam 0);
  new pg_catalog.pg_sequence and information_schema.sequences
  (numeric fields as text, per the standard); `d.lookup` serves each
  sequence as a one-row virtual state relation (last_value, log_cnt,
  is_called) so `SELECT last_value FROM s` and psql `\d s` probes
  work. isDDL/writeTarget/command() cover the three new statements.

## 4. pg_attrdef rows

- pg_attrdef gained a rows func: one row per declared column default,
  adbin = the stored literal text, oid = `tableID*1000 + 800 + attnum`
  (new attrdefOID slice of the table's oid block, below checks at
  +900). pg_attribute now computes `atthasdef` (c.Default != "") and
  `attidentity` ('d' for identity — PG's GENERATED BY DEFAULT), so
  psql's \d Default-column subquery (gated on atthasdef) renders.
- `pg_get_expr` un-stubbed: returns arg 0 when it's a string (adbin
  holds text, so decompiling is the identity), NULL otherwise — the
  other probe sites (indpred, prqual) still get NULL.
- Identity columns deliberately have NO pg_attrdef row (PG puts them
  in attidentity); information_schema.columns still synthesizes the
  serial-style column_default ORMs key off.

## 5. Order-aware path selection among redundant indexes

The m20 deferral: planScan broke score ties by creation order, so with
both `(a)` and `(a,b)` indexed, `WHERE a=1 ORDER BY b` picked `(a)`
and sorted.

- `planScanOrdered(desc, alias, where, keys)` (sql/plan.go): on a
  score TIE (s == bestScore && s > 0), prefer the candidate whose path
  order serves the sort keys. Selectivity still outranks order
  (strictly better score wins regardless), and zero-score ties are
  untouched — swapping a full scan for an unbounded index walk stays
  the LIMIT-gated chooseOrderedPlan override's decision.
- `pathOrder(desc, idx, pinned, keys)` factored out of orderSatisfied
  (which now delegates): the order check over a bare path + pinned
  columns, usable before a plan is built.
- chooseOrderedPlan starts from planScanOrdered, so the tie-break
  works with or without LIMIT (both tied paths visit the same rows).

## Tests

- `sql/window_group_test.go`: TestWindowOverGroups (rank over sums,
  SUM(SUM) running, LAG across groups, PARTITION BY an aggregate,
  sliding frames over groups, FIRST_VALUE of sums),
  TestWindowOverGroupsShapes (HAVING-before-window rank 1..2, implicit
  single group, window only in ORDER BY, EXPLAIN node order),
  TestWindowOverGroupsErrors, TestWindowDistinct (running distinct
  count, sum/avg distinct, EXCLUDE composition, distinct-over-groups,
  EXPLAIN rendering).
- `sql/sequence_test.go`: basics (draws, readbacks, setval 2/3-arg,
  regclass, state relation, draw-then-insert), options (bounds,
  cycle, descending, type ranges, PG-worded errors), DDL (notices,
  namespace collisions both ways, ALTER restart/retarget, drop),
  catalog (pg_class/pg_sequence/info_schema/regclass), reopen
  persistence.
- `sql/attrdef_test.go`: pg_attrdef rows + the verbatim psql 17
  Default-column query; identity → attidentity 'd', no attrdef row.
- `sql/order_test.go`: TestOrderRedundantIndexTieBreak (tie → by_ab
  no-sort with/without LIMIT, DESC reverse, strictly-better score
  wins, unservable order keeps creation-order tie + sort).
- window_cover_test.go's DISTINCT rejection became two
  "window function calls cannot be nested" cases.

## Verification (wire-level)

Same recipe as m24–m27 (`.claude/skills/verify/SKILL.md`): built
`pgwire/cmd/bytdbd`, scratch db on 127.0.0.1:5439, pgx v5 client.
19 checks passed: rank-over-sum + HAVING + $n over the extended
protocol, DISTINCT windows, the full sequence lifecycle (nextval/
setval/currval/lastval/regclass/state relation/RESTART/pg_sequence/
info_schema + clean "does not exist" error after drop), the psql
Default-column query, and the tie-break plan (by_ab, no Sort) with
correct rows.

## Gotchas / notes for next time

- Milestone numbering: 28 = this five-feature sweep. Next arc starts
  at 29.
- **INSERT VALUES takes literals only** — `VALUES (nextval('s'))` does
  not parse. Documented in gotchas with the draw-then-insert pattern;
  "expression values in INSERT" is now first on the README Later list.
  Same for `nextval` in column DEFAULTs (constants only).
- Sequence allocation is **transactional** (PG burns rolled-back
  values; bytdb reuses them) — deliberate, documented divergence.
- `aggQuery.rowMode` is the seam for anything else that needs grouped
  rows as a relation (e.g. a future DISTINCT-in-SELECT or grouping
  sets): rewrite emits exOrd, groups materialize as rows.
- A correlated subquery inside a window over grouped rows evaluates
  against the synthetic row via the *base* scope — such references
  fail or misresolve rather than being rejected cleanly (pre-existing
  behavior class for agg subqueries; PG corner, untested).
- `d.lookup`'s sequence-as-table resolution reads committed state
  (`d.e.Sequence`), not the transaction snapshot — a same-block
  CREATE SEQUENCE + SELECT FROM it sees the committed view. DDL can't
  run in blocks anyway, so unreachable today.
- The tie-break only fires on *equal* scores; a pushed path never
  trades for a better-ordered lower-score path (deliberate — see the
  m20 gap list if that ever matters).
- Still open (README Later): expression values in INSERT, `SELECT
  DISTINCT`, sequence functions in DEFAULTs.
