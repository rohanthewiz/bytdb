# N-060: a multi-column foreign key's default name

Session: `944eb7c6-dd43-4d9c-a2c8-afb944e82304`. The user pasted N-060
from the Next list. Work, save and commit were all on 2026-11-20. The
code is commit `8cbbaf2`.

## The defect

`resolveFK` (`sql/fk.go`) named an unnamed foreign key
`table + "_" + def.Cols[0] + "_fkey"`, so `FOREIGN KEY (x, y)
REFERENCES p (a, b)` on `ch` became `ch_x_fkey`. Postgres's
`ChooseForeignKeyConstraintNameAddition` joins every referencing column
with `_`, giving `ch_x_y_fkey`, the same shape bytdb already uses for
`UNIQUE (a, b)` (`t_a_b_key`, via `freeIndexName`). A migration tool
that computes Postgres's name and drops by it got 42704.

## Change

- `sql/fk.go`: the base name is now
  `desc.Name + "_" + strings.Join(def.Cols, "_") + "_fkey"`. The
  collision loop is unchanged, so a second unnamed key over the same
  columns is `ch_x_y_fkey1`, as in Postgres. CREATE TABLE and
  ALTER TABLE ADD FOREIGN KEY both go through `resolveFK`, so both
  changed.
- **Stored names are untouched.** The name lives in the table's
  `FKDesc`, so a database created before this keeps `ch_x_fkey` for its
  existing keys. Only new unnamed constraints get the new form. No
  migration was written: renaming stored constraints underneath a user
  would break whatever already refers to the old name.
- `sql/ast.go`: the `FKDef.Name` comment says `table_col1_col2_fkey`.
- `docs/features.md`: the foreign-keys section gained a bullet on
  default names (all columns, numeric suffix on a clash). The section
  said nothing about FK naming before, while UNIQUE's form was already
  documented.

Not changed: names aren't truncated to 63 bytes. Nothing in bytdb does
that for any identifier, so the FK name was kept consistent with
`freeIndexName`. That gap is raised as N-063.

## Tests

- New `TestFKDefaultNameMultiColumn` (`sql/fk_test.go`): a single-column
  key is still `ch_pid_fkey`; the inline `(x, y)` key is `ch_x_y_fkey`;
  a violation message names `"ch_x_y_fkey"`; `ALTER TABLE ADD FOREIGN
  KEY (x, y)` again gives `ch_x_y_fkey1`; both drop by those names.
- `TestDropColumnTakesDependents` (`sql/drop_cascade_test.go`) used to
  skip the (x, y) key's name because of this item. It now requires
  `ch_x_y_fkey`.
- `TestSystemCatalogKeyConstraints` (`sql/syscat_test.go`) had the old
  name baked in. It now expects `members_org_id_org_code_fkey`. The row
  order by `conname` is unchanged.
- `go test ./...` passes in the root module (all packages) and in
  pgwire, `cmd/bytdb` and `cmd/bytdbd`. `go.work` uses `.`, so pgwire's
  tests ran against the change, not the pinned v0.24.0.

## Wire check (`/verify`)

`bytdbd` was served on a scratch db at 127.0.0.1:5439 and driven by a
pgx v5.10.0 client. Simple and extended protocol were both used: the
`pg_constraint` lookup binds the table name as `$1`. All five passed:

1. `pg_constraint` after CREATE: `[ch_pid_fkey ch_x_y_fkey]`.
2. Violation: `insert or update on table "ch" violates foreign key
   constraint "ch_x_y_fkey"`, SQLSTATE 23503.
3. A second ADD FOREIGN KEY: `[ch_pid_fkey ch_x_y_fkey ch_x_y_fkey1]`.
4. `DROP CONSTRAINT ch_x_y_fkey` and `ch_x_y_fkey1` work.
5. `information_schema.referential_constraints` agrees.

The server was stopped afterwards.

### Seen along the way

In check 2, pgx's `PgError.ConstraintName` was empty. pgwire's
ErrorResponse (`pgwire/errors.go`) writes only the S, V, C, M, D, H and
P fields. It never writes Postgres's `n` (constraint), `t` (table), `c`
(column), `s` (schema) or `d` (data type) fields. ORMs that map a
violation to a constraint read `n`. Raised as N-062.

## Next

Closed: N-060. Declined: None. Raised: N-062, N-063. Deferred: None.
Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
