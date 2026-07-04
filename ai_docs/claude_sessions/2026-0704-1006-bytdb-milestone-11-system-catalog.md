# Session: bytdb-milestone-11-system-catalog

- **Session ID**: `8094c5d6-d2b3-472e-9f32-38e5583d85a9` (same session as milestone 10; continues `2026-0704-0943-bytdb-milestone-10-postgres-wire-protocol.md`)
- **Date**: 2026-07-04
- **Repo**: `~/projs/go/bytdb`
- **Result**: milestone 11 complete — virtual pg_catalog/information_schema, SELECT without FROM, folded system functions, schema-qualified names, ORDER BY ordinals. Real ORM probes (GORM HasTable, pg_attribute/pg_type joins, `select pg_catalog.version()`) pass verbatim over the wire; psql runs catalog queries interactively. All green under `-race`, both modules.

## Design: virtual tables through the ordinary executor

The one load-bearing decision: system tables enter a query as **materialized rows on the FROM scope** (`scopeTable.rows`, non-nil = virtual), so WHERE/joins/aggregates/ORDER BY all work with zero new executor semantics. `joinRun.rec` grew a branch: virtual → iterate rows (no pushdown — tables are tiny; ON and the final WHERE still filter fully), real → existing planScan/scanPlan, both feeding one shared `next()` closure.

- **Resolution** (sql/syscat.go): `tableLookup func(name) (*TableDesc, [][]any)`; `(d *DB).lookup(base)` layers the registry under a real catalog (Engine.Table or Txn.Table — prepareFrom now takes the lookup, not the txn). User tables win bare names; `pg_catalog.x` / `information_schema.x` are canonical keys; bare pg_catalog members resolve (search-path semantics — bare `tables` does NOT hit information_schema, as in PG).
- **Registry**: pg_namespace (11/2200/13000), pg_class (tables relkind 'r' + indexes 'i' incl. synthetic `<t>_pkey`), pg_attribute (attnotnull = is-PK-column), pg_type (11 static rows: the five + int2/int4/float4/name/oid/varchar), pg_index (indisprimary row per table; indkey as space-joined 1-based attnums — real int2vector text form), information_schema.tables/columns (data_type "bigint"/"double precision"/...; udt_name int8/float8/...). **OID scheme**: table oid = engine table ID; index oid = tableID*1000 + indexID (pkey at *1000+0). Catalog does not describe itself (user tables only).
- **Read-only + reserved**: `sysWriteGuard(writeTarget(st))` in `run()` after coercion — DML/DDL against a system name or any dotted name (only system schemas survive the parser) → "system catalog is read-only". So `create table pg_class` is also blocked (no shadowing).
- **Weak consistency caveat**: virtual rows materialize from the Engine catalog at scope build, not the txn snapshot — fine for introspection, documented implicitly.

## Parser additions

- **`tableName()`**: optional schema qualifier at every table-name site (FROM, INSERT/UPDATE/DELETE, all DDL). `public.t` → `t`; pg_catalog/information_schema stay qualified; other schemas → "no such schema". Scope qualifier-matching uses the bare name (`pg_class.oid` works for `pg_catalog.pg_class` unaliased).
- **FROM is optional**: `SELECT 1` flows through prepareFrom with zero items → `rec` yields one empty combined row — no special case in the executor beyond `SELECT *` without FROM erroring.
- **Literal select items**: `SelectItem{IsLit, Lit, LitName}`; parser takes numbers/strings/±/true/false/null in select lists; `$n` in the select list is rejected with a clear error (bindParams doesn't walk items — rejecting beats silently ignoring; PG-compat deferred). Projection changed from `[]int` ordinals to `[]projEntry{ord, lit}` (ord -1 = literal); literal types via `litType`.
- **`sysFuncCall()`**: zero-arg whitelist folded to string constants at parse time (version/current_database/current_schema/current_user/session_user, optional `pg_catalog.` qualifier), wired into select items AND `predOperand` (so `WHERE table_schema = CURRENT_SCHEMA()` works). Args → "takes no arguments" error.
- **ORDER BY ordinals**: `ORDER BY 1, 2` in both non-agg (maps to projection entry; literal target = constant key, skipped) and aggregate (maps to `q.outputs[n-1]`) paths; non-integer constant and out-of-range errors match PG's wording spirit. GROUP BY ordinals NOT done.

## Gotchas hit

- `col` helper name collided with an existing test helper → `sysCol`. BSD sed has no `\b`; used perl.
- First rec() draft used a labeled `break` on an `if` — illegal Go; restructured to if/else with shared closure.
- **Disk full mid-test**: go-build cache was 11G; `go clean -cache` hit root-owned entries (someone ran sudo go in Apr 2025) — partial clean freed 10G, ~141M root-owned remain (user can `sudo rm -rf ~/Library/Caches/go-build`).

## Testing

- sql: `TestSelectWithoutFrom` (literals, version(), qualified fold, mixed literal+column), `TestSystemCatalog` (pg_class kinds, namespace join, attribute/type join with attnotnull, pg_index indkey "4 1", GORM HasTable probe verbatim, info_schema columns, GROUP BY over virtual rows, public.users, bare-name search-path rules, unknown schema), `TestSystemCatalogReadOnly` (7 write/DDL shapes), `TestOrderByOrdinal` (both paths + errors).
- pgwire: `orm_test.go` `TestClientProbes` — the same probes through real pgx: `select pg_catalog.version()`, `SELECT 1`, GORM HasTable **parameterized** ($1 + CURRENT_SCHEMA() folded), pg_catalog column introspection with $1, pg_index discovery, read-only error over the wire.
- psql manual: version(), catalog joins, information_schema — all render; `\dt` fails at `!~` (regex op) — the documented boundary.

## State at end of session

- Milestones 1–11 done, both modules green under `-race`. Deps unchanged (pgx still test-only in pgwire).

## Next (deferred)

- **Expression language** — what `\dt`/`\d` still need: CASE, IN (...), `~`/`!~` (regex), casts (`::regclass`), `<>` on... (have it), function calls with args (`pg_table_is_visible(oid)`, `format_type`, `pg_get_userbyid`), `= ANY(...)`. That's its own milestone; grep psql's actual queries first.
- GROUP BY ordinals; `$n` in select lists (needs bindParams to walk items).
- Wire: transaction blocks (BEGIN/COMMIT), cancellation, COPY, portal suspension.
- Engine: DESC key columns, CHECK/NOT NULL, savepoints, EXPLAIN.
- Someday: tag bytdb v0.x so pgwire's `replace` can become a versioned require.
