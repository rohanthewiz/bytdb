# pg_constraint lists primary keys and unique constraints (N-003)

- **Session:** `7d096582-a222-4143-a208-7c1b2ce0eea6`
- **Date:** 2026-09-24
- **Scope:** `/sl` loaded the previous session doc and the living list. Then
  N-003 was done: `pg_constraint` now has `p` and `u` rows, and `conkey`
  and `confkey` are filled.

## 1. The gap

`pg_constraint` listed only CHECK (`c`) and FK (`f`) rows. Keys showed up
through `pg_index` only, while `information_schema.table_constraints`
already listed all four kinds. `conkey` and `confkey` were NULL on every
row, so even a tool that found an FK row couldn't read its columns.

## 2. The change

**`sql/syscat.go`, the `pg_constraint` rows:**

- **Primary keys:** one `p` row per table, named `<table>_pkey`
  (`bytdb.PKConstraintName`).
- **Unique constraints:** one `u` row per unique index. Every unique index
  counts, which matches `table_constraints` (the N-013 decision):
  `UNIQUE (cols)` is sugar for a unique index, so the two can't be told
  apart. Plain indexes get no row.
- **Oid:** a key row's `oid` is its backing index's oid (`indexOID`), and
  `conindid` points at the same index. Postgres gives the constraint its
  own oid, but a table's 1000-oid block is full:

  | Range in the block | Used by |
  |---|---|
  | index IDs | indexes |
  | 800+ | column defaults (`pg_attrdef`) |
  | 900+ | checks |
  | 950+ | FKs |

  An index oid never falls in the check or FK ranges, so oids stay unique
  within `pg_constraint`. psql joins on `conindid = indexrelid` and never
  compares `con.oid` to a `pg_class` oid. The code comment records the
  reasoning.
- **`conkey`:** the constrained columns' attnums as an int2[] text literal
  (`{1,2}`), from the new `attnumArray` helper. It is filled for `p`, `u`
  and `f` rows. CHECK rows keep NULL, because filling it would mean
  parsing the stored expression text.
- **`confkey`:** filled on `f` rows. `refAttnums` maps the FK's referenced
  column names to the parent's attnums, and returns NULL rather than a
  partial array if a name is missing.
- **Column type:** `conkey` and `confkey` stay `TString`, matching the
  catalog's other array columns (`datacl`, `prattrs`). Over the wire they
  are text, not `int2[]`. `attnum = ANY(conkey)` works, but a pgx client
  scanning into `[]int16` would not.

**`sql/expr.go`, `constraintdef`:** now renders `PRIMARY KEY (a, b)` and
`UNIQUE (a)` as well as CHECK and FK. It matches on `indexOID`, with a
`ix.Unique` guard, so a plain index's oid still renders NULL.

**Effect on psql:** the `\d` index listing's LEFT JOIN to `pg_constraint`
now finds the pkey (`contype = 'p'`, constraintdef `PRIMARY KEY (id)`).
Unique indexes will render as `UNIQUE CONSTRAINT, btree (...)` rather
than `UNIQUE, btree (...)`, which is consistent with the N-013 stance.

## 3. Tests

**New:** `TestSystemCatalogKeyConstraints` (`sql/syscat_test.go`) covers:

- a composite PK
- a table-level UNIQUE and a column-level UNIQUE
- a plain index, which gets no row
- a composite FK with `conkey` `{2,3}` and `confkey` `{1,2}`
- `pg_get_constraintdef` for all of these
- the `attnum = ANY(conkey)` join to `pg_attribute`
- the `conindid` join to `pg_class` and `pg_index`
- NULL constraintdef for a plain index oid
- agreement with `table_constraints`

**Extended:** `TestPsqlDescribeTable` now checks that the pkey row carries
`contype = 'p'` and its constraintdef, and that the plain index row
carries none.

**Adjusted:** three tests selected *all* `pg_constraint` rows for a table,
so the new `_pkey` row broke them. Their queries were narrowed to the kind
under test, which keeps their intent:

- `TestSQLCheckValidation` and `TestSQLAddDropConstraint` → `contype = 'c'`
- `TestFKCascadeCatalog` → `contype = 'f'`

**Results:** `go vet` is clean, and `go test ./...` passes in root and
`pgwire/`. No `/verify` wire run was done: the change only adds catalog
rows, and they go through the same wire path the previous session
verified.

## 4. Docs

- `README.md` catalog list
- `docs/architecture.md`
- `docs/features.md`: a new paragraph on `pg_constraint` keys, `conkey`
  and `confkey`, and the oid reuse
- `sql/sql.go` package comment
- the stale "pg_constraint lists CHECK constraints" note in `syscat.go`

## 5. Noticed, not filed

`conrelid::regclass` prints the table's number (e.g. `100`), not its name
as Postgres would. This predates the change, and nothing depends on it.

## Files touched

`sql/syscat.go`, `sql/expr.go`, `sql/syscat_test.go`, `sql/psql_test.go`,
`sql/sql_test.go`, `sql/fk_cascade_test.go`, `sql/sql.go`, `README.md`,
`docs/architecture.md`, `docs/features.md`, `ai_docs/todo/next-list.md`.

## Next

Closed: N-003. Declined: None. Raised: N-018.
Deferred: None. Promoted: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
