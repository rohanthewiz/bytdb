# Session: bytdb-milestone-12-expression-language

- **Session ID**: `f4645ab6-7501-41fd-84da-f9837febd322` (continues `2026-0704-1006-bytdb-milestone-11-system-catalog.md`)
- **Date**: 2026-07-04
- **Repo**: `~/projs/go/bytdb`
- **Result**: milestone 12 complete — general expression language + UNION + catalog growth; psql 17.5's `\dt`, `\d`, `\d <table>`, `\d <index>`, `\di`, `\dn`, `\du`, `\dt+`, `\d+`, and `\l` all render against a live session. Both modules green under `-race`. Commit `0da76f0`.

## Design: one grammar, lowered; lazy eval through an env chain

Three load-bearing decisions:

1. **One expression grammar for everything** (parser.go): OR < AND < NOT < comparison postfixes (IS NULL / [NOT] IN / `COLLATE` swallowed / one cmp op, RHS may be `ANY(...)`) < `+ - ||` < `* / %` < unary sign < `::cast` and `[subscript]` postfixes < primary (literal/param, `(expr)` or `(SELECT ...)` → ExSub, CASE, aggregate call, function call incl. schema-qualified, colref). WHERE/ON/HAVING and select items all parse through it. **Lowering** (`lowerBool`/`lowerItem`) maps simple shapes back to legacy `Pred`/`SelectItem` — pushdown, static coercion, and param inference keep working untouched — and wraps the rest in a new BoolExpr leaf `Cond{Ex}` or an expr select item (`SelectItem.Ex`).
2. **Eval-time name resolution through `exEnv`** (expr.go): `{d, tx, sc, row, outer}`; a column that fails in the local scope falls back outer — that *is* the correlation mechanism, no bind-time machinery. Corollary: unknown functions/columns inside never-reached branches never error, which is exactly why psql's publication/policy/statistics queries (generate_series in FROM, `prattrs[s+1]`, string_agg, `::int2[]`) run: they parse, and zero-row joins mean nothing evaluates. Set-returning functions in FROM parse (`FromItem.FuncArgs`) and error only in `buildScope`.
3. **Subqueries decorrelate at eval**: `evalSubquery` builds the sub-scope, rewrites any `Pred` whose columns don't resolve locally into `Cond` (predExpr), and runs the ordinary `prepareFrom`/`runJoin` with `outer: env`. Scalar (0 rows → NULL, >1 → error, Limit/Offset honored), single-aggregate (`(SELECT count(*) ... WHERE x = outer.y)` folds through one `accum`), `EXISTS` (first row wins), `ARRAY(SELECT ...)` (collects → `{a,b}` text; sorts if any ORDER BY). ARRAY/EXISTS are special-cased in the ExFunc eval path when the single arg is an ExSub.

## Executor integration

