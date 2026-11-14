# N-052: exact numeric sqrt / exp / ln / log / power, ported from numeric.c

Session: `0df97cd7-6d83-43d0-a135-f68ec255d4d6`. This is the second
batch of the `/work-next-list` run; the first is
`2026-1114-1610-n033-linux-pty-n049-orm-capture`. As with that doc, the
stem was fixed when the Next list was edited, a few minutes before this
doc was written.

## Premise check

- The numeric plan (`ai_docs/todo/2026-1111-numeric-type-plan.md`, Q9)
  deferred these functions as "a separate project". N-052 is that
  project.
- PG16 (docker `bytdb-numeric-pg`, 55432) corrected the item in two
  places:
  - **`cbrt` has no numeric version.** `cbrt(27.0)` is double precision
    in Postgres, so bytdb's float8 was already right.
  - **Two-argument `log` is numeric-only.** `log(2, 8)` is numeric
    `3.0000000000000000`, and `log(2::float8, 8::float8)` doesn't exist.
- Version choice, from pg_typeof:
  - A numeric argument with no float one → numeric.
  - Any float argument → float8 (power(2.0, 3::float8)).
  - Integer or untyped arguments → float8 (`sqrt('16')` is 4, double
    precision). float8 is the numeric category's preferred type.
- The item's other half, `round/ceil/floor/trunc/sign(int)` → double
  precision, would change what embedded callers read (int64 → float64)
  with no change in value. That is a decision, not a fix, so it is left
  in N-052 as a question with a recommendation (keep bigint).

## decimal/math.go

### First attempt: correctly rounded (discarded)

- Postgres's scale estimates were ported exactly, with values computed
  in big.Float (sqrt and integer powers exact) and rounded half away
  from zero.
- Against 10,390 random PG16 calls (`sqrt/ln/log10/log/exp/power`
  across magnitudes and scales, 15% near 1): **2 mismatches**, both
  `power` of a base near 1 to a short exponent.
- Python's decimal at 80 digits showed mine was the correctly rounded
  answer. Traced by hand, PG's `exp_var` stops its Taylor series after
  `1 + x` at a working scale of 24 places (x²/2 rounds to 0). That
  leaves the true value just under a tie at 16 places.
- Reproducing power_var's intermediate ln/product roundings alone didn't
  fix it, since the deciding step is inside exp_var. Near ties are
  systematic for bases near 1 and short exponents, so this was not a
  "1 in 10^8" leftover.

### Final: port the algorithms step for step

- `expVar` (exp_var):
  - float64 dweight estimate.
  - Halve until |x| ≤ 0.01 (ndiv2), with `div_var_int` at
    `dscale+ndiv2`.
  - Taylor series at local scale `sig-1`, stopping when a term rounds to
    zero.
  - ndiv2 squarings at `sig - weight·8`.
  - Round to rscale.
- `lnVar` (ln_var):
  - Square roots at `rscale - weight·4/2 + 8` until x is in (0.9, 1.1).
    The scale can be negative, rounding before the point.
  - fact doubles with each root.
  - atanh series at `rscale + (nsqrt+1)·log10 2 + 8`, with PG's early
    break `elem.weight < result.weight - local·2/4`.
  - Multiply by fact, rounded to rscale.
- `sqrtRound` (sqrt_var) is exact. It is the integer root of
  c·10^(2r+2-s), or of floor(c/10^k) when the exponent is negative,
  since ⌊√⌊y⌋⌋ = ⌊√y⌋. The extra digit is rounded half up. r may be
  negative.
- `Log` is log_var. It takes two `lnAt`s at
  `rscale + rdw - dw_x + 8`, then `DivRound`.
- `Pow`:
  - zero^negative → 2201F error.
  - An integral exponent fitting int32 → `powInt` (power_var_int).
  - Otherwise power_var: an 8-digit ln estimate → rscale, ln at
    `sig - lnDw + 8`, multiply, round, `expVar`. Negative base: only for
    an integral exponent, sign by parity.
