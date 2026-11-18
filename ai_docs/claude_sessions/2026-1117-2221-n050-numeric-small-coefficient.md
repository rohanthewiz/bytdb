# N-050: small-coefficient fast path for numeric

Session: `d065e6d9-a20c-44c1-a99f-8da298a22cbb`. The user pasted N-050
from the Next list. The item asked to measure a real workload before
starting. The measurement showed a much larger gap than the item's
micro-benchmark, so the change went ahead. The work was done 2026-11-15
and saved and committed 2026-11-17 with `/sw`.

## Measurement first

The baseline from the item's own benchmarks (`sql/numeric_bench_test.go`,
-count 3): sum+avg over 100k `numeric(10,2)` rows took 16.5 ms, against
11.0 ms for float8, with 5.1 allocations per row against 3.0. The
cross-scale filter took 17 ms. An allocation profile put the extra 2
allocations per row in decode: `FromMantissaU64` built a `big.Int` and
`nat.make` its words.

The real workload is new: `sql/numeric_workload_bench_test.go`. A
TPC-H lineitem shape: 100k rows with `numeric(15,2)` quantity, price,
discount and tax, three return flags, two statuses and a date. The
`lif` table holds the same values as float8. It runs:

- **Q1**: products of numeric columns summed per group, plus avgs;
- **Q6**: range filters on numeric columns, then a summed product;
- **Fetch 10k**: 10k rows, every numeric rendered with pgwire's text
  encoders (`AppendText` / `bytdb.AppendFloat`).

Before the change:

| | numeric | float8 | allocs/row (numeric vs float8) |
|---|---|---|---|
| Q1 | 120 ms | 53 ms | 50 vs 17 |
| Q6 | 37 ms | 23 ms | 13 vs 7.6 |
| Fetch 10k | 7.1 ms | 4.0 ms | 23.5 vs 6.8 |

A Q1 CPU profile put about 40% of the time in decimal work:

- `decimalArith` was 20%: `big.Int` Mul/Sub plus `align`'s temporaries;
- decode allocations took most of the rest;
- `asDecimal(int64 1)` called `FromInt64` on every row, which allocated.

## The change (`decimal/decimal.go`)

`Decimal` went from `{coef *big.Int; scale int32}` to
`{wide *big.Int; small int64; scale int32}`, 24 bytes.

- **Small form.** `wide == nil`, and the coefficient is `small`. MinInt64
  is excluded, so negation and `absU64` can never overflow on the fast
  paths.
- **Wide form.** `wide != nil`, used only when the coefficient doesn't
  fit an int64.
- **Canonical.** `mk(*big.Int)` demotes any result that fits, and
  `mkSmall(int64)` promotes MinInt64. So a value has exactly one
  representation, and `reflect.DeepEqual` in `sql/numeric_test.go` and
  `tuple` tests still means "equal digits and scale". Zero is
  `Decimal{scale: s}`.
- **No union.** The struct is not an unsafe union of pointer and int.
  That would save 8 bytes, but the GC's view of the pointer word would
  then depend on a tag. Boxing is one allocation either way.
- **`c()`** materializes a `*big.Int` for the wide paths: `math.go` and
  the mixed small/wide fallbacks. It allocates for a small value, but the
  small paths never call it.

Small paths, each with explicit overflow checks that fall through to the
original `*big.Int` code:

- **Parse.** Digits accumulate into a uint64 while `acc ≤
  (MaxInt64-9)/10`. The digit string for `SetString` is rebuilt from the
  input only on the fallback. `net > 0`, e.g. "1e3", goes wide and `mk`
  demotes it.
- **AppendText.** `strconv.AppendUint` into a stack `[20]byte`. The point
  placement is shared with the wide path through a generic
  `appendPointed[D []byte | string]`.
- **Cmp.** Equal scales compare int64s directly. Otherwise the existing
  `cmpSmall`, which builds a 128-bit product.
