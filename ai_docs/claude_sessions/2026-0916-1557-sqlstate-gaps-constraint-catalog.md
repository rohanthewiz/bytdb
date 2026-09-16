# SQLSTATE gaps, constraint catalog tables, bench tidy

- **Session:** `2a4ce5dd-fc86-45e9-9bc8-68ed89b55a8b`
- **Date:** 2026-09-16
- **Scope:** drain the `2026-0916-1511` Next list except the encryption
  set: `pgwire/errors.go`, `sql/syscat.go`, `bench/go.mod`, docs.

## Ask

`/sl`, then "do all in the Next list except the encryption set".

## Work completed

### Item 2 — pgwire SQLSTATE gaps (`pgwire/errors.go`)

- `invalid input syntax for type ...` → **22P02** (invalid_text_representation),
  for every type (int, float, bool, bytea, date, timestamp, uuid, json).
- `cannot drop constraint "t_pkey"` → **42P16**, joining the `multiple
  primary keys` branch; both protect the one-key-per-table invariant.
- **Newly found in the wire check:** bind-time parameter decoding
  (`pgwire/values.go`) returned XX000. Now `bad <type> parameter` → 22P02
  and `bad binary <type> parameter` → **22P03**
  (invalid_binary_representation). The text match is pinned by prefix,
  suffix, and exactly two spaces, so other `bad ...` messages cannot fall
  in; `bad parameter format code` stays XX000.
- Cases added to `TestSQLStateMapping` (`pgwire/errors_state_test.go`).
- Message texts were not changed — mapping only.

### Item 3 — `information_schema.table_constraints` (+ `key_column_usage`)

- `table_constraints`: one row per PRIMARY KEY (`bytdb.PKConstraintName`),
  unique index (as UNIQUE, `nulls_distinct = 'YES'`), FOREIGN KEY, and
  CHECK. `is_deferrable`/`initially_deferred` `NO`, `enforced` `YES`.
- `key_column_usage` added too, because ORMs join the pair to get key
  columns; `table_constraints` alone does not say which columns. Rows for
  PK, UNIQUE, and FK in key order; FK rows carry
  `position_in_unique_constraint` (= ordinal, since `RefCols` are stored
  in child-column order). CHECK has no key-column rows.
- **Deliberate differences from Postgres** (in the code comment and
  `docs/features.md`): every unique index reports as UNIQUE (bytdb's
  `UNIQUE (cols)` is an index, indistinguishable); NOT NULL columns do not
  appear as synthetic `<oid>_<oid>_<n>_not_null` CHECK rows.
- Test: `TestInfoSchemaConstraints` (`sql/syscat_test.go`) — composite PK
  order, composite FK positions, CHECK without key rows, a non-unique
  index excluded, read-only guard.

### Item 4 — `bench/go.mod`

`go mod tidy` bumped the bytdb pin v0.11.0 → v0.14.0. Builds in the
workspace and with `GOWORK=off`.

### Docs

`README.md` (catalog list), `docs/architecture.md` (catalog list),
`docs/features.md` (constraint introspection paragraph, 22P02 in the
SQLSTATE examples).

### Verification

- `gofmt -l` clean, `go vet` clean (root, pgwire).
- Full suites green: bytdb, replicate, replicate/s3, sql, stdlib, tuple,
  pgwire; `-race` green on sql.
- **Wire check** (`/verify`, bytdbd + pgx v5.10.0): `select 'abc'::int`
  and a bad uuid literal → 22P02; a bad text int bound over extended
  protocol → 22P02 (was XX000 before the follow-up fix); `DROP CONSTRAINT
  users_pkey` → 42P16; the `table_constraints` ⟕ `key_column_usage` join
  returns identical rows over simple and extended (cached statement)
  protocol.

## Files touched

`pgwire/errors.go`, `pgwire/errors_state_test.go`, `sql/syscat.go`,
`sql/syscat_test.go`, `bench/go.mod`, `README.md`, `docs/architecture.md`,
`docs/features.md`.

## Next

*Legend: **age** = session docs since the item was first written down,
counted from this doc (0 = raised here). **value** = payoff, not effort:
**high** means something is worked around today; **medium** means it blocks
one named thing; **low** means nobody has run into it yet or it depends on
something that does not exist.*

1. **btypedb encryption deferred set** **(age 29 · value low)**: online key
   rotation, plaintext↔encrypted migration helper, key+value scope,
   ChaCha20-Poly1305. First written `2026-0722-1303`. Still absent —
   `btypedb/encrypt.go:41` reserves flag bits 1..4, `compact.go:139` names
   the re-encrypt seam. Rotation needs a v3 wrapped-DEK header first.
   Excluded from this session's drain by the user; kept open.
2. **`pg_constraint` omits primary keys and unique constraints** **(age 0 ·
   value low)**: it lists only CHECK and FK (keys surface via `pg_index`),
   while `information_schema.table_constraints` now lists all four. Tools
   that read `contype IN ('p','u')` would miss keys; nobody has reported it.
3. **`bad parameter format code` maps to XX000** **(age 0 · value low)**:
   Postgres treats an invalid Bind format code as a protocol violation
   (08P01). Pre-existing; noticed while mapping the parameter errors.
4. **Release the SQLSTATE and catalog changes** **(age 0 · value low)**:
   untagged on main after `v0.14.0`; cut `v0.14.1`/`v0.15.0` (root +
   pgwire lockstep) when a consumer needs them.

Read by value instead: **high** none · **medium** none · **low** 1, 2, 3, 4.

### Deliberate non-goals (declined, not dropped)

- **Unique indexes vs UNIQUE constraints in `table_constraints`.** bytdb
  cannot distinguish them; all unique indexes report as UNIQUE.
- **Synthetic NOT NULL CHECK rows in `table_constraints`.** Nullability is
  in `information_schema.columns`.
- **General multi-action `ALTER TABLE`.** Only `DROP CONSTRAINT t_pkey, ADD
  PRIMARY KEY` is accepted.
- **`ALTER COLUMN TYPE` on foreign-key columns.** Drop the constraint,
  alter both sides, re-add it.
- **pgwire `v0.10.0` / `v0.11.0` tags stay unbackfilled.**
- **Unindexed FK columns scan the child table per check.** Documented with
  the "index the child FK columns" workaround.
- **Embedded Engine one-shot writes surface `ErrTxConflict` raw.** Caller
  retries; documented in `docs/concurrency.md`.
- **Correlated ON conjuncts and function-wrapped correlated predicates
  evaluate per row.** Rewrite as JOIN for large outer sets.
- **`WriteString` lint hints in test files.**
