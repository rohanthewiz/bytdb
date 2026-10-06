# Date input accepts timestamp text; release v0.21.1

- **Session:** `d422360d-20aa-45ed-88cc-067de08a117f`
- **Date:** 2026-10-06
- **Scope:** Driven from a dbc session (dbc's next-list N-128, raised in
  dbc doc `2026-1006-1430-headless-dbc-copy`). dbc's ETL copy of a date
  column into bytdb failed. Fixed here in `ParseDate`, recorded as N-026,
  and released as **v0.21.1** / **pgwire/v0.21.1**.

## 1. The bug

- dbc's `dbc copy` and `s.Copy` read a Postgres `date` with pgx, which
  yields a midnight-UTC `time.Time`. bytdb's database/sql driver
  (`stdlib.CheckNamedValue`) binds every `time.Time` as
  `t.UTC().Format("2006-01-02 15:04:05.999999")`, and the SQL layer adapts
  that text to the column type. For a date column that is `ParseDate`,
  which took only `YYYY-MM-DD`, so the insert failed with "invalid input
  syntax for type date".
- The same failure reproduced from a SQLite source: modernc returns DATE
  columns as `time.Time` too.

## 2. The fix (`92757e1`, `types.go`)

- **Where:** `ParseDate`, not the driver. Every date text path goes through
  it (SQL literal coercion in `sql/coerce.go`, `sql/expr.go` casts, the Go
  API's string case in `dml.go`, pgwire parameters in `pgwire/values.go`),
  so all of them take Postgres's date input now. The driver keeps sending
  UTC text, which lands on the UTC day. That is the same day the Go API's
  `coerce` truncates a `time.Time` to (`dml.go`), so the two paths agree.
- **Semantics (checked against Postgres 17 in a throwaway container):**
  - Any timestamp text is a date: the date as written, with the time and
    zone parsed for syntax, then dropped. They are never applied:
    `'2024-01-02 23:30:00-05'::date` is 2024-01-02, not the UTC date
    2024-01-03.
  - A malformed time is still an error, as in Postgres
    (`'2024-01-02 25:00:00'`, `'2024-01-02 junk'`).
  - Implementation: the bare-date layout is tried first (the common case),
    then every `tsLayouts` entry. The day count is rebuilt from `t.Date()`
    in the parsed location as midnight UTC, so the division stays exact
    for pre-1970 dates.
- **Not covered:** timestamps without seconds (`'2024-01-02 23:30'`).
  Postgres accepts them, but `tsLayouts` never has, for timestamps either.
  Unchanged here.
- **Side effect, Postgres-compatible:** a predicate on a date column with
  a timestamp literal now coerces the literal to its date
  (`day < '2024-03-05 10:00'` means `day < 2024-03-05`), as Postgres does
  for an untyped literal.

## 3. Tests

- `TestValueTextRoundTrip/date_from_timestamp_text` (`types_format_test.go`):
  the Postgres 17 results above as expectations, plus the inputs that must
  still fail.
- `stdlib.TestTimeParameterIntoDate`: binds midnight UTC, 23:59:59.999999
  UTC, 08:00 at +09:00 (the UTC day before) and a pre-1970 time to a date
  column. It fails on the old `ParseDate` with the N-128 error.
- `go vet` / `go test ./...` green at the root and in `pgwire/`.
- **Seen from dbc:** pointed at this tree through a throwaway `go.work`
  outside both repos, dbc's `etl.Copy` of DATE columns SQLite→bytdb
  works. With dbc's v0.21.0 pin it fails as before.

## 4. Docs

- `docs/features.md`: the `DATE` row says timestamp text is accepted, and
  that a `time.Time` binds as its UTC day.

## 5. Release v0.21.1

A patch: the only change since v0.21.0 is this fix. Followed the `release`
skill:

- `git fetch --tags`: `main` was level with `origin/main`.
- Root tag **`v0.21.1`** on `92757e1`, pushed. Resolves on
  proxy.golang.org.
- `pgwire/go.mod` pin `v0.21.0` → `v0.21.1`. `bench` `go mod tidy` raised
  its pin to match. Both build with and without `GOWORK=off`, and pgwire
  tests pass. Committed as `28cabb8` "pgwire, bench: bump bytdb to v0.21.1".
- pgwire tag **`pgwire/v0.21.1`** on `28cabb8`, pushed. Resolves on the
  proxy.

dbc still needs to bump to v0.21.1 and add a date column to an etl
SQLite→bytdb test to close its N-128. That is tracked in dbc's next-list.

## Next

Closed: N-026. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
