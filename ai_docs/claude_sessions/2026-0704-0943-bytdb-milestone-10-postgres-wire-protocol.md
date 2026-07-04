# Session: bytdb-milestone-10-postgres-wire-protocol

- **Session ID**: `8094c5d6-d2b3-472e-9f32-38e5583d85a9`
- **Date**: 2026-07-04
- **Continues**: `2026-0704-0842-bytdb-milestone-9-prepared-statements-and-errtext.md` (its deferred list led with this milestone)
- **Repo**: `~/projs/go/bytdb`
- **Result**: milestone 10 complete — the `pgwire` nested module serves bytdb over PostgreSQL protocol 3.0; verified end-to-end with real pgx v5 (extended + simple protocols, statement cache, stdlib `database/sql`) and an interactive psql session. All green under `-race` in both modules.

## sql package additions (prerequisites the wire protocol forced)

- **`Stmt.Describe() (*StmtInfo, error)`** (new sql/describe.go): what Parse/Describe need without executing — `Command` (tag word), `Params` (one inferred `bytdb.ColType` per `$n`: the column each placeholder is compared against / inserted into / assigned to; HAVING against COUNT→int, AVG→float, SUM/MIN/MAX→arg type; first position wins on conflict; unused $n stays `""`), and `Cols`/`Types` (SELECT output shape from the catalog — non-agg via new shared `projectSelect`, agg via existing `resolveAgg`+`resultCols`). Errors where execution would (unknown table/column), which lands those errors at wire-Parse time like real Postgres. Also `Stmt.Command()` (catalog-free).
- **Refactors for it**: `buildScope` now takes a `func(string) *bytdb.TableDesc` (callers pass `tx.Table` or `Engine.Table`); `execSelect`'s projection factored into `projectSelect(sc, s, res)`.
- **Untyped-literal coercion** (new sql/coerce.go, hooked in `run` after `bindParams`): `WHERE id = '2'` now compares as integer 2, per Postgres untyped-literal semantics. Forced by a real-world discovery: **pgx ≥5.10's simple-protocol sanitizer renders EVERY argument as a quoted string literal** (`id =  '2' `, post-CVE hardening), so without coercion simple-mode pgx matched zero rows. The pass clones what it changes (prepared trees stay pristine — tested), is **best-effort on resolution** (unknown table/column leaves values alone so executor errors stay canonical), and only surfaces conversion failures: `invalid input syntax for type int (value: abc)`. Covers INSERT rows, UPDATE SET, WHERE/ON/HAVING; bytea accepts `\x` hex or raw; coerced literals reach the planner, so a quoted PK literal still plans a point get (tested).

## pgwire module (`pgwire/`, own go.mod)

- Module `github.com/rohanthewiz/bytdb/pgwire`, `replace github.com/rohanthewiz/bytdb => ../`; root `go.work` added (mainly so gopls resolves the nested module — CLI builds don't need it). Runtime deps: bytdb + serr only; **pgx v5 is test-only**.
- **Files**: pgwire.go (Server: NewServer/Serve/ListenAndServe/Close, conn tracking, goroutine per conn), conn.go (session: startup + both protocols), proto.go (framing; `rbuf` latching reader / `wbuf` builder), values.go (OIDs bool 16/bytea 17/int8 20/float8 701/text 25; text+binary encode/decode; client-declared int2/int4/float4 widen; unknown OIDs: text passes through as string — coercion catches it downstream — binary refused), errors.go, split.go (top-level `;` splitter honoring quotes/`--`/nested `/* */`), cmd/bytdbd (flags -db, -addr).
- **Startup**: SSLRequest/GSSENC → bare `'N'` then loop (default `sslmode=prefer` works); CancelRequest → close; trust auth (AuthenticationOk immediately); ParameterStatus incl. `server_version 16.0 (bytdb)`; BackendKeyData (counter pid, secret 0); ReadyForQuery always `'I'` (autocommit only).
- **Simple 'Q'**: split → per statement `Prepare`+`Exec`; RowDescription (text) from `res.Cols/Types`; first error stops the buffer; error state self-clears (no Sync needed in simple protocol). Empty buffer → EmptyQueryResponse.
- **Extended**: Parse → `Prepare`+`Describe` eagerly, stores client param OIDs (nonzero overrides inference for decoding); Bind → decode per format code + resolved OID, exact arity check, nil for wire NULL; Describe 'S' → ParameterDescription + RowDescription/NoData, 'P' → portal formats; Execute → DataRows + CommandComplete (`SELECT n` / `INSERT 0 n` / `UPDATE n`; maxRows/portal suspension ignored); Close/Flush/Sync; on error discard-until-Sync per protocol. Empty statement → NoData/EmptyQueryResponse.
- **Errors structurally** (as planned last session): `serr.UserFields()` → `pos` byte offset converted to 1-based **rune** offset for Position (`base` param handles multi-statement buffers — psql renders the `LINE 1: ... ^` caret correctly, verified), remaining fields → DETAIL `k: v, ...`, message → Message. SQLSTATE from stable message texts: pos→42601, "no such table"→42P01, "no such column"→42703, ambiguous→42702, "duplicate primary key"/"unique index violation"→23505, param arity→08P01, else XX000.

## Testing / verification

- sql: `TestDescribe`/`TestDescribeErrors` (inference incl. join-ON prefix scopes, agg typing, gaps/repeats), `TestLiteralCoercion` (all clauses, planner pushdown of coerced literal, prepared-tree immutability, invalid-syntax errors, text columns untouched).
- pgwire unit: splitter, value round-trips both formats, client-type widening.
- pgwire e2e through real pgx v5.10: `TestExtendedProtocol` (default sslmode=prefer handshake, statement cache re-use, binary binds/results, NULLs, tags), `TestJoinAndAggregate` (join + HAVING param over the wire), `TestErrors` (PgError.Code/Position/Detail; conn usable after errors), `TestSimpleProtocol` (multi-statement buffer, quoted-literal args, mid-buffer error stops rest), `TestDatabaseSQL` (stdlib adapter, db.Prepare).
- Manual psql session against `bytdbd`: DDL, multi-row insert, selects, quoted-literal where, update, aggregates, syntax error with caret, session continues.

## State at end of session

- Milestones 1–10 done. Root module still zero new deps (serr/btypedb only); pgwire runtime deps = bytdb + serr, pgx test-only. go.work + go.work.sum committed.

## Next (deferred)

- Wire: transaction blocks (BEGIN/COMMIT — needs SQL-layer support first), cancellation keys, portal suspension, COPY, `pg_catalog` (the ORM long tail: SQLAlchemy/ActiveRecord/Prisma introspect it on connect).
- Engine: DESC key columns (byte inversion), CHECK/NOT NULL, savepoints, EXPLAIN (plan struct is ready to print: get/index/from/stops/filter).
- Someday: tag bytdb v0.x so pgwire's `replace` can become a versioned require.