- **Add / Sub.** `alignSmall` rescales with `scaleUp`, which checks for
  overflow (`mulPow10` plus `≤ MaxInt64`). Signed overflow is detected
  with `(x^sum)&(y^sum) < 0` and `(x^y)&(x^diff) < 0`.
- **Mul.** `bits.Mul64` on the magnitudes. The result is small when
  `hi == 0 && lo ≤ MaxInt64` and the scale sum is ≤ MaxScale.
- **DivRound** (so Div and avg). `divRoundSmall` builds `|n|·10^shift` as
  a 128-bit value and divides with `bits.Div64`, guarded by
  `hi < den` (Div64 panics otherwise). A negative shift multiplies the
  divisor instead. Half away from zero is tested as `r ≥ den-r`, so no
  2r overflow. avg of money values, scale about 12, stays on this path.
- **Mod.** Go's `%`, which truncates with the dividend's sign, like
  `big.Int.Rem`.
- **rescale** (Round/Trunc). Widening uses `scaleUp`. Narrowing by k ≥ 20
  digits is always 0, since |coef| < 10^19. k ≤ 19 divides by
  `pow10u64[k]`. A negative target scale multiplies back up through
  `scaleUp`.
- **Floor / Ceil.** `t.small ∓ 1` through `mkSmall`.
- **Normalize.** `%10` loop. **Neg.** `-small`.
- **IsInteger, Int64, IntDigits, lead, weightAndFirst** (and so
  DivScale). `digitsU64`: bit length × 1233 >> 12, plus one table
  compare.
- **Float64.** Clinger's exact fast path, the one strconv takes: |coef| ≤
  2^53 and scale ≤ 22 means `float64(coef) / 1e<scale>`, which is
  correctly rounded. This also took allocations out of float8 Q6, which
  compares float columns against numeric literals.
- **Mantissa.** Digits on the stack; only the returned string is
  allocated.
- **FromMantissaU64 / FromMantissa (≤ 19 digits).** A shared
  `mantissaSmall`, the per-row decode, which allocates nothing.
- **Sum.** Gains `small int64` and `wide bool`. `addSmall` falls back by
  `promote()`, which moves the total into the `big.Int` for good. In the
  wide path a small operand is read into `tmp`; math/big allows the
  receiver to alias an operand. `AddInt64(MinInt64)` goes straight to
  wide because `alignSmall` passes an unscaled operand through
  unchecked.

The package doc has a new "Two coefficient forms" section with the
layout table and the design reasons. It replaces the old design note,
which said the fast path was "left for when a benchmark asks for it".

## Results

-count 6, means. The "before" set from this session was noisy (float8
baseline 11.3–15.6 ms), so these use the quiet baseline from the start
of the session.

| | before | after | float8 | allocs/row before → after |
|---|---|---|---|---|
| Q1 | 120 ms | 69 ms | 52 ms | 50 → 17 (float8 17) |
| Q6 | 37 ms | 27 ms | 19 ms | 13 → 6.0 |
| Fetch 10k + text | 7.1 ms | 3.7 ms | 4.2 ms | 24 → 7.0 |
| sum+avg | 16.5 ms | 13.5 ms | 11.4 ms | 5.1 → 3.0 (float8 3.0) |
| cross-scale filter | 17 ms | 14.6 ms | — | 5.1 → 3.0 |

Most of what's left on Q1 is boxing. Each arithmetic result is a 24-byte
Decimal in an interface, one allocation per operator per row. float8
boxes its 8 bytes the same way.

## Tests and verification

`decimal/small_test.go` is new. The oracle is the original code: every
wide path accepts a non-canonical wide operand, so running an operation on
`wideOf(a)`, `wideOf(b)` is the pre-change implementation.

- **Inputs.** `smallCoefs` straddles the digit-count steps, the rounding
  halves, 10^18 and 10^19, √MaxInt64 (3037000499/500), ±MaxInt64,
  MaxInt64/2+1 and 2^53±1. Scales are 0–25.
