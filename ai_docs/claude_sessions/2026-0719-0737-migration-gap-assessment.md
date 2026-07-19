# Migration Gap Assessment — App-to-bytdb Postgres Gaps Verified

- Session ID: `bc9f93fc-3ed9-45e6-9d33-83bcee99b83b`
- Date: 2026-07-19
- Repo: `~/projs/go/bytdb` (main @ 8611539)

## Context

An external gap analysis (produced while evaluating bytdb as a Postgres replacement
for an app using lib/pq + vattle/sqlboiler v2) listed four "real gaps" plus smaller
items. This session verified each claim against current bytdb source (post
audit-remediation, through commit 0e80c26). Two of the four claimed gaps are stale.

## Verdicts

### 1. text[] array columns — ACCURATE (real gap)

- `typeName` (`sql/parser.go` ~1043) accepts only: int/float/text/varchar/bool/bytea/
  timestamp/timestamptz/date/uuid (+ serial family as identity). `text[]` and any
  array type fail with "unknown column type".
- `array_to_string` / `array_length` parse but are catalog-compat shims that always
  return NULL (`sql/expr.go:866`).
- App fix stands: store delimited text or JSON-encoded string; replace
  `types.StringArray` with a small encode/decode helper.
- **Correction to the proposed fix:** bytdb has **no `LIKE` or `ILIKE` at all** —
  no LIKE operator exists in parser or evaluator. It has the Postgres regex family
  `~`, `!~`, `~*`, `!~*` (`sql/expr.go:540`, cached compiled regexes). The sermon
  search rewrite must use `~*` (case-insensitive regex), not "lower() LIKE".

### 2. jsonb columns — ACCURATE

- No json/jsonb type; such column defs are rejected. Migrations must declare `text`.
- App treats these as opaque blobs (all JSON parsing in Go, no `->`/`->>` in
  queries), so this is cheap, as the analysis said.

### 3. Timestamps — OUTDATED (gap no longer exists)

- Native `TTimestamp`: int64 microseconds since Unix epoch, always UTC (`types.go`).
  Also native `TDate` and `TUUID`.
- Parser accepts `timestamp`, `timestamptz`, `timestamp(p)`, `WITH/WITHOUT TIME
  ZONE` (precision parses and is ignored; storage is always micros — Postgres's own
  max precision).
- Wire layer serves real Postgres type OIDs with Postgres-format text
  (`pgwire/values.go`), e.g. `2024-01-02 03:04:05.123456+00`, so lib/pq scans
  straight into `time.Time` with no app changes.
- `serial`/`bigserial`/etc. parse natively as identity + NOT NULL int columns.
- Net: `created_at`/`updated_at`, `ORDER BY created_at DESC`, and SQLBoiler
  `time.Time` scanning all work as-is.

### 4. SQLBoiler v2 layer — PLAUSIBLE (unverified without the app repo)

- Odds are better than the analysis implied. bytdb's SQL surface now covers: quoted
  identifiers, `$N` params, JOINs (nested-loop + hash join), `IN (SELECT ...)`,
  CTEs, views, window functions, `ON CONFLICT` upserts, RETURNING.
- Concrete landmine: any generated query using `LIKE` fails to parse (see item 1).
- The app's newer features (chat, prayerwall, apitoken, recurrences, sermon cache)
  already bypass SQLBoiler via a db.Executor seam — migration accelerates that.

## Smaller items

- **"bytdb has no FKs" — OUTDATED.** FK constraints exist and are enforced: MATCH
  SIMPLE, NO ACTION/RESTRICT, checked at end of statement inside the statement's
  txn (`sql/fk.go`, `fk.go`). `ON DELETE CASCADE` and `SET NULL/DEFAULT` are
  **explicitly rejected at parse time** (`sql/parser.go` `referencesClause`, ~941)
  rather than silently weakened. Conclusion stands: do the 4 cascade deletes in app
  code and strip `CASCADE` from the schema — but real referential integrity exists
  underneath.
- **RETURNING — SUPPORTED** on INSERT/UPDATE/DELETE. `lastval()`, `currval()`,
  `nextval()`, `setval()` all exist (`sql/seqfuncs.go`). No workaround needed for
  chat/prayerwall inserts.
- **Migration rewrite — smaller than claimed.** `BIGSERIAL` and `timestamptz` parse
  as-is. Real edits: jsonb→text, text[]→text, drop CASCADE actions, strip
  `OWNER TO`. Also note: `char(n)` and time-of-day `time` types are rejected with
  clear errors (use varchar/text and timestamp).

## Gap the analysis missed

- **Column DEFAULT accepts constants only** — `DEFAULT now()` is deliberately
  rejected (`sql/parser.go` `defaultLiteral`: defaults are applied, not
  re-evaluated per insert). Nearly every `created_at timestamptz DEFAULT now()` in
  the app's 12 goose migrations will fail to parse. Set timestamps in app code or
  in the INSERT itself (`now()` IS supported as an expression: `sql/expr.go:803`,
  along with `transaction_timestamp`, `statement_timestamp`, `clock_timestamp`,
  `current_date`, `gen_random_uuid`).
- Check how goose bootstraps its version table — if it uses `DEFAULT now()`, the
  migration runner itself won't connect cleanly.

## Net app-side work

Items 1 (arrays + search rewrite to `~*`), 2 (jsonb→text), 4 (SQLBoiler SQL
survivability), plus a `DEFAULT now()` sweep and 4 app-side cascade deletes.
Timestamps, RETURNING/lastval, and FK integrity are already solved on the bytdb
side.

## Verification method

Grepped/read current source: `sql/parser.go` (typeName, referencesClause,
defaultLiteral, serialTypes), `sql/expr.go` (function/operator inventory, regex
ops), `sql/fk.go`, `fk.go`, `types.go`, `sql/seqfuncs.go`, `pgwire/values.go`,
plus session docs in `ai_docs/claude_sessions/` (returning-clause,
upsert-defaults-lastval, audit-remediation-complete).