- `powInt`:
  - Scale from double `n·(log10 f + p)` over up to 16 digits of base-10^4
    groups.
  - Specials for n = 0, ±1, 2.
  - Square-and-multiply with PG's local scales
    (`min(sig - 2·weight·4, 2·dscale)` and so on, and
    `sig += int(ln|n|) + 8`; C's `log`, natural).
  - Weight overflow past 32767 groups; negative n via `1/result`.
- **Assumption:** PG's mul_var/div_var are taken as exact-then-rounded.
  Their guard digits make that true except for freak carry chains.
- **Shared estimates:** `pgGroups` (base-10000 groups),
  `estimateLnDweight`, `clampScale` (16 significant digits, ≥ dscales,
  ≤ 1000).
- **Result: 0 / 10,390 random and 0 / 4,293 hard cases.** The hard
  corpus:
  - 1 ± 10^-k and 1 ± 5·10^-k for k ≤ 60, through every function and
    seven exponents.
  - ln/sqrt of 10^k up to 10^2600 and of 10^-k.
  - exp near ±6000 and results capped at 1000 places.
  - Integer powers up to ±2^31, and integral exponents past int32
    (parity sign).
  - 300 random inputs with 30–300 decimals.
  - Exact log cases (`log(2, 2^k)`, `log(10, 10^k)`).
- Two hard-corpus generator slips were caught on the way. One random
  call drew the exponent twice, so the recorded value differed from the
  one sent; fixed before comparing. One row (power(3, 2^31-1)) overflows
  in PG and in bytdb alike.
- All big.Float code was removed once nothing used it.
- **Cost:** `weight()` rendered the number as a string on every series
  term. `numDigits` from the bit length cut ln 25 → 17 µs and log
  58 → 38 µs (M-series, one call). The float64 estimate is checked
  against powers of ten on both sides, tested on 3000 sizes, and the
  comparisons stayed 0 mismatches.

## SQL layer (sql/funcs.go)

- `mathResult` / `logResult` → `pickMathVersion`:
  - Untyped arguments are cast to the chosen type, and integer ones too
    when numeric is chosen. Evaluation picks the version from the values
    alone, and `Describe` reports `$n` as that type.
  - `log($1, $2)` describes as numeric, `sqrt($1)` as float8.
- `mathEval(fl, dec)`: any float64 value → float path; otherwise →
  decimal path.
- The float versions now raise Postgres's errors:
  - `power`: 0^neg and neg^non-integer, where they returned Inf/NaN.
  - `ln`/`log`: "of zero" and "of a negative number" split, where there
    was one combined message.
  - `log(1, x)`: division by zero.
- `log10` added: one-argument log under its own name.
  - Bug caught by the CLI smoke run: the map holds `*scalarFunc`, so
    narrowing a copy's `max` capped `log` at one argument too. It now
    copies the struct.
- pgwire SQLSTATEs: `cannot take logarithm of …` → 2201E; square root /
  zero-to-negative / complex-result → 2201F.

## Tests and verification

- `decimal/math_test.go`:
  - 51 PG16 values: each scale rule, near-1 and near-tie cases, both
    exp_var-decided powers, integer and past-int32 powers, and
    1000-place and 2600-digit results by length and ends.
  - 14 errors, by sentinel and PG-confirmed order (log checks the base
    first).
  - Underflow to zero at scale 1000 by both power routes.
  - `TestNumDigits`.
- `sql/numeric_math_test.go`: 15 pg_typeof versions, values, NULL,
  placeholder types, 13 error texts.
- `sql/difftest/corpus/79_numeric_math.sql`: versions (`::text`, because
  PG's pg_typeof is regtype), table-driven calls over
  numeric/int/float8 columns, near ties, big exponents, untyped
  arguments, and 9 `@error` codes. It passes against PG16, with the
  whole corpus at 18 files.
- Mutation: `exp_var`'s working scale +5 → the near-tie test fails.
- `go test ./...` (root, pgwire), `-race` on decimal and sql, vet,
  staticcheck v0.8.2 on both modules, difftest/bench vet: all pass.
- `/verify` with pgx v5.10.0, 17/17:
  - Result OIDs per version (1700 vs 701).
  - Exact digits over the simple and cached-statement protocols.
  - Binary numeric decode of `power(2.0, 10)` (1024, exp −13).
  - Bound placeholders (`log($1,$2)` with strings, `power(2.0, $1)`
    with an int).
  - Prepared ParamOIDs `[1700 1700 701]`.
  - All 9 SQLSTATEs.

Commit `94be4da`.

## Docs

`docs/features.md` (math functions): describes the numeric versions,
the version rule, Postgres digits with the 14,683-call check, and the
error codes. `docs/gotchas.md`: the "exact numeric sqrt…" deviation row
is removed; the `round(int)` row stays.

## Run summary (both batches)

- Done:
  - N-033 Linux half (`ed56ea5`).
  - N-049 (`ee5df4b`), plus the CI lint fix `3514eae`.
  - N-052's numeric half (`94be4da`).
- Skipped:
  - N-050: needs a real workload.
  - N-051: trigger not met.
  - N-053: no consumer; `money` formatting is a product call.
- Questions for the user:
  - N-052's `round(int)` type: recommend keeping bigint.
  - N-056: cascade drops vs D-54's RESTRICT-only.
- Not released: root changes since v0.24.0 (N-047, N-049, N-052) are on
  main untagged.

## Next

Closed: None (N-049 and N-013 were closed in the first batch's doc).
Declined: None. Raised: None. Deferred: None. Promoted: None.
Moved: None. Updated: N-052 (numeric half done; only the `round(int)`
question remains, marked as the user's call).
Full list: `ai_docs/todo/next-list.md`.
