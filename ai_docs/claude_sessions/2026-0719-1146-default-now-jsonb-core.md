# Session: DEFAULT now() + jsonb core

- Session ID: `e17458e6-070f-474f-ab99-85ad21b25660`
- Date: 2026-07-19
- Branch: `main` (started clean at `1507645`)

## What was asked

Assess, then implement, the two remaining engine-side migration gaps:
`DEFAULT now()` support and a jsonb type. Decision: do DEFAULT now()
first (small, independent), then jsonb core following the text[]
blueprint, deferring jsonb operators until the app's query surface
says which ones are needed.

## DEFAULT now() / current_date

Previously rejected by design ("defaults are constants, applied rather
than evaluated"). The design now admits exactly the clock functions as
evaluated markers — general expressions in DEFAULT stay rejected.

- **Parser** (`sql/parser.go` `defaultLiteral`): `now()`,
  `transaction_timestamp()`, `statement_timestamp()`,
  `clock_timestamp()` (parens required), and the keyword forms
  `CURRENT_TIMESTAMP` / `LOCALTIMESTAMP` all normalize to the
  `DefaultNow` marker — the same normalization Postgres does.
  `CURRENT_DATE` → `DefaultCurrentDate`. `current_time`/`localtime`
  get a targeted rejection (no time-of-day type).
- **Marker type** (`sql/ast.go`): `type ExprDefault string` with
  `DefaultNow = "now()"`, `DefaultCurrentDate = "current_date"`. A
  distinct type so a *string constant* spelling "now()" can never be
  the marker; the descriptor stores marker text **unquoted**, which
  `renderLit` can never produce for a string constant — that's the
  collision-proofing.
- **Validation** (`sql/sql.go` `toEngineColumn`): markers allowed on
  timestamp/date columns only; stored as `col.Default = "now()"` /
  `"current_date"`. Both CREATE TABLE and ALTER TABLE ADD COLUMN flow
  through here.
- **Evaluation** (`sql/exec.go` `columnDefaults`): runs once per
  INSERT statement inside the write txn; one `time.Now().UTC()` shared
  by all rows and all clock columns of the statement (statement-
  granular cousin of Postgres's per-transaction freeze).
  `current_date` truncates to midnight UTC *before* coercion so it
  lands as midnight on a timestamp column; both markers then go
  through the existing `coerceLit` time.Time arm (micros for
  timestamp, days for date).
- **Catalog**: `information_schema.columns.column_default` reports
  `now()` verbatim, matching Postgres.

## jsonb core (TJSONB)

Follows the TTextArray playbook exactly: runtime value is a string —
the canonical rendering of the document — canonicalized on every write
path so plain string `=` implements jsonb document equality.

- **Canonical form** (`types.go` `CanonJSONB`): Go `encoding/json`
  compact rendering, object keys sorted lexically, last duplicate key
  wins, `UseNumber` so numeric source text survives (20-digit ints,
  deliberate decimals), `SetEscapeHTML(false)`, trailing-garbage
  check via `dec.Token() == io.EOF`. Postgres sorts keys
  length-first and adds spaces — cosmetically different, invisible to
  any JSON-parsing client; what matters is one spelling per document
  *within this engine*.
- **Engine**: `TJSONB ColType = "jsonb"` (`engine.go`), validTypes
  (`ddl.go`), write coercion accepts `string` and `[]byte`
  (json.RawMessage's shape) (`dml.go`).
- **SQL layer**: type names `jsonb` *and* `json` both map to TJSONB
  (documented: jsonb semantics; not worth a second type) —
  `sql/parser.go` `typeNameBase`. `jsonb[]` rejected by the existing
  array-suffix guard. `coerceLit` canonicalizes so WHERE literals
  compare in canonical spelling. `::json`/`::jsonb` cast = parse +
  re-render (`sql/expr.go` `castVal`); OID 3802 added to the
  format-type-name switch.
- **Catalog** (`sql/syscat.go`): typeOID 3802, sqlTypeName/udtName
  "jsonb", pg_type row `{3802, "jsonb", -1, "U"}`.
- **Wire** (`pgwire/values.go`): `oidJSONB = 3802`, `oidJSON = 114`
  (input only; columns present as jsonb). Encode: text = the stored
  string; binary = version byte `1` + text. Decode params: text
  passes through (engine/coerce canonicalize downstream); binary
  jsonb strips/checks the version byte; binary json is bare text.
- **Not included, deliberately**: jsonb operators (`->`, `->>`, `@>`,
  `?`, jsonb_set…) and any jsonb-specific ordering/indexing (ORDER BY
  is bytewise on canonical text). Scope operators to the app's actual
  queries in a follow-up.

## Tests

- `jsonb_test.go` (root): CanonJSONB canonicalization table +
  fixed-point property, malformed table (incl. multi-document),
  engine writes via string and []byte, malformed refused at write.
- `sql/jsonb_test.go`: DDL, canonicalized reads, cross-spelling
  equality, ::jsonb cast, constant jsonb DEFAULT, json alias,
  jsonb[] rejection, information_schema + pg_type.
- `sql/default_test.go` `TestDefaultNow`: all spellings, per-statement
  instant sharing across rows, current_date-on-timestamp = midnight,
  now()-on-date = days, catalog marker text, `'now()'` string-literal
  non-collision. Rejection table updated (wrong-type errors replaced
  the old "defaults are constants" cases; bare `now` without parens
  is a parse error).
- `pgwire/jsonb_test.go`: pgx map round-trip (binary both ways),
  canonical stored text via json.RawMessage, cross-spelling equality
  over the wire, malformed param clean error.
- Updated: `engine_test.go`/`alter_test.go` used "jsonb" as the
  canonical *unknown* type — now "hstore". `sql/sql_test.go`
  expectation updated.

## Verification

`go test ./...` green. `/verify` end-to-end: built `bytdbd`, served a
scratch db on :5439, drove it with a pgx v5.10.0 client — 17 checks
covering both features over simple + extended protocols (RowDescription
OID 3802, binary jsonb params, RETURNING with evaluated defaults,
column_default reporting, malformed-input errors), all pass. Second
client run after a server restart on the same db file: marker still
evaluates, stored jsonb still compares canonically (REOPEN PASS).

## State / next steps

- ~16 files modified + 3 new test files, committed with this doc.
- Remaining app-migration gap: ON DELETE CASCADE usage in app code
  (engine supports NO ACTION/RESTRICT only, by design).
- jsonb operators: grep the app's queries for `->`/`->>`/`@>` etc.
  and implement exactly that set next.
- Memory file `btypedb-local-workspace.md` updated with both features.