- `evalBool`/`evalPreds`/`checkPred`/`plan.matches` now thread `(tri, error)`; `evalPreds`/`runJoin`/`scanPlan` take an env (nil where the tree provably holds no Conds — joinRun's pushed conjuncts are always plain Preds). UPDATE/DELETE build a single-table env (`tableEnv`) since their WHERE may hold Conds (IN, regex on non-simple, subqueries).
- **Expr select items**: evaluated at collect time and appended past `sc.width` on each combined row; projection entries just point there, so ORDER BY ordinals/aliases and the post-sort projection needed no new machinery. ORDER BY general exprs append as hidden sort columns; bare ORDER BY names check output aliases first (`aliasEntry`).
- **checkPred got dynamic string adaptation** (string vs number/bool re-reads via coerceLit) — covers positions static coercion can't see (subqueries, expr internals) and made `id IN ('2')` work. Regex ops live in checkPred with a `sync.Map` compile cache (`(?i)` prefix for `~*`).
- **UNION [ALL]** (`execUnion`): arms execute via `runSelectCore` (agg or not per arm), concatenate left-to-right, distinct arms dedup the accumulated set via `tuple.Encode` keys; ORDER BY on the union takes ordinals or output names only; LIMIT/OFFSET after. Head `Select` carries `Union []UnionArm`; coerce/params/describe recurse into arms (describe shape = arm 1).
- `ExArith` short-circuits on NULL left operand (all ops are NULL-strict) — required so `c.reloptions || array(select ... unnest(...))` skips the unevaluable right side on real pg_class rows.
- params.go: bindParams deep-clones exprs/subqueries/arms (`bindExpr`/`bindSelect`); numParams walks item lits, Cond exprs, ExSub selects. **Params now work in select lists** (`SELECT $1`) — the old rejection is gone.

## Catalog growth (syscat.go)

- pg_class += relam/relchecks/relhasrules/relhastriggers/relrowsecurity/relforcerowsecurity/relispartition/reloftype/reltablespace/relreplident/reltoastrelid/reloptions(nil); relam = 2 (heap) for tables, 403 (btree) for indexes; pg_am populated with those two rows (this is what makes `\d <index>` return rows — its query inner-joins pg_am).
- pg_attribute += atttypmod/attcollation/attidentity/attgenerated/attstorage/attcompression/attstattarget, **plus rows for index relations** (key columns, attrelid = index oid) so `\d <index>` lists columns.
- pg_index += indisclustered/indisvalid/indisreplident/indnullsnotdistinct/indimmediate/indcheckxmin/indnkeyatts/indpred(nil); pg_type += typcollation(0); pg_namespace += nspowner.
- Populated: pg_database (1 row, for `\l`), pg_roles (1 row "bytdb", for `\du`). Empty probes: pg_attrdef, pg_collation, pg_constraint, pg_inherits, pg_policy, pg_statistic_ext, pg_publication{,_rel,_namespace}, pg_auth_members. `sysTableDef.rows` may be nil (always empty).
- Functions (expr.go `evalFunc`): format_type (typmod-aware varchar), pg_get_indexdef (renders `CREATE [UNIQUE ]INDEX ... USING btree (...)`; col-number form returns the column name), pg_get_userbyid, pg_table_is_visible, pg_relation_is_publishable, pg_get_expr/constraintdef/etc → NULL, array_to_string/array_length → NULL, pg_encoding_to_char, pg_size_pretty/pg_table_size, coalesce/nullif/upper/lower/length. `'users'::regclass` resolves via the engine catalog to the table oid.
- Lexer: `~ !~ ~* !~* :: [ ] || / %` operators and `E'...'` escape strings (the E-check sits before the ident branch).

## Behavior changes to note

- `WHERE 1 = 2` (literal-literal) is legal now → Cond; old parse error removed (ORMs' `WHERE 1=1`). `select frim t` parses as `SELECT frim AS t` (like PG) — pgwire's TestErrors switched to `select * frim t` for its syntax-error case. `a = -$1` is now a general expression, not an error.
- Aggregate queries still reject expr items/Cond-in-HAVING with clear errors; GROUP BY ordinals still not done.

## Iteration protocol that worked

`psql -E` against bytdbd echoes each hidden query client-side even when the server errors — so the loop was: run `\d ...`, read the first failing query, add the column/table/function/grammar bit, rebuild, repeat. Query shapes are pinned by the `server_version` pgwire reports ("16.0 (bytdb)") — psql 17.5 uses its v16 paths, including the three-arm publication UNION on every `\d <table>`.

## Testing

- sql: `expr_test.go` (CASE both forms + alias ordering, IN/NOT IN incl. type adaptation, regex + OPERATOR() + compile error, arithmetic/casts/functions/E-strings, NULL strictness, `'users'::regclass`, `1=1`/`1=2`, scalar/EXISTS/ARRAY/correlated-count subqueries, >1-row error, UNION distinct/all/alias-order/agg arms/arity error, UPDATE/DELETE with IN and regex, params in exprs and select lists incl. Prepare). `psql_test.go`: the verbatim psql 17 queries — \dt, \d name resolution, relation details, columns (two correlated subselects), index listing (IN inside LEFT JOIN ON), publication UNION, \d index (EXISTS + indnkeyatts subquery), \du (ARRAY over pg_auth_members), \l (E'\n').
- pgwire: `TestPsqlBackslashQueries` — \dt + name resolution + publication UNION through real pgx.
- Manual psql 17.5: all backslash commands listed in Result; plus user-data exercises (correlated counts, unions, regex, CASE bands).

## State at end of session

- Milestones 1–12 done, committed (`0da76f0` + session-doc commit), pushed to origin/main. Deps unchanged.

## Next (deferred)

- GROUP BY ordinals/expressions; expr items in aggregate queries; `= ANY(array)` for real.
- Wire: transaction blocks (BEGIN/COMMIT), cancellation, COPY, portal suspension.
- Engine: DESC key columns, CHECK/NOT NULL, savepoints, EXPLAIN.
- Someday: tag bytdb v0.x so pgwire's `replace` can become a versioned require; `\dp`/`\z` (needs aclexplode) if anyone cares.
