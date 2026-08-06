# SELECT DISTINCT: qualified ORDER BY columns now resolve against the select list

Session: e9472edf-6fe6-4842-b363-0db5225c8141
Date: 2026-08-05

## What happened

An agent using bytdb reported: "bytdb rejects a qualified ORDER BY
under SELECT DISTINCT — `ORDER BY n.updated_at` fails even though
`n.updated_at` is selected." Confirmed as a real bug and fixed.

## Root cause

`outputSortKeys` (sql/exec.go) resolves DISTINCT's ORDER BY against
the materialized output columns and only accepted bare names
(`o.Col.Table == ""`) or positions. Any table qualifier fell into the
default arm and errored with "for SELECT DISTINCT, ORDER BY
expressions must appear in select list" — even for
`SELECT DISTINCT n.updated_at ... ORDER BY n.updated_at`.

Postgres accepts these: a qualified name and a selected bare column
resolve to the same column reference, so the qualified form IS in the
select list. bytdb was stricter than the rule its comments cited.
`TestSelectDistinctErrors` even asserted the wrong behavior
(`order by emp.dept` was in the must-fail list).

## Fix (sql/exec.go)

New `resolveQualifiedOrder(s, cols)` runs in `execSelectDistinct`
before `outputSortKeys` and rewrites a qualified ORDER BY column into
a select-list position (`IsLit` int64) when it provably denotes a
selected column. Two proofs, tried in order:

1. **Items aligned** (`!s.Star && len(s.Items) == len(cols)`, i.e. no
   star expansion): match a plain-column select item that is either
   the identical `ColRef`, or the same bare name when the query's
   single FROM binding (alias if present, else table name) equals the
   qualifier — a bare name can only have bound to that table.
2. **Star shapes** (`SELECT *` / `t.*`, items absent or misaligned):
   qualifier must equal the single FROM binding; then match output
   column names directly, safe because every output column comes from
   that one table.

Anything unproven passes through unchanged, so:

- genuinely unselected columns still error (`order by emp.sal` when
  only dept is selected),
- alias shadowing can't hijack a qualified ref
  (`select distinct sal as dept ... order by emp.dept` still errors —
  qualified names never match output aliases, per Postgres),
- multi-table star shapes (`SELECT DISTINCT * FROM a, b ORDER BY a.x`)
  stay strict: without per-column provenance the qualifier can't be
  validated.

The UNION ORDER BY path (the other `outputSortKeys` caller) was left
untouched — Postgres also rejects qualified names in set-op ORDER BY.

## Verification

- New `TestSelectDistinctQualifiedOrder` (sql/distinct_test.go):
  qualified/bare select items, table aliases, DESC, `SELECT DISTINCT *`
  with qualified keys, and a self-join where the qualifier
  disambiguates arms. Error tests updated: `emp.dept` moved to the
  success side; added unselected-qualified and alias-shadowing
  must-fail cases.
- `go test ./...` green across all modules.
- End-to-end via the /verify skill: built `pgwire/cmd/bytdbd` against
  a scratch db, drove it with a pgx client — the reported query shape
  passed over the simple protocol, the alias-qualifier variant passed
  over the extended protocol with a `$1` binding, and an unselected
  qualified column was still rejected with the Postgres-style error.

## Files touched

- sql/exec.go — `resolveQualifiedOrder` + comment updates on
  `execSelectDistinct`
- sql/distinct_test.go — new test func, error-list updates
