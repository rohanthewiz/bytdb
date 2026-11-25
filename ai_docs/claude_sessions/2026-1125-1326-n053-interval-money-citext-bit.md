# N-053: interval, money, citext and bit / varbit

Session: `8ee3e672-add0-4661-8ead-35ea85a6f91d`. The user pasted N-053
from the Next list. Two decisions were asked up front and answered: do
all four types, phased, one commit per type, interval first (it is the
only one with a named consumer, goose's table locker); and give money a
real type (int64 cents, OID 790, `$1,234.56`) rather than a numeric(p,2)
alias. The plan, decisions Q1–Q10 and as-built notes are in
`ai_docs/todo/2026-1125-interval-money-citext-bit-plan.md`. Work, save
and commits were all on 2026-11-25:

- `a783702` interval (phase 1)
- `a948f92` money (phase 2)
- `a7aa830` citext (phase 3)
- `d31c99b` bit / varbit and the bitwise operators (phase 4)
- `1441548` README and plan doc (phase 5)

## Reference

Postgres 16.9 in the `bytdb-numeric-pg` container (port 55432, still
running), psql at `/opt/homebrew/opt/libpq/bin/psql`. A throwaway
`postgres:17` container on 55491 answered one question (make_interval
overflow) and was removed. Port 55433 belongs to something else; don't
use it. The citext extension was created in the container's `postgres`
database while probing; the difftest corpus creates it if missing.

Each type's rules were probed in PG first, then pinned by goldens or
the corpus:

- **interval goldens** — `interval/testdata/gen.sql` + `gen.sh` capture
  9,331 cases (input with and without typmods, output, + − cmp, * / by
  float, justify_*, timestamp ± interval, timestamp − timestamp, age,
  extract, date_part, date_trunc, make_interval). `golden_test.go`
  replays them.
- **money goldens** — `testdata/money/gen.sql` + `gen.sh`, 2,739 cases
  (input, output, cash_words, casts, every operator).
  `money_golden_test.go` replays them.
- **Corpus** — `sql/difftest/corpus/81_interval.sql`, `82_money.sql`,
  `83_citext.sql`, `84_bit.sql`; the whole corpus is green.

## Design

### interval (`interval/`, a new leaf package)

- `Interval{Months, Days int32; Micros int64}` — Postgres's three
  fields, never normalized on their own. `bytdb.Interval` aliases it.
- **Comparison by span**: `(months*30 + days)*86.4e9 + micros` as a
  128-bit integer (`Span`). `'1 mon' = '30 days'`.
- **Tuple tags** `0x0A` (key: the 16-byte sign-flipped span) and `0x0B`
  (stored: the fields). A key decodes to `FromSpan`'s justified fields,
  which re-encode to the same key; interval is key-lossy, so a
  primary-key column is also written into the row value (the numeric
  precedent, now `keyLossy()` in engine.go).
- **Parser** mirrors PG16's ParseDateTime → DecodeInterval with the ISO
  8601 fallback. PG16 quirks kept: no consecutive-unit check (`'1 day
  day'` is 1 day), "ago" may repeat, a clock field overwrites clock time
  a fractional day set. PG17 tightened some of these.
- **Typmod** = Postgres's atttypmod packing (range<<16 | precision), in
  `Column.IntervalTypmod` (0 = none; no valid typmod is 0). Text bound
  for a typmod'd column is parsed with it (`'5'` is 5 minutes in
  `interval minute`) — in the engine (`coerceColumn`) and in the SQL
  layer (typer.value, columnDefaults, renderDefault keeps the text).
  A **bound parameter** is not: PG types it plain interval and then
  truncates, which `psql \bind` confirmed (00:01:00 vs 01:30:00).
- **SQL typing**: `ExArith.Kind` gained typed operator kinds
  (`arithTsMinusTs`, `arithTsInterval`, `arithIntervalNeg`), because a
  timestamp is a bare int64 at run time — int64 − int64 and 0 − Interval
  are ambiguous. Unknown literals resolve as PG does (`now() + '1 day'`
  is an interval, `now() - '2024-01-01'` a timestamp).
- **RANGE frames** take interval offsets over timestamp and interval
  keys (not in the plan; rolling windows need it). Date keys still
  refused → N-064.
- `make_interval` accepts named arguments (`days => 7`); the lexer
  gained `=>` and only `namedArgFuncs` use it.
- `SET intervalstyle` other than postgres is refused (22023) in both
  Session and bare DB paths.

### money (`money.go`, root)

- int64 cents (`TMoney`), like timestamps on int64 micros: keys,
  comparison and hashing need nothing new.
- cash_in / cash_out (en_US: `$1,234.56`, `-$1,234.56`), cash_words with
  its quirks ("One million  dollars", "hundred and five"), the casts, and
  operators with "money out of range".
