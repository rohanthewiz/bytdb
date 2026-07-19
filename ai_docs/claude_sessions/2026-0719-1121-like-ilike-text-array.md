# LIKE/ILIKE Operators, text[] Type, and array_to_string

- Session ID: `bc9f93fc-3ed9-45e6-9d33-83bcee99b83b`
- Date: 2026-07-19
- Repo: `~/projs/go/bytdb` (main; follows the migration-gap-assessment doc from
  this same session, committed as 5d14914)

## What was built

Closed the two "accurate" gaps from the migration assessment: bytdb now has the
LIKE/ILIKE operator family, a first-class one-dimensional `text[]` column type,
and real `array_to_string`/`array_length` — verified end to end over the wire
with a live pgx client (11/11 checks) plus the full test suite green.

## Design

### text[] — canonical-literal string representation

`TTextArray` ("text_array") rides on a **string** runtime representation: the
canonical Postgres array literal (`{a,"b c",NULL}`), the same
ride-on-existing-kinds pattern timestamp (int64) and uuid ([]byte) use. No new
tuple encoding; the stored text IS the text wire form, so lib/pq round-trips
for free. Every write path canonicalizes (parse + re-render), which is what
makes `=` on array columns behave as element-wise equality despite the
underlying comparison being string comparison.

- `types.go`: `ParseTextArray` (strict: quoted elements with backslash escapes,
  unquoted trim + NULL keyword, rejects empty tokens/multidim/imbalance),
  `FormatTextArray` (array_out quoting rules: quote when empty, spells NULL, or
  contains `{ } , " \` or whitespace), `CanonTextArray` (fixed point).
- Elements are `string` or `nil` (NULL element). One dimension only.

### LIKE/ILIKE — regex-path reuse

Four new PredOps (`OpLike/OpNotLike/OpILike/OpNotILike`) parsed at the
comparison level in `exprCmp` (mirroring the `[NOT] IN` two-token lookahead).
`likeRegex` translates the pattern to an anchored `(?s)^...$` regex (rune-wise:
`%`→`.*`, `_`→`.`, backslash escapes, rest QuoteMeta) feeding the existing
`compileRegex` cache; ILIKE is the `(?i)` compile flag. Trailing backslash gets
Postgres's exact error. `ESCAPE '\'` accepted; any other escape char rejected
("only the default backslash ESCAPE character is supported") rather than
silently weakened. Pattern RHS composes (`'%' || $1 || '%'`).

## Files touched

Engine: `engine.go` (TTextArray const), `ddl.go` (validTypes), `dml.go`
(coerceValue: string → CanonTextArray, []string → FormatTextArray for embedded
Go callers), `types.go` (the parse/format pair).

SQL layer:
- `sql/ast.go` — four PredOps appended after the regex ops.
- `sql/parser.go` — `typeName` split into wrapper + `typeNameBase`; wrapper
  accepts a `[]` suffix, legal only on unbounded text/varchar (`int[]` → "only
  text[] arrays are supported", `varchar(5)[]` → dedicated error). `likeTail`
  helper + four exprCmp cases.
- `sql/exec.go` — `checkPred` pattern-op case extended: LIKE family converts
  via `likeRegex` then shares the regex match/negate path; NULL → unknown;
  non-text operands error "pattern match requires text operands".
- `sql/coerce.go` — `coerceLit` TTextArray case (canonicalize);
  `coerceBool` now SKIPS pattern ops entirely (a pattern is text no matter the
  column type — previously `ts_col ~ '...'` died in coercion with a misleading
  parse error; this improved the regex ops too).
- `sql/expr.go` — `likeRegex`; real `array_to_string` (2/3-arg Postgres NULL
  semantics) and `array_length` (dim 1 only; empty → NULL); `textArrayValue`
  canonical renderer now used by `ARRAY[...]` eval and `evalArraySub`
  (previously naive unquoted join); `castVal` handles `::text[]`/`::varchar[]`;
  `formatType` gained 1009→"text[]" AND the previously missing
  date/timestamp/uuid names (psql `\d` used to print `???` for those).
- `sql/syscat.go` — typeOID 1009, pg_type row `{1009,"_text",-1,"A"}`,
  sqlTypeName "ARRAY", udtName "_text".

pgwire: `pgwire/values.go` — `oidTextArray = 1009`; oidForType; encodeValue
(text passes stored literal through; binary via `encodeTextArrayBinary`) and
`decodeTextArrayBinary` for binary params. Binary format: ndim/hasnull/elemOID
header + (len,lbound) dim + int32-length-prefixed elements, -1 = NULL; empty
array is ndim=0 exactly as Postgres sends it; accepts elem OIDs 25/1043 only.
pgx requests binary for _text in the extended protocol, so this path is load-
bearing, not optional.

## Existing machinery leveraged (not rebuilt)

`= ANY('{...}')`/`= ANY(col)` already worked via `anyElements` +
`parseArrayLiteral` (lenient, kept separate from the strict root parser);
`ARRAY(SELECT ...)` existed for psql; ExIn/ExAny lowering untouched. LIKE
lowers to `Pred` leaves through the existing `lowerBool`, so it flows the same
filter path as the regex ops (non-flippable, non-indexable).

## Tests

- `sql/like_array_test.go` — LIKE/ILIKE semantics (%/_/escape/anchoring,
   3-valued NOT, param patterns, ESCAPE rejection, trailing-escape error,
  non-text operand error), text[] DDL/canonicalization/equality/ANY/
  array functions/casts/constructor/UPDATE, error cases (malformed, multidim,
  int[], varchar(n)[]).
- `textarray_test.go` (root) — literal parse/format round-trip table incl.
  escaped quotes/backslashes, NULL vs "null" vs `"null"`, fixed-point check;
  malformed corpus.
- `pgwire/textarray_test.go` — pgx binary []string round-trip, empty array,
  text-literal path, ILIKE-over-array_to_string through the wire.
- Full `go test ./...` green; `go vet` clean.

## End-to-end verification (/verify skill)

Scratch `bytdbd` on :5439 driven by a standalone pgx client (offline module via
copied go.sum, pgx v5.10.0): create table with text[] + bigserial; insert with
binary []string params + RETURNING; text-literal insert; []string scan-back;
`array_to_string(refs,' ') ILIKE '%'||$1||'%'` with bound param; LIKE
case-sensitivity vs ILIKE; NOT ILIKE; `= ANY(col)`; empty-array round-trip with
NULL array_length. 11/11 passed.

## Migration impact

The sermon search (`array_to_string(...) ILIKE`) now runs unmodified, and
`types.StringArray` columns round-trip without app-side re-encoding. Remaining
app-side gaps from the assessment: jsonb→text columns, the `DEFAULT now()`
sweep, 4 cascade deletes in app code, SQLBoiler SQL survivability.

## Notes

- Nothing pushed at doc-write time; commit follows this doc (session wrap).
- Deliberate scope cuts: no NULL-element support in lib/pq's StringArray
  (bytdb supports NULL elements; pq errors client-side), no `string_to_array`,
  no array subscripts (`ExIndex` still errors), no non-text arrays, ESCAPE
  limited to backslash.
