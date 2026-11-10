# Deep-look plan: Phases 2 to 4 (typing, subqueries, statement atomicity)

Session: `2c5b1603-fa95-4c81-b037-0bcdc19533ac` (continues
`2026-1109-2243-deep-look-phase0-phase1`).

## What landed

- **D-19 follow-up, `TableShared`** (`27a11e4`). The deep copies D-19 added
  to `Engine.Table`/`Txn.Table` cost a point select +6% time and +13% bytes
  (benchstat, n=6). The SQL layer now reads through `TableShared`
  (documented read-only); back to the pre-D-19 numbers.
- **Phase 2, typed bind pass** (`1a42947`, `6733a5f`, `6fabe48`).
  `sql/typecheck.go` replaces `coerceLiterals`: every statement, subquery,
  CTE, view body, CHECK and USING expression is typed with the right scope;
  untyped literals take their context's type; date vs timestamp gets a cast;
  impossible pairings are 42883; casts carry `ExCast.From`; floats render as
  Postgres does (`bytdb.FormatFloat`, also on the wire); Describe gets
  context-typed parameters (untyped ones are OID 0); group/dedup/partition
  keys follow SQL equality (`hashKey`). `VACUUM (VERIFY)` and
  `DB.VerifyChecks` find rows a previously inert CHECK admitted.
- **Phase 3, subqueries** (`db4fcd2`). Shapes the reduced runners do not
  implement run through `runSelectBody` with the outer row as correlation
  scope (`DB.outer`); `IN (uncorrelated subquery)` probes a hash set
  (957 ms -> 72 ms).
- **Phase 4, statement atomicity** (`335c86e`, `e0363d8`, `95dfc88`). Two-pass
  UPDATE, RETURNING-first DELETE, end-of-statement FK checks, one snapshot
  for UNIONs and view-using DML, implicit transactions for multi-statement
  queries and pipelines.

## Decisions

- **Typing is strict only where both sides are definitely typed.** What the
  pass cannot resolve (an unknown table, an ORDER BY alias) is left to the
  executor. Integer vs date/timestamp and bytea vs uuid stay accepted on
  purpose: they are the runtime representations embedders bind (a pgx uuid
  arrives as 16 bytes).
- **The pass is copy-on-write.** `bindParams` returns the shared prepared
  tree when there are no parameters; mutating it would race. A test pins
  that execution never rewrites the prepared tree.
- **Simple subqueries keep the reduced runners.** Their per-outer-row plan
  cache is the fast path for correlated lookups; only the shapes they do not
  implement go to the executor.
- **D-32's implicit block opens lazily at the first write.** A read-only
  batch never takes the writer lock, and a single statement keeps plain
  autocommit (and its OCC retry). pgwire decides "more than one statement"
  for a pipeline by peeking only at already-buffered bytes.
- **Plan examples corrected by the corpus:** `integer || integer` is an error
  in Postgres too; a direct primary-key swap fails mid-statement in Postgres
  too (D-31's third example).
- **UPDATE ... RETURNING still evaluates after each write** (its values come
  from the engine). A RETURNING subquery over the updated table is the one
  D-29 shape left.

## Measurements

Apple M3, benchstat n=6 unless noted: prepared point select +4.4% for the
per-execution typing pass (until D-40 step 3); IN-subquery 957 ms -> 72 ms
(n=5); FuzzExec 10 minutes clean (18.2M executions).

## Next

Closed: N-029, N-037, N-038, N-039. Declined: None. Raised: None.
Deferred: None. Promoted: None. Moved: None. Updated: N-042, N-044.
Full list: `ai_docs/todo/next-list.md`.
