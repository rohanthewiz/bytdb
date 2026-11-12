# Plan: an exact `numeric` / `decimal` type (N-048)

**Status (2026-11-11): all six phases implemented**, decisions Q1–Q10 as
written. Difftest's `77_numeric.sql` matches Postgres 16.15 digit for
digit; follow-ups are N-050 to N-053 in the next-list (N-054 is an unrelated SQLSTATE gap found while verifying).

The user chose a real decimal type over a float8 alias or a better refusal
(2026-11-11). Today `create table t (x numeric(10,2))` fails with "unknown
column type", and `::numeric` casts produce float8. The goal: Postgres
dumps with money columns load, and `numeric` behaves as Postgres 16 does,
checked with the difftest corpus.

## Decisions

| # | Decision | Why |
|---|---|---|
| Q1 | A new runtime Go type, `decimal.Decimal` (new leaf package `bytdb/decimal`, aliased as `bytdb.Decimal`), not a ride on an existing representation | uuid/jsonb ride on bytes/text because their order is bytewise. Numeric needs scale-insensitive equality (`1.0 = 1.00`) with scale-preserving display, so keys and stored values must differ. That needs its own encoding. |
| Q2 | Coefficient as `*big.Int` plus a display scale; the value is immutable | No new dependency; arbitrary precision as Postgres has. A small-int fast path is a later optimization if a benchmark asks for it. |
| Q3 | Two new tuple tags: `0x08` decimal key form (normalized, order-preserving), `0x09` decimal value form (same body + display scale) | Mirrors the -0 precedent: `Append` collapses for keys, `AppendValue` keeps for values. Old data never holds these tags; tags are permanent once released. |
| Q4 | Numeric primary-key columns are also written into the row value | PK columns decode from the key, where scale is normalized away. The value pair overrides the key decode, so `1.50` reads back as `1.50`. |
| Q5 | Decimal literals (`1.5`, `1e3`) are numeric, as in Postgres; integer literals stay integer | `select 0.1 + 0.2` = `0.3` and `numeric` columns take literals exactly. Float contexts get an implicit numeric→float8 conversion at bind, so float columns behave as before. |
| Q6 | Promotion: integer < numeric < float8 | Postgres's implicit casts: `int + numeric` is numeric, `numeric + float8` is float8. |
| Q7 | No `NaN` / `Infinity` numeric values in this round; refused with a clear error | Every arithmetic path would need special cases; no consumer asked. Tag space allows adding them later without rewriting data. |
| Q8 | Rounding is half away from zero (Postgres numeric), division scale follows Postgres's `select_div_scale` | Results must match Postgres digit for digit (`1/3.0` = `0.33333333333333333333`). |
| Q9 | Math functions without a numeric implementation (sqrt, exp, ln, log, power, cbrt, trig) convert a numeric argument to float8 and return float8 | Postgres returns numeric for some of these; doing them exactly is a separate project. Values agree; difftest keeps `@notype` there. |
| Q10 | `avg(integer)` returns numeric, as in Postgres; `sum(integer)` stays bigint | avg's numeric result is observable and cheap now. bytdb cannot tell int4 from int8 columns, and Postgres's sum(int4) is bigint, so sum stays. |

## Phases (one or more commits each, tests first)

1. **`decimal` package.** Parse (incl. exponent, sign, leading/trailing
   dot), canonical text, compare, normalize, add/sub/mul, Postgres
   division and mod, round/trunc to a scale (half away from zero),
   rescale with `numeric(p,s)` overflow check, conversions to and from
   int64 and float64 (float via shortest round-trip text), integral test.
   Fuzz: parse/format round trip, arithmetic identities.
2. **Tuple encoding.** Tags 0x08/0x09; order-preserving body (sign,
   exponent, base-100 digit pairs, inverted for negatives); `Compare`,
   `Skip`, `rank`; extend `FuzzTupleOrder` / `FuzzTupleRoundTrip`.
3. **Engine.** `TNumeric` with `Column.Precision` / `Column.Scale`
   (0/0 = unconstrained); `coerce` (Decimal, ints, float64, string) and
   typmod enforcement (round to scale, 22003 "numeric field overflow");
   PK value pairs (Q4); ALTER COLUMN TYPE to/from numeric; Go API
   (`Row` getter, `bytdb.Decimal` re-export).
4. **SQL layer.** Parser: `numeric[(p[,s])]`, `decimal[...]`, casts with
   typmod; decimal literals as numeric (Q5). Typecheck: promotion (Q6),
   unknown literal → numeric, `pgTypeName`. Evaluation: arithmetic,
   comparison, unary minus, `hashKey` normalization, casts in and out,
   CASE/COALESCE branch typing. Functions: abs, round, trunc, ceil,
   ceiling, floor, sign, mod, scale, greatest/least, min_scale/
   trim_scale if cheap; Q9 fallbacks. Aggregates: sum, avg, min, max
   over numeric; avg(int) (Q10). Pushdown of numeric key ranges.
5. **Surfaces.** pgwire: OID 1700, text and binary output, text and
   binary parameter input, parameter typing. Catalog: pg_type row,
   `format_type` → `numeric(10,2)`, `atttypmod` (`((p<<16)|s)+4`),
   information_schema `data_type`/`numeric_precision`/`numeric_scale`/
   `numeric_precision_radix`, psql `\d`. stdlib driver: returns
   numerics as string (as pgx/lib/pq do), accepts float64/string/int
   parameters. Shell: right-aligned, as text. `.dump` round trip.
6. **Difftest and docs.** New corpus file for numeric (DDL, typmod
   rounding and overflow, arithmetic scales, division, aggregates,
   casts, comparisons, unique-index equality, ORDER BY, functions); drop
   `@notype` where results are now typed. Docs: features.md type table,
   README dialect notes, the bytdb skill, gotchas (Q7, Q9 deviations).
   Next-list: close N-048; raise any deferred piece.

## Out of scope (record as follow-ups if wanted)

- NaN / ±Infinity numeric values (Q7).
- Exact numeric sqrt/exp/ln/power (Q9).
- `money`, `interval`, `bit`, `citext` (N-048's lower-value tail).
- A small-coefficient fast path (Q2).
