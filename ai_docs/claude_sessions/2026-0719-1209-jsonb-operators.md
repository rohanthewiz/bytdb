# Session: jsonb operator family

- Session ID: `ef09ed73-1ae7-4a0e-be34-01700590d1d9`
- Date: 2026-07-19
- Branch: `main` (started clean at `47dfdf9`, jsonb core already in)

## What was asked

Continue from the jsonb-core session: implement the most typical jsonb
operators. Delivered the full everyday set — accessors `-> ->> #> #>>`,
predicates `@> <@ ? ?| ?&`, concatenation `||`, deletion `-`.

## Design

Two operator kinds, split along the layer's existing seams:

- **Value operators** (`-> ->> #> #>> || -`) are `ExArith` ops parsed
  at the additive level (`sql/parser.go` `exprAdd`) — Postgres puts
  every "other" operator between `+ -` and the comparisons, and one
  shared level keeps `j->'a'->>'b'` and `j->'a' || j->'b'`
  left-associative. Evaluated by `jsonbArith`, dispatched from
  `evalEx`'s ExArith case (`sql/expr.go`).
- **Boolean operators** (`@> <@ ? ?| ?&`) are five new `PredOp`s
  (`OpContains`, `OpContainedBy`, `OpKeyExists`, `OpKeyExistsAny`,
  `OpKeyExistsAll`) riding the Pred machinery like the LIKE family:
  parsed via `predOpFor`, evaluated only in `checkPred`
  (`sql/exec.go` → `jsonbPredicate`), never pushed into scans (the
  planner only pushes EQ/ranges), not flippable (asymmetric — a
  literal-first spelling falls to Cond, preserving operand order).

**Shared-spelling dispatch** — the subtle piece. `||` and `-` mean
text-concat/arithmetic *or* jsonb merge/delete, and runtime values
cannot say which (jsonb and text are both Go strings). `staticJSONB`
(`sql/expr.go`) decides at eval time from expression shape: a jsonb
column (resolved through the environment chain, so correlated outer
columns count), a `::json(b)` cast, a `->`/`#>` chain, or a `||`/`-`
chain over one. Two untyped literals stay text/numeric — mirroring
Postgres's unknown-vs-unknown operator resolution. Verified untouched:
`'a' || 'b'` = `ab`, `7 - 2` = `5`.

## Semantics core (`sql/jsonb_ops.go`, new)

Parse canonical text → operate on Go shapes (`map[string]any`, `[]any`,
`string`, `json.Number`, `bool`, nil) → re-render canonically. Go's
Marshal sorts map keys lexically, the same order `CanonJSONB` emits, so
extracted sub-documents compare equal to independently stored copies.

- Accessors: text key on objects, int index on arrays (negative from
  the end); every miss — absent key, out of range, wrong container
  kind, non-numeric path element against an array — is NULL, never an
  error. `->>`/`#>>` unquote strings, JSON null → SQL NULL, containers
  render canonically.
- `@>`: recursive through object values; array elements match
  order-free with kinds never crossing (`[1,2,[1,3]]` contains
  `[[1,3]]` but not `[1,3]`); top-level scalar-in-array exception
  (`'[1,2]' @> '1'`). Numbers compare via `big.Rat`, so stored `1.0`
  contains `1` — even though `=` on jsonb columns stays canonical-text
  equality (documented asymmetry).
- `?` on objects (key), arrays (string element), string scalar
  (equality). `?|`/`?&` take `array['a','b']` or `'{a,b}'`; empty list:
  `?|` false, `?&` true.
- `||`: object∪object (right wins), else array concat with non-arrays
  wrapped as one-element arrays. `-`: text drops an object key / equal
  string elements; int drops by index (negative from end, out-of-range
  no-op); wrong-container deletes error in Postgres's words ("cannot
  delete from scalar", "cannot delete from object using integer
  index").

## Supporting edits

- `sql/lexer.go` `scanOp`: 3-char ops `->>` `#>>`; 2-char `->` `#>`
  `@>` `<@` `?|` `?&`; single `?`.
- `sql/coerce.go`: key-existence ops join the pattern family's
  static-coercion skip (a bare key would fail `CanonJSONB`);
  containment stays coercible — its operand IS jsonb, so malformed
  `@>` literals error before any row is read.
- `sql/expr.go` `exprType`: `->`/`#>` → TJSONB, `->>`/`#>>` → TString,
  `||`/`-` jsonb-aware; `castColType` now maps json/jsonb → TJSONB, so
  casts and accessor results describe as OID 3802 on the wire.
- `sql/explain.go` `opText`: `@> <@ ? ?| ?&` spellings.

## Tests / verification

- `sql/jsonb_ops_test.go` (new): accessor table (chains, negative and
  out-of-range indexes, NULL propagation, kind mismatches), predicate
  table (nested/numeric/array containment, both key-list spellings),
  concat/delete table (merge, wrap rules, no-ops, scalar/object delete
  errors), UPDATE-via-`||` round-trip, text `||` and numeric `-`
  regression rows.
- All suites green: root, sql, tuple, pgwire modules; gofmt + vet
  clean.
- `/verify` end-to-end: bytdbd on :5439, pgx v5.10.0 client, 25 checks
  over simple + extended protocols — accessors, RowDescription OID
  3802 for `->` results, all five predicates, `||`/`-`, `$n` params
  against `->>`/`@>`/`?`/`#>>`, UPDATE `SET body = body || $1`, clean
  errors that don't break the connection. Server restart on the same
  db: merged document still reads canonically and matches `@>`/`?`
  (REOPEN PASS).

## State / next steps

- 7 files modified + 2 new (`sql/jsonb_ops.go`, `sql/jsonb_ops_test.go`),
  committed with this doc.
- Not included, deliberately: `#-`, `jsonb_set`/`jsonb_build_*`
  functions, `j['a']` subscripting, `@>` on text[] columns, jsonb
  indexing (predicates evaluate as residual filters only).
- Remaining app-migration gap: ON DELETE CASCADE usage in app code.
- Memory file `btypedb-local-workspace.md` updated.
