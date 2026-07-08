# Session: MkDocs site docs (architecture/features/gotchas/testing) + cross-DB benchmarks

- **Session ID**: `f7a7c05f-be9d-40a1-9a39-dc9c1a8ceaba`
- **Date**: 2026-07-08
- **Prior context**: fresh session; began with a Q&A on startup recovery (WAL replay), then built the docs site and benchmark harness.
- **State at end**: `mkdocs build --strict` green; benchmarks run with real numbers; **docs/bench files intentionally left uncommitted** (only this session doc committed) pending review.

## Part 0 — Q&A: how startup restores data

Two-layer answer, later reused as the recovery section of the docs:
1. **btypedb `Open`** (`db.go:174`): the DB file *is* the WAL. Replay every CRC-framed record into fresh COW trees; batch groups (opBatch header) apply all-or-nothing; first torn/CRC-fail record ends the valid log and the tail is truncated; leftover `.compact` temp deleted (never live). TTL records carry absolute deadlines so expiry reconstructs identically.
2. **bytdb `loadCatalog`** (`engine.go:304`): catalog rides along in the kv keyspace; open fails on any unreadable/future-version descriptor, collecting all offenders into one error.

## Part 1 — MkDocs site

- `mkdocs.yml`: Material theme (teal, light/dark toggle), superfences **mermaid** custom fence, admonition/details/highlight, 6-page nav. Build/serve via `uvx --with mkdocs-material mkdocs build --strict` (no local mkdocs install).
- `docs/` pages (10 mermaid diagrams total):
  - `index.md` — layer-stack diagram, embedded + bytdbd quick starts, project layout.
  - `architecture.md` — keyspace + catalog-as-data, tuple encoding (tag order, sign-flip ints, 0xFF-XOR desc), WAL record format, group-commit sequence diagram, **startup-recovery flowchart**, single-writer/COW txn model + savepoints + reentrancy guard, index entry forms (unique vs NULL-carrying), compaction two-pause flow, TTL, pgwire extended-protocol sequence + syscat emulation.
  - `features.md` — SQL surface with examples **quoted from passing tests** (file refs), 3-level Go API, wire-client compatibility list.
  - `gotchas.md` — RAM-resident sizing; single-writer + reentrancy trap; visibility-precedes-durability; sticky writeErr; unsupported-SQL table (DEFAULT/FK/RIGHT-FULL joins/CTE/RETURNING/date-decimal-uuid-json/EXPLAIN ANALYZE/window frames...); NOT-IN-with-NULL and text-vs-number semantics; DDL edges (NOT NULL add on empty only, dropped-column data lingers); engine indexes vs btypedb runtime indexes; TTL sweeper lag; WAL-growth/one-process/trust-auth ops notes.
  - `testing.md` — five-tier pyramid diagram; coverage table; how to run incl. fuzz.
  - `benchmarks.md` — results, durability-spectrum table, analysis, methodology + topology diagram, caveats, reproduction.
- `.gitignore` += `site/`, `bench/bench`.

## Part 2 — Test coverage (measured, `-count=1 -cover`)

| bytdb | sql | tuple | pgwire | btypedb |
|---|---|---|---|---|
| 83.9% | 83.2% | 91.1% | 87.2% | 83.1% |

281 test funcs, 4 fuzz targets (`FuzzTupleRoundTrip`, `FuzzTupleOrder`, `FuzzMessageParse`, `FuzzParse`); zero pre-existing `Benchmark*` funcs in either repo.

## Part 3 — Benchmark harness (`bench/`, new standalone module)

- Own module with `replace` to `../` and `../pgwire`; **nested `bench/go.work`** so the repo-root workspace neither includes bench deps nor breaks gopls (root go.work would otherwise reject the module — build with `GOWORK=off`).
- Six targets, one connection, sequential (latency not saturation): bytdb embedded SyncAlways / SyncEverySecond, bytdb over pgwire (in-process server, TCP loopback), Postgres 16.14 (Docker `--tmpfs` PGDATA, port 54329), DuckDB in-mem (`go-duckdb/v2`), Redis 7 (local, `--save '' --appendonly no`, port 63799).
- Workloads: 5k autocommit inserts · 100k batch (1k-row literal VALUES / redis pipeline) · 20k point reads · 5k point updates · 20× `count+sum` over 100k. Fixed seed. Emits markdown table + `results.json`.
- `run.sh` reproduces (Docker + redis-server required).

### Numbers (Apple M1 Pro, macOS, µs/op)

| target | ins_pt | ins_batch | read_pt | upd_pt | scan_agg |
|---|---:|---:|---:|---:|---:|
| bytdb SyncAlways | 4221.6 | 6.5 | 4.2 | 4571.4 | 31153 |
| bytdb SyncEverySecond | 16.8 | 1.9 | 4.0 | 22.8 | 30601 |
| bytdb pgwire | 50.9 | 2.0 | 35.9 | 60.4 | 31021 |
| postgres tmpfs | 173.8 | 3.4 | 199.6 | 212.3 | 4837 |
| duckdb mem | 161.2 | 6.3 | 76.2 | 118.0 | 387 |
| redis | 31.2 | 1.2 | 30.4 | 29.1 | — |

Headlines: embedded reads ~7× Redis (no wire); pgwire ≈ Redis, ~5× tmpfs-Postgres; SyncAlways solo commit = 4.2 ms is macOS `F_FULLFSYNC` (the only power-cut-surviving config in the lineup — tmpfs PG/DuckDB-mem lose everything, Redis loses minutes); DuckDB wins the aggregate 80× (columnar vs our row-at-a-time interpreter). Docker-VM hop inflates PG point ops on macOS — say so in docs.

## Next steps / open items

1. **Docs + bench are uncommitted** — review then commit (`mkdocs.yml`, `docs/`, `bench/`, `.gitignore`).
2. Consider CI: `mkdocs build --strict` + gh-pages deploy.
3. `scan_agg` (31 ms/100k) is the weak spot — batched expression eval or a tighter scan loop would narrow the PG gap; DuckDB-class is out of scope.
4. Maybe add concurrent-client benchmark variant (group commit should shine for SyncAlways).
5. Benchmarks doc claims Linux fsync ≪ macOS F_FULLFSYNC — could verify on a Linux box and add a column.
