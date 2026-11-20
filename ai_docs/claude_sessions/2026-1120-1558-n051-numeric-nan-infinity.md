# N-051: numeric NaN and ±Infinity

Session: `49023138-f403-4a37-a8b0-373288e15b77`. The user pasted N-051
from the Next list. Its trigger ("a dump or a client that writes one")
had not fired, but the user asked for the item, so it was built. Work,
save and commit were all on 2026-11-20. The code is commit `af703bb`.

## Reference first

Every rule was checked against Postgres 16.9 before any code was
written. The PG container was gone, so a new one was started:
`docker run -d --name bytdb-numeric-pg -e POSTGRES_PASSWORD=pg -p
55432:5432 postgres:16.9`. The image was already local, so nothing was
pulled. psql is at `/opt/homebrew/opt/libpq/bin/psql`. The verify skill
says psql isn't installed, which is out of date.

The probes confirmed numeric.c's rules as recalled. Two of them were
not obvious:

- **Spelling.** `'-NaN'` is a syntax error: NaN takes no sign. The
  infinities take a sign, any case, and `inf` as well as `Infinity`.
- **Typmod.** NaN fits `numeric(5,2)`, but an infinity fits no
  constrained column. The DETAIL is "A field with precision 5, scale 2
  cannot hold an infinite value."

## Design

### `decimal.Decimal` (`decimal/decimal.go`, `decimal/math.go`)

- **Representation.** A `kind uint8` field: finite, NaN, +Inf, -Inf.
  It sits in the padding after `scale`, so the struct is still 24 bytes.
  `finite` is 0, so the zero value is still 0 and no existing value
  changes meaning. A special value has every other field zeroed, so the
  form stays canonical (`reflect.DeepEqual` still works).
