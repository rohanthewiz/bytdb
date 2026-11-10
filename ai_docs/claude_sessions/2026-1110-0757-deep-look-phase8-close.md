# Deep-look plan: Phase 8 close (D-57, D-58), Phase 9, repro cleanup

Session: `2c5b1603-fa95-4c81-b037-0bcdc19533ac` (continues
`2026-1110-0737-deep-look-phase8-d51-d56`).

## What landed

- **D-57, database/sql driver** (`86f59c7`). `[]string` -> `text[]` and
  `[16]byte` -> `uuid` arguments; `date` scans as `time.Time` (midnight UTC,
  pgx/lib/pq parity); DSN options `encryption_key_file`, `memory_ceiling`,
  `max_result_rows`, `max_txn_writes`, `auto_compact`, `compact_min_bytes`,
  `compact_growth_pct`; `:memory:`; symlink-resolved engine sharing;
  redacted DSN errors. Key decoding and memory-ceiling parsing moved into the
  engine so bytdbd and the DSN share them.
- **D-58 part 1** (`400f51d`). `bytdbd -replicate-s3`, `-restore-from`
  (restore only when `-db` is missing), `-s3-endpoint/-region/-virtual-host`,
  `-replicate-interval`; credentials from `AWS_*` env only. `/healthz` and
  replication gauges on the metrics listener.
- **D-58 part 2** (`71cd5a2`). SIGHUP reloads `-auth-file` and the TLS key
  pair: `pgwire.Server.SetTLSConfig` (atomic state; each connection's
  SCRAM-PLUS binding follows the certificate it was served) and
  `Credentials.ReplaceFrom`.
- **Phase 9** (`2388d34`). `FOR UPDATE` per write mode in
  `docs/concurrency.md`; the bytdb skill updated for the Phase 8 surface. The
  rest of the table had landed with its items.
- **D-01 cleanup.** `ai_docs/todo/deep-look-repros/` deleted after running
  every module against main: all pass, except two repros whose shapes the
  fixes refuse or whose expectations contradicted Postgres (ported tests pin
  the real behavior). Recoverable from `2388d34`.

## Decisions

- **`date` scans as `time.Time`** (the plan's D-57) even though an earlier
  test had pinned the text form: pgx and lib/pq do the same, and scanning
  into a `string` yields RFC 3339 there too. Recorded for the release notes.
- **`-restore-from` only restores a missing file**, so one command line both
  rebuilds a lost machine and restarts an existing one; it never overwrites.
- **`/healthz` turns unhealthy only after three failed intervals**, not one:
  a single failed tick is the replicator's retry loop working.
- **SIGHUP reloads only what was configured at startup**; it never turns auth
  or TLS on or off under connected clients.
- **No new dependencies for tests**: bytdbd's replication tests use an
  in-memory `replicate.Storage`.

## Next

Closed: N-043. Declined: None. Raised: N-049. Deferred: None. Promoted: None.
Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
