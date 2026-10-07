# Integer 0/1 into a bool column; release v0.21.2

- **Session:** `84cf43f6-6ea0-46e9-95ad-31aa337da6af`
- **Date:** 2026-10-06
- **Scope:** Driven from dbc's next-list N-131 (raised in dbc doc
  `2026-1006-1855-scripts-dir-resolution`), dropped in from the cats-todo
  backlog. dbc's copy of a SQLite table into bytdb failed on a boolean
  column. Fixed here in the engine's `coerce`, recorded as N-027, and
  released as **v0.21.2** / **pgwire/v0.21.2**.

## 1. The bug

- `s.Copy("demo-sqlite", "demo-bytdb", "cats", …)` failed on `adopted`.
  SQLite has no boolean type, so the driver returns a BOOLEAN column as
  `int64` 0/1. bytdb's `coerce` (`dml.go`) took only a Go `bool` for a
  `TBool` column, so the insert failed with "value does not fit column
  type". Copying from bytdb to SQLite already worked.
- The path: `database/sql` → `stdlib.CheckNamedValue` (ints narrow to
  `int64`) → the SQL layer's `coerceLit` (passes non-strings through) →
  the engine's `coerceRow` → `coerce`.

## 2. The fix (`38e193a`, `dml.go`)

- **Where:** the engine's `coerce`, not dbc's etl writer and not
  `stdlib`. One rule then covers the Go API, `database/sql` and pgwire.
  The pasted item allowed either place. The fix for N-026 / dbc N-128 was
  also made on the bytdb side.
- **Rule:** an integer 0 or 1, of any width, becomes `false`/`true`. This is
  `database/sql`'s `driver.Bool` rule. The bool case reuses the int case
  (`coerce(v, TInt)`), so the list of integer types lives in one place.
  Any other integer is refused with "integer for a bool column must be 0
  or 1", not treated as true C-style, since a 2 or -1 in a bool column
  usually means the wrong column was mapped.
- **Deviation from Postgres:** a literal `INSERT … VALUES (1)` into a
  bool column is accepted. Postgres refuses it (no implicit int-to-boolean
  cast). Bound parameters and literals look the same by the time they
  reach `coerce`. Telling them apart would mean tracking where each value
  came from through `bindParams`, which isn't worth it for this.
  Recorded in the code comment, the commit message and
  `docs/features.md`.
- **Comparisons are unchanged:** `WHERE adopted = 1` goes through the SQL
  layer's predicate coercion, not `coerce`. That path is outside this fix.

## 3. Tests and verification

- `stdlib.TestIntParameterIntoBool`: INSERT `$n` int64 0 and 1, UPDATE
  `$n` int64 1, reads back booleans, and checks that 2 is refused.
- `TestCoerceWidthsAndMismatches` (`scan_bounds_test.go`): `int64` 0/1,
  `int` 1 and `uint8` 0 go into the bool column, and the mismatch list
  gains 2, -1 and a float.
- Both tests fail on the old `coerce`: checked by temporarily putting
  `HEAD`'s `dml.go` back.
- `go vet` / `go test ./...` pass at the root and in `pgwire/`.
- **`/verify` over the wire** (scratch `bytdbd`, pgx v5.10.0 client):
  - The literals 0 and 1 are stored as false/true.
  - Simple-protocol `$n` `int64` works for INSERT and UPDATE.
  - The literal 2 is refused with SQLSTATE XX000, the same generic code as
    the old "does not fit" error.
  - Extended-protocol `int64` for a bool parameter is refused by pgx itself
    ("cannot find encode plan" for OID 16) before it reaches the server.
    That matches real Postgres and isn't a bytdb issue.

## 4. Docs

- `docs/features.md`: the `BOOL` row notes the 0/1 rule and the literal
  deviation.

## 5. Release v0.21.2

A patch: the only change since v0.21.1 is this fix. Followed the `release`
skill:

- `git fetch --tags`: `main` was level with `origin/main`.
- Root tag **`v0.21.2`** on `38e193a`, pushed. Resolves on
  proxy.golang.org.
- `pgwire/go.mod` pin `v0.21.1` → `v0.21.2`. `bench` `go mod tidy` raised
  its pin to match. Both build with and without `GOWORK=off`, and pgwire
  tests pass. Committed as `b958b26` "pgwire, bench: bump bytdb to v0.21.2".
- pgwire tag **`pgwire/v0.21.2`** on `b958b26`, pushed. Resolves on the
  proxy.

dbc still needs to bump to v0.21.2 to close its N-131. That is tracked
in dbc's next-list.

## Next

Closed: N-027. Declined: None. Raised: None.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
