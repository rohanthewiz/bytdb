# Plan: `interval`, `money`, `citext` and `bit` / `varbit` (N-053)

**Status (2026-11-25): phases 1 (interval) and 2 (money) done**;
phases 3–5 to go.
The user chose all four types, phased, with a real money type rather
than a numeric(p,2) alias (2026-11-25).

Today each of the four fails CREATE TABLE with "unknown column type",
and `interval '1 day'` has no type to land on. The goal: each type
behaves as Postgres 16 does for the everyday surface (literals, columns,
casts, operators, comparison and ordering, indexes, aggregates, the wire
in text and binary), checked against PG 16.9 by the difftest corpus and
a three-mode pgx wire check.

`interval` goes first. It is the only one with a named consumer: goose's
table locker sends `now() + $3::interval`, and `now() - interval '7
days'` is common in application SQL.

## Decisions

| # | Decision | Why |
|---|---|---|
| Q1 | **interval** gets a new runtime Go type, `interval.Interval{Months, Days int32; Micros int64}` (new leaf package `bytdb/interval`, re-exported as `bytdb.Interval`): Postgres's three fields, unnormalized | Postgres keeps the fields apart: `'1 mon'` and `'30 days'` add differently to a timestamp but compare equal. No existing representation holds three fields. |
| Q2 | Two new tuple tags for interval. `0x0A` is the key form: the 128-bit span `(months*30 + days)*86400e6 + micros`, sign-flipped big-endian. `0x0B` is the stored form: the three fields | This is Postgres's `interval_cmp_value`, so `'1 mon' = '30 days'` is one key. A unique index refuses the second, GROUP BY/DISTINCT fold them, and an index lookup finds either. It is the numeric precedent (Q3/Q4 of N-048): keys normalize, stored values keep the form written. |
| Q3 | **citext** gets a runtime Go type too, `citext.Text` (a named string, leaf package `bytdb/citext`, re-exported as `bytdb.CIText`). Tags `0x0C` (key: the lowercased text) and `0x0D` (stored: the text as written) | Case-insensitive equality has to hold in every place that compares, hashes or keys a value: WHERE, ORDER BY, GROUP BY, DISTINCT, hash joins, unique and PK indexes, FK checks, upsert. Those paths dispatch on the runtime value, not the static type. A distinct Go type makes each of them right by construction. A plain string would need static types threaded into all of them, and a missed one would fail silently. |
| Q4 | The key-lossy types (numeric, interval, citext) all write primary-key columns into the row value too | Generalizes N-048's Q4. The key form loses the display (`1 mon` vs `30 days`, `ABC` vs `abc`), so the value pair restores what was written. |
| Q5 | **money** rides on int64 cents (`TMoney`), the way timestamp rides on int64 micros | Equality, order, hashing and keys of int64 are exactly money's. Display (`$1,234.56`), the operators that differ (money × float8 rounds back to money, money / money is float8) and the casts are typed statically. No new tuple tag is needed. |
| Q6 | money's text is fixed to Postgres's `en_US`/`C` form: `$1,234.56`, `-$1,234.56`, 2 fraction digits. `lc_monetary` is not modelled | Docker Postgres and most servers run `en_US.utf8`. Input accepts `$`, thousands commas, a sign or parentheses, and rounds to cents as `cash_in` does. |
| Q7 | **bit** and **varbit** ride on a string of `'0'`/`'1'` (`TBit`, `TVarBit`). `Column.MaxLen` carries the n of `bit(n)`/`varbit(n)` | Byte order of that string is `bit_cmp`'s order: the first differing bit decides, and a prefix sorts first. So keys, ORDER BY and hashing are right with no new encoding. A bit costs a byte, which is fine for a type nobody has asked for. |
| Q8 | `timestamp - timestamp` returns an interval (days and time, as Postgres's `timestamp_mi`), no longer bigint microseconds | It matches Postgres, and interval now exists. **Release note:** embedded callers get a `bytdb.Interval` where they got an int64. |
| Q9 | Postgres 16 parity. Interval `infinity` (PG 17+), IntervalStyle other than `postgres`, `to_char(interval)`, and `lc_monetary` are out of scope, recorded as follow-ups if wanted | PG 16.9 is the reference container. Each of these is separable. |
| Q10 | citext presents a fixed pg_type OID that bytdb picks, since Postgres assigns it at CREATE EXTENSION time. `CREATE EXTENSION citext` stays a no-op, and the type is always available | Clients look citext up by name (`pg_type.typname`). difftest maps each side's citext OID to one class. |

## Phases (one or more commits each, tests first)

1. **interval.** Package `interval`: the fields, Postgres's input forms
   (units with fractions cascading down, `@ … ago`, `hh:mm:ss`, signed
   parts, `Y-M` SQL form, ISO 8601 `P…` both forms), `postgres`-style
   output, span compare, `+ - * /` with overflow (22008), justify_*.
   The typmod field qualifiers (`interval day to second`, `interval(3)`,
   `interval '5' minute`). Tuple tags `0x0A`/`0x0B`, with fuzz order and
   round trip. Engine: `TInterval`, coerce (`Interval`, `time.Duration`,
   text), PK value pair, ALTER to and from text. SQL: literals and casts,
   timestamp/date ± interval (calendar months with day clamping),
   timestamp − timestamp (Q8), interval arithmetic and comparison, hash
   keys, sum/avg/min/max, extract/date_part on interval, `make_interval`,
   `age`, `justify_*`, `date_trunc` on interval, `isfinite`. Catalog: pg_type
   1186, `format_type`, information_schema `interval_type`. Wire: OID 1186,
   text and binary both ways. Driver: interval values as Postgres text.
   CLI: dump round trip. Tests: difftest corpus file, and goose's table
   locker statements verbatim.
2. **money.** `TMoney` on int64 cents. Text in/out (Q6). Operators:
   money ± money, money × and ÷ int (truncating) or float8 (rounding),
   int/float8 × money, money ÷ money → float8, comparisons. Casts to and
   from numeric, int → money, text. sum/min/max. Out-of-range is 22003.
   pg_type 790, wire OID 790 (binary int8). Driver: money as its text.
   Corpus file.
3. **citext.** Package `citext`, tags `0x0C`/`0x0D`, `TCIText`. Typing:
   citext against citext or an unknown literal compares
   case-insensitively. citext against text resolves to text, as in
   Postgres. Text functions take citext as text and return text.
   LIKE/ILIKE on citext ignore case. min/max return citext. Catalog and wire
   (fixed OID, Q10). Corpus file, with difftest's OID mapping.
4. **bit / varbit.** Lexer `B'…'` and `X'…'`. `TBit`/`TVarBit` with
   length enforcement (22026) and cast padding or truncation (`::bit`
   is `bit(1)`). Operators `& | # ~ << >> ||` (equal lengths for
   `& | #`), comparisons. Functions length, bit_length, octet_length,
   get_bit, set_bit, bit_count, substring, position. Casts with int
   (explicit) and text. pg_type 1560/1562, wire binary (bit length plus
   packed bytes). Corpus file.
5. **Docs and close.** features.md type table, gotchas (remove the
   refused-types row, add Q6/Q9 deviations), README, the bytdb skill. A
   pgx three-mode `/verify` wire check against PG per type. Next-list:
   close N-053, raise what Q9 deferred.

## Out of scope

- Interval infinity, IntervalStyle `sql_standard`/`iso_8601`/
  `postgres_verbose`, `to_char` on interval (Q9).
- money in other locales (Q6).
- Arrays of any of these types.
- `overlay` on bit strings; bit strings in hash/btree opclass catalogs.
