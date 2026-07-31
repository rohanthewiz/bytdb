# MkDocs documentation refresh: two new pages, 22 validated mermaid diagrams

- **Session ID:** `59f8afd2-f334-4104-a185-152f638a155f`
- **Date:** 2026-07-30
- **Scope:** User asked for "mkdocs style docs" with detailed mermaid diagrams
  (they read them with their own gkdocs reader, so no local mkdocs needed).
  A `docs/` tree + `mkdocs.yml` already existed but was badly stale — it
  predated foreign keys, CTEs/views, TRUNCATE, the new column types, SCRAM/TLS,
  WAL encryption, and S3 replication. This session refreshed all six existing
  pages against current code and added two new ones.

## Deliverables

| File | Change |
|---|---|
| `mkdocs.yml` | Nav gained Replication & Backup and Encryption & Security |
| `docs/index.md` | Rewritten: system diagram now includes `replicate` + encrypted file + S3; doc map covers new pages |
| `docs/architecture.md` | New SQL-layer section (pipeline + planner decision-ladder flowcharts), foreign-keys section, replication-hooks section; keyspace table corrected (views = table ID 3, sequence objects at ID 2 index 3); encryption woven into WAL/recovery/compaction diagrams; wire section updated for TLS/SCRAM/BackendKeyData |
| `docs/features.md` | Rewritten: 10-type column table (VARCHAR(n) enforced), FK section with ON DELETE CASCADE resolution flowchart, join-strategy decision diagram (index NL vs hash join), CTEs/derived/views, jsonb operator family, text[], LIKE/ILIKE/BETWEEN, TRUNCATE/SET/SHOW/RENAME, parameterized LIMIT/OFFSET. Window-function sections kept verbatim (verified still accurate) |
| `docs/replication.md` | **New**: ship-cycle sequence diagram, generation-lifecycle state diagram, two-pass restore flowchart, failure-mode matrix, Backup/BackupTo |
| `docs/security.md` | **New**: encrypted-record format diagram, HKDF key-derivation diagram, fail-fast open flowchart (5 key errors), SCRAM sequence diagram w/ channel-binding downgrade detection, timeout table, full bytdbd flag reference |
| `docs/gotchas.md` | Rewritten: stale not-supported rows removed (FKs, CTEs, views, types, SSL, `$n` LIMIT); current gaps listed (WITH RECURSIVE, DISTINCT ON, jsonpath, COPY, non-text arrays…); new divergences (timestamp ≡ timestamptz, json → jsonb canonicalization, LIKE ESCAPE backslash-only); new "Performance edges" section (index FK columns; views materialize per statement; hash-join type compatibility) |
| `docs/testing.md` | Fresh numbers: **561 test functions, 11 fuzz targets** (was 281/4); per-package coverage from a live run incl. replicate (80.4%) and replicate/s3 (89.4%); fuzz-target table mapping each to its invariant |

## Method

- Three parallel Explore agents produced source-verified reports (file:line):
  replication internals (`replicate/`, btypedb `LogState`/`ReadLogRange`/epoch),
  encryption + wire auth (`encrypt.go`, `pgwire/auth.go`, bytdbd flags), and
  the newer SQL features (fk.go, views.go, jsonb_ops.go, hash joins, etc.).
- Coverage run (`go test -count=1 -cover ./...` across all three modules) ran
  in the background — all green.
- **All 22 mermaid blocks validated** with the real mermaid v11 parser
  (npm mermaid + jsdom in the scratchpad). One class of bug found and fixed:
  literal `<placeholder>` tokens in sequence-diagram messages parse as HTML
  tags — replaced with `⟨…⟩`.

## Facts pinned down during exploration (useful later)

- Reserved system tables: `0` next-table-ID, `1` descriptors, `2` sequences
  (identity counters + `(name)->SeqDesc` at index 3), `3` views (`view.go`,
  `seqobj.go`, `engine.go:34-36`).
- Restore picks the newest generation whose contiguous-from-zero chunk chain
  reaches `manifest.Size`; all-manifests-short → `ErrIncompleteReplica`; the
  chain may validly extend past the certified size (torn tail).
- FK enforcement is planner-driven — no index requirement on child columns,
  so unindexed FKs scan the child table per check (documented in gotchas).
- Hash join requires: equality conjunct, hash-compatible types (numeric class
  or identical), and no index/PK serving the equality; CTE/view/derived joins
  always qualify.
- `ReadLogRange`/`BackupTo` hold only `compactMu` — chunk size bounds the
  max compaction stall (`MaxChunkBytes`, default 8 MB).

## State

- Docs changes + this session doc committed and pushed (see repo log).
- Prior pending items unchanged: bytdb.Update recover, pgwire DoS follow-ups,
  replicate restore-completeness (from the 2026-07-22 sessions).