- **Guards.** Each operation tests `d.kind|o.kind != finite` first and
  hands specials to a small helper: `addSpecial` (Sub is `addSpecial(d,
  o.Neg())`), `mulSpecial`, `divSpecial` (zero at scale 0 for Div,
  DivRound's own scale otherwise), Mod's inline branch, and `powSpecial`
  (numeric_power's POSIX pow(3) list, in its order).
- **The risk.** A special has `wide == nil, small == 0`, so any method
  that misses the guard silently treats it as 0. Every exported method
  was audited, and `TestSpecialRepresentation` pins each accessor's
  answer for each special.
- **Accessor choices:**
  - `Sign` is ±1 for the infinities and 0 for NaN (numeric.c never asks
    for NaN's sign). `checkLog` tests NaN before Sign so that 0 can't
    read as "log of zero".
  - `IsInteger` is false for all specials. numeric.c's
    `numeric_is_integral` counts the infinities as integral, but only
    power uses it, and `powSpecial` decides that itself.
  - `Int64` fails for specials. `Float64` maps to the float specials,
    with NaN as `0x7FF8000000000000`; see the wire check below.
  - `Scale` is 0. `Coef`, `Mantissa` and `IntDigits` read as 0 or empty,
    and callers check `IsFinite`.
- **Log's error order.** In the special branch, `Log` checks both
  arguments' signs before either zero, as numeric_log does: `log(0,
  -Inf)` is "negative". The finite path keeps ln_var's base-first order.
- **Sum.** It records `sawNaN`, `sawPosInf` and `sawNegInf` beside the
  finite total, the way NumericAggState counts them. `Value()` applies
  the rules, and the overflow check uses `finiteValue()` so a pending
  infinity can't hide a digits overflow.
- **API.** New: `NaN()`, `Inf(sign)`, `IsNaN`, `IsInf(sign)` (math.IsInf
  style), `IsFinite`, and `TypmodError.Infinite`. `ErrSpecial` is
  kept but deprecated; nothing returns it.

### Tuple encoding (`tuple/tuple.go`)

- **Classes.** Three were needed, not the two the item guessed:
  -Infinity `0x00`, +Infinity `0x04`, NaN `0x05`. Each is the class byte
  alone, plus the 2-byte scale (0) in the stored form.
- **Order.** Byte order is value order, so:
  - a unique index holds one NaN (`23505` on the second);
  - an index scan for `x > 5` includes Infinity and NaN, as Postgres's
    `NaN > 5` is true.
- **Compatibility.** No byte written earlier changes meaning. An older
  binary reading a special value fails with "unknown decimal class".

### SQL, engine glue and wire

- **Casts to integers.** `numeric → int` errors for specials with
  `cannot convert NaN|infinity to bigint|integer|smallint`, named by the
  cast's spelling (0A000). The helper `specialToIntErr` exists twice,
  unexported, in the root and sql packages, rather than adding exported
  API.
- **Functions.** `sign(NaN)` is NaN. `scale` and `min_scale` of a
  special are NULL, and `trim_scale` returns it unchanged.
- **Hash keys** (`sql/hashkey.go`). Numeric NaN folds to `nanKey` and
  ±Inf to the float64 infinities, so `SELECT x UNION SELECT
  'NaN'::float8` keeps one NaN, as in PG.
- **Binary wire** (`pgwire/numeric.go`). Specials are a bare header with
  sign `0xC000`, `0xD000` or `0xF000`. On receipt, the digits are
  length- and range-checked as numeric_recv checks them, then ignored.
- **CLI** (`pgwire/cmd/bytdb/format.go`). Specials are quoted in `.dump`
  SQL and rendered as JSON strings, as float8's already were and as PG's
  to_json renders them.
- **SQLSTATEs** (`pgwire/errors.go`). The cast errors map to 0A000 and
  "timestamp cannot be NaN" to 22008. The old ErrSpecial text keeps its
  0A000 mapping.

### Two bugs found alongside (both on main before this)

- **float8 NaN defaults.** A float8 column with `DEFAULT 'NaN'` or
  `'Infinity'` refused every INSERT that used the default. `renderLit`
  wrote the float as strconv's `NaN` / `+Inf`, unquoted, and
  `parseStoredLiteral` couldn't lex it back. This was confirmed against
  a HEAD build in a scratch worktree. Non-finite floats and Decimals are
  now stored quoted, in PG's spelling.
- **`to_timestamp(NaN)`.** It returned `int64(NaN·1e6)`, an arbitrary
  instant. It now raises PG's `timestamp cannot be NaN` (22008).

## Verification

- **PG goldens** (`decimal/special_test.go`). A plpgsql wrapper captured
  PG's `::text` result or error for every binary op (+ - * / mod power
  log cmp) over 17 operands with at least one special (744 cases), and
  13 unary functions over 11 values (143). Each finite operand is also
  run in the forced-wide form. All 887 passed first time. A deliberate
  mutation in `addSpecial` was caught.
- **Unit tests.** Parse spellings, representation and accessors, typmod,
  Sum ordering, tuple order and round trip (asc and desc), the numeric
  binary codec, an engine test (every Go input shape, PK and unique-index
  NaN, range scan, reopen, ALTER to and from float8 and int), an SQL
  test with PG's captured answers, and CLI dump/restore/JSON.
- **difftest corpus.** `sql/difftest/corpus/80_numeric_special.sql`
  runs against PG16 and is green. A mutation in Mod's special branch was
  caught.
- **Fuzzing.** `FuzzTupleRoundTrip` and `FuzzTupleOrder` ran 60 s each
  (2.7M and 18.9M execs), clean, with the new specials as seeds and
  boundary values.
- **`/verify`, three-mode pgx check.** Simple protocol, extended with
  text results (Exec) and with binary results (DescribeExec), with raw
  cells compared byte for byte against PG16. Inputs included binary
  `pgtype.Numeric{NaN}` and Infinity/-Infinity parameters, plus a binary
  insert read back into `pgtype.Numeric`. The first run found two byte
  differences:
  - **dscale.** PG sends dscale **32** for both infinities. numeric_send
    reads a special value's header as a short numeric's, and
    `0xD000 & 0x1F80 >> 7 = 32`. bytdb now matches (`infDscale`).
  - **NaN bits.** numeric → float8 NaN was Go's `math.NaN()`
    (`…0001`); PG and bytdb's stored float8 NaN are `…0000`. Fixed with
    `pgNaN`.

  After both fixes there were 0 differences in 58 checks. Integer
  columns were wrapped in `::bigint` to hide the known 8-byte int width
  (N-058).
- **Static checks.** gofmt, vet and staticcheck v0.8.2 are clean, as are
  the root, sql and pgwire suites and pgwire/cmd/bytdb.
- **Benchmarks.** Six interleaved runs each, HEAD worktree vs. working
  tree. Means moved +0.5 to +2.7% (Q1 89.0 → 91.4 ms, Q6 36.6 → 36.7,
  Fetch10k 4.80 → 4.87, SumAvg 18.6 → 19.1, Filter 19.4 → 19.9). That is
  within run-to-run spread (Q1 base 87.6–90.3, new 88.5–94.8), and
  allocations are identical. The machine was slower overall than on
  2026-11-15 (Q1 69 ms then).

## N-061's reference answers (PG16)

`r(id, x numeric, f float8)` holds -Infinity, -1, 0, 2.5, Infinity,
NaN and NULL (ids 1–7, the same values in x and f). The columns are
counts over `ORDER BY x` with these frames:

- a: `RANGE BETWEEN 'Infinity' PRECEDING AND CURRENT ROW`
- b: `CURRENT ROW AND 'Infinity' FOLLOWING`
- c: `1 PRECEDING AND 1 FOLLOWING`
- d: `ORDER BY x DESC` with a's frame
- e: `UNBOUNDED PRECEDING AND 'Infinity' PRECEDING`

| id | x | a | b | c | d | e |
|---|---|---|---|---|---|---|
| 1 | -Infinity | 1 | 5 | 1 | 5 | 1 |
| 2 | -1 | 2 | 4 | 2 | 4 | 1 |
| 3 | 0 | 3 | 3 | 2 | 3 | 1 |
| 4 | 2.5 | 4 | 2 | 1 | 2 | 1 |
| 5 | Infinity | 5 | 1 | 1 | 1 | 5 |
| 6 | NaN | 1 | 1 | 1 | 1 | 6 |
| 7 | NULL | 1 | 1 | 1 | 1 | 7 |

The float8 column gives identical counts. `'NaN'` and `'-Infinity'`
offsets are 22013. bytdb refuses every numeric RANGE offset frame, and
on float8 the quoted `'Infinity'` offset; see N-061.

## Files

- `decimal/decimal.go`, `decimal/math.go`: kind, the special-value
  rules, Sum's counters, `pgNaN`.
- `decimal/special_test.go` (new): the PG goldens and the special-value
  tests. `decimal/decimal_test.go`: the old refusal tests updated.
- `tuple/tuple.go`: the three classes. `tuple/tuple_test.go`,
  `tuple/fuzz_test.go`: order and round-trip tests, seeds.
- `numeric.go`, `default.go`: engine glue, cast error, comments.
- `sql/numeric.go`, `sql/expr.go`, `sql/funcs.go`, `sql/hashkey.go`,
  `sql/parser.go`: the SQL changes and the renderLit fix.
- `pgwire/numeric.go`, `pgwire/errors.go`, `pgwire/cmd/bytdb/format.go`:
  the wire codec, SQLSTATEs and CLI. Their tests are updated or added.
- `sql/difftest/corpus/80_numeric_special.sql` (new).
- `docs/features.md`: the numeric row describes the specials.
  `docs/gotchas.md`: the "refused" row is removed.

## Gotchas

- **A scratch worktree needs its own go.work.** Relative `use` paths
  resolve against the go.work file's directory, so build a worktree of
  HEAD with a go.work that has absolute paths (the worktree, its
  pgwire, and `~/projs/go/btypedb`) through `GOWORK=<file>`. Remove the
  worktree with `git worktree remove --force` afterwards.
- **The difftest `@todo` marker takes only `D-xx` ids.** Gaps outside
  the deep-look plan can't be parked in the corpus, which is why N-061's
  queries were taken out rather than marked.
- **difftest's nested go.work** uses `../..`, so it tests the local tree.
  The first run downloaded `btypedb v0.11.0` for the module graph.
- **`bytdb-numeric-pg` is still running** on 55432. Stop it with
  `docker rm -f bytdb-numeric-pg`.

## Next

Closed: N-051. Declined: None. Raised: N-061. Deferred: None.
Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
