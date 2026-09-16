# MIT License and release v0.15.0 (root + pgwire)

- **Session:** `e00be207-b6be-4f1d-b288-f4721782d433`
- **Date:** 2026-09-16
- **Scope:** add an MIT License, then tag v0.15.0 for the root module and
  pgwire, which also closes Next item 4 of
  `2026-0916-1557-sqlstate-gaps-constraint-catalog`.

## What was done

### 1. MIT License — `62bac68`

- `LICENSE` at the repo root: standard MIT text, "Copyright (c) 2026 Rohan
  Allison" (year of the first commit, 2026-07-03).
- `pgwire/LICENSE`: identical copy. **Why:** pgwire is its own Go module,
  and a module zip only contains files under its own directory. Without a
  local copy pkg.go.dev reports "no license detected" for pgwire and can
  hide its docs.
- `bench/` got no license: it is a benchmark harness, not an importable
  library.
- `README.md`: new `## License` section at the end, linking `LICENSE`.

### 2. Release v0.15.0

Minor bump (not patch) because the changes since `v0.14.0` add features:
SQLSTATE gap mappings and the `information_schema` constraint tables
(`509a735`), plus the license.

Steps, following the v0.14.0 lockstep pattern:

1. `go vet ./...` plus `go test ./...` in root and `pgwire/`: all green.
2. Annotated tag `v0.15.0` on `62bac68`, pushed. The module proxy resolved
   `github.com/rohanthewiz/bytdb@v0.15.0` right away.
3. `pgwire/go.mod`: `github.com/rohanthewiz/bytdb v0.14.0 -> v0.15.0`
   (only `go.mod` changes, because the `replace => ../` keeps local builds
   on the working tree). The pgwire build and tests were green again.
   Committed as `383802b`, pushed.
4. Annotated tag `pgwire/v0.15.0` on `383802b`, pushed.

Tag lines are now: root `v0.12.0 -> v0.13.0 -> v0.14.0 -> v0.15.0`, pgwire
the same in lockstep (v0.10/v0.11 gaps still unbackfilled by decision).

## Next

*Legend: **age** = session docs since the item was first written down,
counted from this doc (0 = raised here). **value** = payoff, not effort:
**high** means something is worked around today; **medium** means it blocks
one named thing; **low** means nobody has run into it yet or it depends on
something that does not exist.*

1. **btypedb encryption deferred set** **(age 30 · value low)**: online key
   rotation, plaintext↔encrypted migration helper, key+value scope,
   ChaCha20-Poly1305. First written `2026-0722-1303`. Still absent —
   `btypedb/encrypt.go:41` reserves flag bits 1..4, `compact.go:139` names
   the re-encrypt seam. Rotation needs a v3 wrapped-DEK header first.
2. **`pg_constraint` omits primary keys and unique constraints** **(age 1 ·
   value low)**: it lists only CHECK and FK (keys surface via `pg_index`),
   while `information_schema.table_constraints` lists all four. Tools that
   read `contype IN ('p','u')` would miss keys; nobody has reported it.
3. **`bad parameter format code` maps to XX000** **(age 1 · value low)**:
   Postgres treats an invalid Bind format code as a protocol violation
   (08P01). This predates the SQLSTATE work.

Closed this session: *Release the SQLSTATE and catalog changes* (tagged
`v0.15.0` / `pgwire/v0.15.0`).

Read by value instead: **high** none · **medium** none · **low** 1, 2, 3.

### Deliberate non-goals (declined, not dropped)

- **License file in `bench/`.** Internal harness module; not imported.
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