- **`TestSmallMatchesWide`.** 37 unary checks per value and 11 binary
  ops per pair, about 150k pairs. Each result must match the wide path's
  digits, scale and error, and be canonical.
- **`TestSumSmallMatchesAdd`.** Sum sequences that cross the int64 edge
  both ways, mixing scales and `AddInt64`, including MinInt64.
- **`TestDigitsU64`** and **`TestSmallFormBoundaries`.** The latter
  checks MaxInt64+1 promotes, subtracting 1 demotes again, -MaxInt64-1
  is wide, and 19- and 20-digit literals.
- **`FuzzSmallMatchesWide`.** Ran 120 s clean.
- **Mutation check.** Two deliberate mutations (`r >= den-r` → `>`, and
  dropping Add's overflow check) were each caught on the first failing
  case.

Wider checks:

- **Unit suites.** `go test ./...` (root), pgwire's tests and the
  `sql/difftest` corpus against PG16 all pass.
- **Static checks.** staticcheck v0.8.2, built from the module cache,
  plus gofmt and vet are clean.
- **`/verify`, three-way wire check.** New bytdbd (5439), a pre-change
  bytdbd built from HEAD's decimal.go (5440) and PG16 (55432). Each ran
  in the simple protocol (text) and the extended protocol with
  DescribeExec (binary numeric), comparing raw cells:
  - 21 boundary values × 40 unary expressions;
  - every pair × 8 binary expressions;
  - tables loaded by literal, by text parameter and by binary
    `pgtype.Numeric` parameter, then sum/avg (including sums past
    int64), GROUP BY, DISTINCT, self-join on numeric, IN, BETWEEN and
    typmod 22003 on insert;
  - parameterized arithmetic in both formats.

  Result: 9,102 checks. New and old were identical on every one. All 117
  differences from PG were pre-existing (raised below):
  - `^` is not lexed;
  - `::int4` does not range-check;
  - binary integer results are 8 bytes wide.

## Files

- `decimal/decimal.go`: the representation and the fast paths.
- `decimal/small_test.go`: new; the differential tests and the fuzz
  target.
- `sql/numeric_workload_bench_test.go`: new; the lineitem Q1, Q6 and
  fetch benchmarks, with the before/after table in its header.
- `sql/numeric_bench_test.go`: header records the 2026-11-15 numbers.
- `sql/bench_deeplook_test.go`: `TestMain` also removes the lineitem
  dataset's temp dir (`liDir`).
- `ai_docs/todo/next-list.md`: N-050 closed; N-057 to N-059 raised.

## Gotchas

- **benchstat is not installed.** `go run
  golang.org/x/perf/cmd/benchstat@latest` was denied, since it needs a
  download. Means and ranges came from an awk summary of `-count 6`
  output instead.
- **staticcheck.** `~/bin/staticcheck` and `~/go/bin/staticcheck` are
  built with go1.23 and refuse this go1.26.1 module. `go run
  …@v0.8.2` with `GOPROXY=off` fails on the deprecation lookup. What
  works is building it from the cached module:
  `cd ~/go/pkg/mod/honnef.co/go/tools@v0.8.2 && GOPROXY=off
  GOFLAGS=-mod=mod go build -o <scratch>/staticcheck ./cmd/staticcheck`.
- **Benchmark noise.** This M-series machine had stretches of heavy
  noise: some runs were 2× slow, with b.N falling from about 80 to 33. A
  first "after" run showed sum+avg 33% slower. A quiet rerun gave a clean
  13.2–14.1 ms. Check b.N and the per-run ranges before believing a
  regression.
- **Building the pre-change server.** Swap `git show
  HEAD:decimal/decimal.go` in, and move `small_test.go` aside (it uses
  the new internals). Build `pgwire/cmd/bytdbd`, then restore both.
- **pgx result formats.** `QueryExecModeExec` returns text results, so
  binary numeric results need `QueryExecModeDescribeExec`.

## Next

Closed: N-050. Declined: None. Raised: N-057, N-058, N-059.
Deferred: None. Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
