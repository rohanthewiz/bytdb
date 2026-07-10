# Session: Milestone 24 — value window functions (LAG/LEAD/FIRST_VALUE/LAST_VALUE/NTH_VALUE)

- **Session ID**: `97a9f326-7a7d-457e-85ce-88cfc8dc0811`
- **Date**: 2026-07-10
- **Prior context**: post `96e8e94` main (README sync for milestones 22–23), user had just pushed everything. Asked what was left on window functions; answer: LAG/LEAD/FIRST_VALUE not implemented, explicit frames rejected, window+GROUP BY rejected. User picked LAG/LEAD/FIRST_VALUE as Milestone 24.
- **State at end**: implementation complete, all tests green in both modules, `gofmt -l` clean, verified end to end over the wire. Committing and pushing at session close (this doc's commit).

## What landed

All five value window functions, Postgres-faithful, as new per-row assignment
cases over the milestone-19 partition/sort machinery (`sql/window.go`):

- **LAG/LEAD** (`assignLagLead`): offset defaults to 1, evaluated per row (may
  be an expression), negative flips direction, NULL offset → NULL, partition
  edge → Default expression or NULL. Whole-partition addressing, frame
  ignored (as in PG). Overflow guard: offsets outside ±len(partition) are
  simply out of range, so `int64(n) - off` can't overflow.
- **FIRST_VALUE**: partition head for every row (default frame always starts
  UNBOUNDED PRECEDING).
- **LAST_VALUE/NTH_VALUE** (`assignFrameEnd`): peer-group walk implementing
  the PG default frame — frame ends at the current row's last ORDER BY peer;
  no ORDER BY → whole partition. LAST_VALUE = last peer (the famous PG
  surprise, deliberately kept). NTH_VALUE = frame's nth row or NULL if the
  frame is shorter; n evaluated per row; n < 1 errors; NULL n → NULL.

Supporting changes:

- `sql/ast.go`: WinLag/WinLead/WinFirstValue/WinLastValue/WinNthValue;
  `Offset`/`Default` fields on ExWindow (NTH_VALUE's n rides in Offset);
  `winArity` table (lag/lead 1–3, first/last_value 1, nth_value 2);
  walkExpr walks the new fields.
- `sql/parser.go`: value-family argument parsing with arity validation at
  parse; nested window calls rejected ("window function calls cannot be
  nested" via hasWindowExpr on args); `windowOver` now checks for the OVER
  keyword — previously `row_number()` without OVER gave a confusing
  downstream parse error, now "window function requires an OVER clause".
- `sql/window.go`: resultType switch (ranking → int, value family → Arg's
  type); windowText renders multi-arg calls for EXPLAIN; `evalOn` helper.
- `sql/params.go`: bindExpr binds Offset/Default (so `$n` works inside them).
- `sql/describe.go`: **the verification find** — select-list placeholders
  describe as untyped (present as text OID on the wire), which broke
  `lag(v, $1)` for pgx (it encodes bindings by the *server-described* type
  and failed client-side with "cannot find encode plan"). describeSelect now
  infers window Offset params as TInt and LAG/LEAD Default params as the
  value argument's type, walking select list + ORDER BY.

Tests: new `sql/window_value_test.go` — core lag/lead, offset variants
(explicit/negative/oversized/NULL/per-row expression), peer-frame semantics
for first/last/nth, describe types + param inference, EXPLAIN text, bound
params, and 9 error cases. All existing tests unaffected.

## Docs

- README: status → "Milestones 1–24" + window functions named; Milestone 24
  roadmap entry; LAG/LEAD/FIRST_VALUE removed from the "Later" bullet
  (frames + standalone CREATE SEQUENCE remain).
- `docs/features.md`: window section lists the value family, delta example
  (`v - lag(v, 1, 0) OVER ...`), LAG/LEAD semantics paragraph, pointer to the
  gotcha.
- `docs/gotchas.md`: unsupported-table row updated (frames + window+GROUP BY
  remain; value functions now noted as working); third "Postgres-faithful
  surprise" note added: LAST_VALUE returns the last *peer* under ORDER BY —
  fix here is dropping the ORDER BY (PG's usual fix, an explicit frame, is
  unsupported).

## Verification (wire-level)

psql is not installed on this machine. Verified by building
`pgwire/cmd/bytdbd`, serving a scratch db on 127.0.0.1:5439, and driving it
with a pgx v5 client (pgx is already in the module cache as a pgwire test
dep — copy `pgwire/go.sum` into the scratch module and `GOPROXY=off go get`
resolves offline). Confirmed deltas, peer-frame behavior with tied keys,
clean wire errors for all rejection paths, and both int and text `$n`
bindings inside window args (the latter failing before the describe.go fix,
passing after). Recipe persisted to `.claude/skills/verify/SKILL.md`.

## Gotchas / notes for next time

- Milestone numbering: 24 = value window functions. Next arc starts at 25.
- The generic select-list placeholder limitation remains: `select age + $1`
  still describes as text. Pre-existing, documented in `pgwire/values.go`;
  only window Offset/Default got targeted inference. Extend the same way if
  another select-list shape needs it.
- Error SQLSTATE for the new errors is the generic `XX000` (PG uses e.g.
  `42809`/`22013`) — cosmetic, consistent with existing behavior.
- Still open on the window arc: explicit frames (`ROWS/RANGE BETWEEN`),
  window + GROUP BY, `DISTINCT` in window calls. Frame support is the
  prerequisite for the "real" LAST_VALUE fix PG users expect.
- Still open elsewhere: standalone `CREATE SEQUENCE`, `pg_attrdef` rows
  (psql `\d` Default column), order-aware index path selection.