- **Units vs cents.** SQL reads an integer or numeric assigned to money
  as whole units (Postgres's casts); the Go API's value is cents. The
  conversion (`moneyAssign`) applies only to constants and typed
  int/numeric assignments — `coerceLit` is called on runtime values in
  several paths (UPDATE SET, upsert, IN keys, ALTER USING) and must not
  multiply cents again; that was the first bug found. Money wire
  parameters are handed to the SQL layer as text for the same reason.
- money × float8 rounds half to even; money ÷ int truncates on cents;
  money ÷ money is float8; `money * '2'` scales and `money / '2'` is
  money ÷ money, as PG resolves unknowns.

### citext (`citext/`, a new leaf package)

- `citext.Text` (a named string) compares by its lowercase form, so
  every path that compares, hashes or keys values is right by
  construction. Tags `0x0C` (key: lowercase) and `0x0D` (stored: as
  written); key-lossy.
- citext reaches functions and `||` as text (an explicit cast the typer
  inserts, except for coalesce/greatest/least/nullif/pg_typeof);
  citext = text is text = text; LIKE/ILIKE/~ on citext ignore case.
- Fixed oid `bytdb.CITextOID` = 2100000000. difftest looks up PG's
  per-database citext oid after setup and maps both to one class, and
  keeps `public` on its search path.

### bit / varbit (`bits.go`, root)

- A string of '0'/'1' per bit; its byte order is bit_cmp's order.
  `Column.MaxLen` is n: bit(n) exact (bare bit = bit(1)), varbit(n) at
  most. Explicit casts pad/truncate.
- Lexer: `B'…'`/`X'…'` (token `tBits`, validated in the parser so a
  bad digit is 22P02), and `& | # << >>`; parser: prefix `~`. The
  operators work on integers too (shift counts wrap at 64).
- `bit_length`/`octet_length` over bits are rewritten by the typer
  into length arithmetic; `bit_length(text)` is new.

## Bugs found and fixed alongside

- **date 'infinity' → timestamp** became year 36040 (and the reverse
  cast was finite too). `bytdb.DateToTimestamp` / `TimestampToDate` now
  map the infinities; used by castValFrom, the typer's literal fold and
  ALTER.
- **Window functions dropped typed operator kinds.** `rewriteWindows`
  (and `bindExpr`) rebuilt `ExArith` without `Kind`, so `ts - ts` beside
  `count(*) over ()` was bigint. Found by the money tests after phase 1
  was committed; fixed in phase 2.
- **UNION/VALUES arms**: a later arm's untyped literal now takes the
  first arm's type (`unionCoerceArm`), as PG resolves it.
- **Typed literals in a select list** were lowered to untyped ones.
- A bound `time.Time` under an explicit `::timestamptz` was
  "unsupported cast".
- A negative RANGE offset was XX000; now 22013.
- `min`/`max` now keep the later of equal values (PG's transition
  functions return the new argument unless the old is strictly smaller).

## Verification

- Goldens: interval 9,331 (5 match only under fused multiply-add — the
  arm64 reference build fuses `a*b-c` in interval_mul; bytdb computes
  unfused, as x86-64 PG does; verified by recomputing with `math.FMA`);
  4 documented differences (PG16 make_interval wraps; PG17 and bytdb
  refuse; and one timestamp beyond bytdb's int64-µs-since-1970 range).
  money 2,739, all exact.
- Corpus 81–84 against PG 16.9, and the full corpus, green.
- `/verify`: a scratch `bytdbd` on 5439 and a pgx comparator
  (scratchpad `verify/`) running each case in simple, extended-text and
  extended-binary modes against PG, raw bytes and OIDs compared. interval
  42 checks, money 27, citext 21, bit 27. Remaining differences are
  only (a) int4 vs int8 result OIDs (N-058) and (b) PG raising a
  constant-folding error at plan time where bytdb raises it at execute
  (same SQLSTATE and message).
- Fuzzing: `FuzzTupleRoundTrip` / `FuzzTupleOrder` with interval and
  citext seeds and generators (30–45 s each, clean; one saved seed from
  a FromSpan min-value bug, fixed); new `interval.FuzzParse` (parse →
  print → parse round trip, 45 s, clean).
- Engine, SQL, wire (pgx three modes, `pgtype.Interval`, `pgtype.Bits`)
  and shell dump/restore tests for each type; goose's table locker
  verbatim (`TestGooseTableLockerVerbatim`).
- gofmt, vet, staticcheck v0.8.2 clean; all modules' tests and
  `go test -race` on root, sql, tuple, interval, citext, pgwire green.
- Benchmarks vs a `ccb2e09` worktree, 3 interleaved runs: −4.0% to
  +3.6%, inside run-to-run spread, allocations identical.

## Files

- New: `interval/` (interval.go, parse.go, format.go, typmod.go,
  calendar.go, tests, testdata), `citext/`, `money.go`, `bits.go`,
  `testdata/money/`, `tuple/testdata/fuzz/FuzzTupleOrder/5ce887544db57fa7`,
  root/sql/pgwire tests per type, corpus 81–84.
- Changed: tuple (tags 0x0A–0x0D), engine (types, coerce, keyLossy,
  typmods, ALTER casts), sql (parser, lexer, typecheck, expr, exec,
  funcs, agg, window, syscat, plan, explain, params, session), pgwire
  (values, errors, shell format), stdlib, docs (features, gotchas,
  README), the bytdb skill.

## Gotchas

- **Unknown-literal resolution differs per operator** in PG (money * '2'
  vs money / '2'; timestamp + '…' vs timestamp − '…'). Probe before
  guessing.
- **Generator SQL precedence**: `-x::type` casts before negating, and
  `c::numeric / 100` rounds 19-digit values (use `* 0.01`). Both bit the
  golden generators.
- **pgwire's go.mod** pins a released bytdb; with this session's new API
  it builds only through the workspace until the lockstep release.

## Next

Closed: N-053. Declined: None. Raised: N-064, N-065, N-066, N-067,
N-068, N-069. Deferred: None. Promoted: None. Moved: None.
Updated: N-055. Full list: `ai_docs/todo/next-list.md`.
