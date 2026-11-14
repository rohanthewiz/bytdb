# Next list

The project's single list of open follow-ups. Sessions **edit this file in
place**. They don't copy a list forward into each session doc. A session doc's
`## Next` becomes a short summary instead: `Closed: N-… · Raised: N-…`.

Seeded 2026-09-24 by `/next-list seed` from the 15 session docs
`2026-0805-1603` → `2026-0916-1611`, with each item's `raised` traced back
through all 74 docs.

## Conventions

- **IDs are permanent** (`N-001`, `N-002`, …) and never reused, even after an
  item closes.
- **`raised`** is the stem of the session doc where the item first appeared
  (`ai_docs/claude_sessions/<stem>.md`).
- **Age is computed, never stored**: the number of session docs written after
  `raised`. `/next-list` works it out from the directory listing.
- **Value** measures payoff, not effort:
  - **high**: something is being worked around today, or a second independent
    consumer has arrived.
  - **medium**: it blocks one named thing, or it is a visible defect that
    nobody has to route around yet.
  - **low**: a gap nobody has hit, or it depends on something that doesn't
    exist yet.
- **Open** holds what we plan to pick up next. **Roadmap** holds what we want
  to do later, not soon. **Non-goals** holds what we probably won't do.
- **Nothing leaves Open or Roadmap without a line in another section.** A
  finished item moves to Closed with its date and the evidence. A declined
  item moves to Non-goals with the reason. Merged items go to Closed as
  `merged into N-xxx`. Moving an item between Open and Roadmap is fine.
- Open and Roadmap stay in ID order.

**Next ID:** N-055

## Open

N-045 to N-047 and N-049 come from one audit and share one plan,
`ai_docs/todo/2026-1109-deep-look-remediation-plan.md`; the `D-xx` numbers
are that plan's item IDs. The user asked for the plan to be implemented as
written, so its six decisions (Q1–Q6) are taken at the plan's
recommendations. Every phase of it (0–9) is on main and released in
`v0.23.0` (N-044, closed); what remains is the pieces deliberately deferred
(N-046, N-047, N-049; N-045 was declined after measuring, see Non-goals).

- **N-046** · raised `2026-1110-0555-deep-look-phase7-part2-phase8-start` · value low
  **Shared read snapshot for one-shot reads (D-46).** `Engine.Get` opens and
  releases a btypedb snapshot per call, both under btypedb's mutex, so
  concurrent one-shot reads do not scale. Reusing one refcounted snapshot per
  commit version needs btypedb to guarantee concurrent reads of one snapshot
  and ships with a btypedb release. Documented workaround: `ReadTxn`.
- **N-047** · raised `2026-1110-0555-deep-look-phase7-part2-phase8-start` · value low
  **Remaining Phase 7 pieces.** D-47 second half (memoize catalog rows per
  catalog version, excluding live tables such as `pg_stat_activity` and
  sequence state, or push `table_name = 'x'` into the builders); D-45's
  identity-counter batching (keep default-mode counters in the `Txn` until
  commit) and lazy counter names.

- **N-049** · raised `2026-1110-0757-deep-look-phase8-close` · value low
  **Capture GORM v2, golang-migrate and atlas statements verbatim.**
  `pgwire/orm_verbatim_test.go` holds goose, lib/pq and pgx statements copied
  from their sources; the other three tools were not in the module cache, and
  the plan asked for captured statements, not remembered ones. With network
  access, read each tool's Postgres driver/dialect source (golang-migrate's
  `database/postgres`, GORM's `gorm.io/driver/postgres` migrator, atlas's
  inspect queries), copy the connect- and migration-time SQL into the same
  test file, and fix whatever fails.

- **N-050** · raised `2026-1111-1759-numeric-type-n048` · value low
  **Small-coefficient fast path for numeric (plan Q2).** `decimal.Decimal`
  holds a `*big.Int` even for money-sized values. After this session's
  cheap wins (shared powers of ten, allocation-free `Cmp`, numeric
  mantissa decode, in-place `decimal.Sum`), `sum`+`avg` over 100k
  `numeric(10,2)` rows is 18 ms against 11.7 ms for float8, and a
  cross-scale filter 18 ms (`sql/numeric_bench_test.go`). An `int64`
  coefficient for values that fit would remove most remaining
  allocations. Measure a real workload before starting.
- **N-051** · raised `2026-1111-1759-numeric-type-n048` · value low
  **numeric `NaN` and `±Infinity`.** Refused today (0A000 over the wire,
  plan Q7). Postgres has them; adding them means special cases in every
  arithmetic, comparison and aggregate path plus two tuple classes (the
  encoding's class byte has room: 0x01/0x02/0x03 are used). Trigger: a
  dump or a client that writes one.
- **N-052** · raised `2026-1111-1759-numeric-type-n048` · value low
  **Exact numeric transcendental functions and `round(int)`'s type.**
  `sqrt`, `exp`, `ln`, `log`, `power`, `cbrt` convert a numeric argument
  to float8 and return float8, where Postgres returns numeric with its
  own scale rules (`sqrt(16.0)` = `4.000000000000000`). Separately,
  `round/ceil/floor/trunc/sign` of an integer return bigint here and
  double precision in Postgres (values agree). Both are documented in
  `docs/gotchas.md`.
- **N-053** · raised `2026-1111-1759-numeric-type-n048` · value low
  **The rest of N-048's refused types: `interval`, `money`, `bit`,
  `citext`.** Still "unknown column type". `money` maps naturally onto
  `numeric(p,2)` now; `citext` onto text with case-insensitive
  comparison; `interval` is the largest (a new value kind with calendar
  arithmetic). No consumer has asked.

## Validate

- **N-033** · raised `2026-1104-1337-bytdb-cli-shell` · value low
  **Run the `bytdb` shell interactively on Linux and Windows terminals.**
  Interactive mode (liner line editing, raw/cooked terminal handoff
  around `.shell` / `.edit`, Ctrl-C cancelling a running query, Tab
  completion, history, width fitting) was exercised through a pty on
  macOS only. Linux and Windows were cross-compiled, never run. Check
  each: prompt editing, Ctrl-C mid-query, `.shell`, `.edit`, and that the
  terminal is sane after exit.

## Roadmap

Wanted, but not soon. Promote an item to Open when the thing it waits for
arrives.

- **N-001** · raised `2026-0722-1303-wal-encryption-at-rest` · value low
  **btypedb encryption deferred set:** online key rotation, a
  plaintext↔encrypted migration helper, key+value scope, and
  ChaCha20-Poly1305. None of these exist yet. `btypedb/encrypt.go:40-41`
  reserves flag bits 1..4, and `compact.go:138-139` names the re-encrypt seam.
  Rotation first needs a v3 wrapped-DEK header, because `Compact`'s raw
  tail-copy is invalid across keys. Restated at `2026-0730-1924`,
  `2026-0907-2325`, `2026-0907-2339`, `2026-0916-1511`, `2026-0916-1557` and
  `2026-0916-1611`, and kept open by decision each time. **Trigger:** a
  deployment needs to rotate a key. *Restored 2026-10-03 by `/next-list`:
  commit `dd7a25d` (closing N-022) deleted the whole Roadmap and Non-goals
  sections with no closing line. Premise re-checked against btypedb then:
  still absent, and both citations still hold.*

## Non-goals

*Restored 2026-10-03 by `/next-list` after commit `dd7a25d` deleted this
section. The entries below are the text as of `3b603e9`, unchanged.*

- **N-005** · declined `2026-0730-2350-occ-stage2-sequences`: **Embedded
  Engine one-shot writes surface `ErrTxConflict` raw.** The caller writes the
  retry loop, the same contract as `WriteTxn`. Documented in
  `docs/concurrency.md`.
- **N-006** · declined `2026-0805-1603-correlated-subquery-index-pushdown`:
  **Correlated ON conjuncts and function-wrapped correlated predicates
  evaluate per row.** Full Postgres-style decorrelation was considered and
  rejected. For large outer sets, rewrite as a JOIN.
- **N-007** · declined `2026-0805-1603-correlated-subquery-index-pushdown`:
  **Statement paths that never seed a `subMemo` re-prepare correlated
  subqueries on each invocation.** This errs on the safe side, and they still
  get pushdown within one invocation. It was recorded as "known remaining
  (deliberate)" and never appeared in a Next list. Filed here so it stays
  visibly declined.
- **N-008** · declined `2026-0907-2325-next-list-rebuild-backlog-drain`:
  **Unindexed FK columns scan the child table on each check.** FK enforcement
  is planner-driven and doesn't require an index. The skill's gotchas document
  the workaround ("index the child FK columns").
- **N-009** · declined `2026-0907-2325-next-list-rebuild-backlog-drain`:
  **`WriteString` lint hints in test files.** Test readability is worth more
  than the allocation.
- **N-010** · declined `2026-0916-1511-next-list-rebuild-and-drain`:
  **General multi-action `ALTER TABLE`.** Only the `DROP CONSTRAINT t_pkey,
  ADD PRIMARY KEY` pairing is accepted, because it is the one pairing that must
  be atomic in bytdb. Other actions can run as separate statements.
- **N-011** · declined `2026-0916-1511-next-list-rebuild-and-drain`:
  **`ALTER COLUMN TYPE` on foreign-key columns.** Both sides would have to
  change together. Instead, drop the constraint, alter both columns, and re-add
  it.
- **N-012** · declined `2026-0916-1511-next-list-rebuild-and-drain`:
  **pgwire `v0.10.0` / `v0.11.0` tags stay unbackfilled.** The lockstep tag
  line resumes at `v0.12.0`.
- **N-013** · declined `2026-0916-1557-sqlstate-gaps-constraint-catalog`:
  **Unique indexes vs UNIQUE constraints in `table_constraints`.** bytdb can't
  tell them apart, so every unique index reports as UNIQUE.
- **N-014** · declined `2026-0916-1557-sqlstate-gaps-constraint-catalog`:
  **Synthetic NOT NULL CHECK rows in `table_constraints`.** Nullability is
  already in `information_schema.columns`.
- **N-015** · declined `2026-0916-1611-license-and-release-v0.15.0`:
  **License file in `bench/`.** It is an internal harness module and nothing
  imports it.
- **N-045** · raised `2026-1110-0555-deep-look-phase7-part2-phase8-start` ·
  declined `2026-1113-2328-n045-measured-declined-typer-copy-trim` by the
  user after measuring (trim in `0e02458`):
  **Cache the typed tree on prepared statements (D-40 step 3).** Measured
  on `BenchmarkDLPointSelectPrepared` (Apple M-series): the prepared point
  read is now 2.27 µs and 75 allocations (the plan's 3.9–4.9 µs and
  117–128 allocations predate D-40's other steps), and typing is about 6.5%
  of its CPU (~145 ns) and 9 of its allocations. A cache saves at most that
  in-process and under 1% over the wire (a point read is 11–27 µs), yet it
  must key on the Go types of the bound arguments as well as the catalog
  version (a string `$1` is untyped and takes its column's type, an
  `int64` is int, and refusals such as `1 = true` depend on it) and replay
  every conversion and error exactly: correctness risk in the D-23 pass
  for ~145 ns. The profile's cheap part landed instead: `typer.sel` and
  `typer.core` copy the 224-byte `Select` only when something changed (75
  → 73 allocations, 4,680 → 4,232 B/op). Revisit only if a profile shows
  typing well above this share of a hot statement.

## Closed

- **N-054** · raised `2026-1111-1759-numeric-type-n048` · closed
  2026-11-11, `2026-1111-1808-duplicate-relation-42p07-n054`. **A
  duplicate CREATE TABLE was SQLSTATE XX000.** It was all of them: no
  `pgwire/errors.go` case mapped any duplicate-relation error, so CREATE
  TABLE ("table already exists"), CREATE INDEX ("index already exists")
  and the already Postgres-worded CREATE VIEW / CREATE SEQUENCE / RENAME
  TABLE refusals all went out as XX000. Fixed with a new sentinel,
  `bytdb.ErrRelationExists` (`err.go`, `errRelationExists`), raised at
  all seven sites (`ddl.go` x3, `seqobj.go`, `view.go` x2, `index.go`)
  with Postgres's single wording `relation "x" already exists`. pgwire
  maps the sentinel and the bare text form to 42P07, the text case ahead
  of 42710 so a relation named `constraint_*` isn't claimed as a
  duplicate constraint. Evidence: `sql/difftest/corpus/78_duplicate_relation.sql`
  (9 `@error 42P07` entries, all failing with XX000 before the mapping)
  matches Postgres 16.15, as do messages; `TestDuplicateRelation` (pgx),
  `TestSentinelErrors` and `TestSQLStateMapping` rows; race suite and
  staticcheck v0.8.2 clean. Docs: README sentinel table, skill, and a
  gotchas line on bytdb's per-table index-name scope (verified).
  **Release notes:** duplicate-relation errors are now 42P07 (were
  XX000); CREATE TABLE's and CREATE INDEX's messages change to
  `relation "x" already exists`; new sentinel `ErrRelationExists`.

- **N-048** · raised `2026-1110-0700-deep-look-phase8-d52-d53-d51` · closed
  2026-11-11, `2026-1111-1759-numeric-type-n048`. **`numeric` / `decimal`
  columns were refused in CREATE TABLE.** The user chose a real decimal
  type over a lossy float8 alias or a better refusal. Plan
  `ai_docs/todo/2026-1111-numeric-type-plan.md` (Q1–Q10), all six phases:
  package `decimal` (exact arithmetic, Postgres's division scale and
  half-away rounding, typmod); tuple tags 0x08 (key, scale-normalized, so
  `1.0` and `1.00` are one key) and 0x09 (stored value, scale kept);
  `TNumeric` with `Column.Precision/Scale`, numeric primary keys also
  written into the row value so they read back at their scale; the SQL
  layer (decimal literals are numeric, int < numeric < float8 promotion,
  casts with typmod, comparisons, hash keys, sum/avg, numeric versions of
  `abs round trunc ceil floor sign mod`, `scale min_scale trim_scale`);
  pgwire OID 1700 text and binary both ways (encoder byte-identical to
  Postgres's `numeric_send`), pg_type/pg_attribute typmod/format_type/
  information_schema; the `database/sql` driver returns exact text; the
  shell's `.dump` round-trips. Evidence: `sql/difftest/corpus/77_numeric.sql`
  (40 statements) matches Postgres 16.15 exactly, and difftest now
  compares numeric columns as exact text; fuzzers for parse and tuple
  order/round-trip; race suite green. **Release notes:** a literal with a
  point or exponent (`1.5`) is now numeric (OID 1700) instead of float8,
  so `select 1.5`, `7.0/2` and `avg(int_col)` return numeric (Go API:
  `bytdb.Decimal`; driver: a string); `2.5::int` is 3 (numeric rounds
  half away from zero); `x::numeric(p,s)` rounds; `bigint out of range`
  and `division by zero` now carry SQLSTATE 22003 / 22012 (were XX000).
  Follow-ups raised: N-050 to N-053.

- **N-044** · raised `2026-1109-2243-deep-look-phase0-phase1` · closed
  2026-11-11, `2026-1111-1625-release-v0.23.0`. **Cut the deep-look release.** One release of
  everything on main, as the item's updates concluded. btypedb `v0.10.0`
  (`ad77fd8`, D-48's faster replay) was tagged first and the root bumped to
  it (`a6ce6f7`). Root `v0.23.0` on `a6ce6f7`, `pgwire/v0.23.0` on
  `e2f0e14` (pgwire, bench and sql/difftest pins bumped); all three resolve
  on proxy.golang.org. The release notes this item collected are in the
  `v0.23.0` tag body ("Upgrade notes"). Before tagging, the Linux CI `test`
  job had failed on every push since D-15:
  `TestWriteDeadlineBoundsStallNotTransfer` was cut after ~2.6 MB because
  Linux wakes a writer blocked on a full socket only after about a third of
  an autotuned (up to 4 MiB) send buffer drains, longer than the test's
  300 ms WriteTimeout at its ~2-3 MB/s pace. Reproduced in a
  `golang:1.26.1` container (5/5), fixed in the test by clamping both ends'
  socket buffers to 64 KiB (30/30, full race suite green on Linux), with
  `WriteTimeout`'s doc noting the same granularity for production
  (`38fd26d`). The release skill now covers `sql/difftest`, requires a
  green CI run before tagging, and orders a btypedb tag first.
- **N-031** · raised `2026-1104-1337-bytdb-cli-shell` · closed 2026-11-11,
  `2026-1111-1625-release-v0.23.0`. **`go install …@version` refuses the pgwire commands.**
  Validated after `pgwire/v0.23.0`, the first pgwire tag without a
  `replace`: from outside a checkout with `GOWORK=off`, `go install
  github.com/rohanthewiz/bytdb/pgwire/cmd/{bytdb,bytdbd}@latest` installs
  both commands and `go run …/cmd/bytdbd@latest -h` prints usage, once
  through the proxy (`GOPRIVATE=none.invalid`) and once with the default
  env. `go version -m` shows pgwire v0.23.0, bytdb v0.23.0, btypedb v0.10.0.
  The `pgwire-released-pin` CI job stays allowed-to-fail on purpose: it
  fails whenever pgwire uses root API newer than the last tag, which is
  the N-031 trade-off, not a defect.

- **N-043** · raised `2026-1109-1706-deep-look-audit-remediation-plan` · closed
  2026-11-10, `2026-1110-0757-deep-look-phase8-close`.   **Convenience (plan Phase 8, D-51 to D-58).** Statements and catalog
  tables that clients and migration tools send (advisory locks, `DISCARD
  ALL`, `current_user`, `pg_tables`); everyday SQL that fails to parse
  (`CAST`, `INSERT ... SELECT`, `FOR UPDATE`, `NULLS FIRST/LAST`); scalar
  functions and aggregates; migration DDL; sentinel errors and readable
  messages in the Go API; multi-statement `Exec` in the stdlib driver;
  replication flags and a Unix socket for `bytdbd`. Which tool needs which
  statement is from memory: capture each tool's real statements before
  prioritizing by it. *Updated `2026-1110-0700-deep-look-phase8-d52-d53-d51`:*
  D-52, D-53, D-54, D-55 and D-51 part 1 (session surface) are done. Left:
  D-51 part 2 (catalog tables and rows: `pg_tables`, `pg_indexes`,
  `pg_views`, `pg_settings`, `information_schema.schemata`/`views`/
  `referential_constraints`/`constraint_column_usage`, `relnatts`,
  `relhasindex`, varchar typmod, `regclass` rendering) and part 3
  (`NegotiateProtocolVersion`, verbatim ORM statements in
  `pgwire/orm_test.go`); D-56, D-57, D-58 rest; Phase 9 doc corrections;
  delete `ai_docs/todo/deep-look-repros/` once ported. *Updated
  `2026-1110-0737-deep-look-phase8-d51-d56`:* D-51 (all three parts) and
  D-56 are done. Left: D-57 (stdlib converters, DSN options, symlinks,
  `encryption_key_file`), D-58 rest (bytdbd replication flags, health
  endpoint, replication metrics, SIGHUP), Phase 9, the repro cleanup.
  The verbatim-statement tests cover goose, lib/pq and pgx only: GORM v2,
  golang-migrate and atlas were not in the module cache, so their
  statements still need capturing from source. *Closed:* all of
  D-51 (session surface, catalog tables and rows, NegotiateProtocolVersion,
  verbatim goose/lib/pq/pgx statements), D-52, D-53, D-54, D-55, D-56
  (sentinels, Postgres-worded messages, value shapes, Row getters, Lookup*,
  one-shots, ExecScript), D-57 (driver shapes, DSN settings, `:memory:`, key
  file, symlinks) and D-58 (daemon replication/restore, `/healthz`,
  replication metrics, SIGHUP reload) are on main, as is Phase 9's doc table.
  The repro directory was deleted after a run against main (recoverable from
  `2388d34`). The uncaptured tools moved to N-049.

- **N-042** · raised `2026-1109-1706-deep-look-audit-remediation-plan` · closed
  2026-11-10, `2026-1110-0555-deep-look-phase7-part2-phase8-start`. **Performance (plan Phase 7, D-37 to D-50).** Done:
  D-37 (uuid/timestamp/date pushdown), D-38 (IN-list probes), D-39 (in-place
  and column-masked decoding), D-40 steps 1, 2, 4 (descriptor memo, parse
  cache), D-41 (wire rows), D-42 (`bytdbd -socket`), D-43 (top-N heap), D-44
  (per-row overheads, LIKE prefix ranges, name index), D-45 (write-path
  allocations), D-47 first half (shape-only catalog lookups), D-48 in
  btypedb main (`ad77fd8`, replay 389 -> 104 ms; ships with a btypedb tag,
  N-044), D-49 sizing note, D-50 mitigation (`bytdbd` result cap 1M). D-46
  done as documentation (ReadTxn for hot loops). What was left open is
  carried in N-045 to N-047.
- **N-041** · raised `2026-1109-1706-deep-look-audit-remediation-plan` · closed
  2026-11-09, `2026-1109-2355-deep-look-phases-5-6`. **Replication and server hardening (plan Phase 6,
  D-34 to D-36).** D-34: generation names lead with a sequence number (one
  past the store's largest at roll time) and restore/pruning order by it,
  not the wall clock; `Status.Sealed` and `Status.LagBytes`; per-operation
  store deadlines scaled by payload (`OpTimeout`, `MinTransferRate`);
  size-tiered chunk merging (`MergeChunks`, parts deleted after a 10-minute
  grace); restore downloads 6 chunks at a time and re-lists once if a chunk
  vanishes; s3 `SessionToken` and an error for a truncated page without a
  continuation token. D-35: `pgwire.Server.Shutdown(ctx)` drains (idle
  connections get 57P01, running statements finish), `bytdbd
  -shutdown-grace`. D-36: float columns store NaN (sorting above +Inf, as
  Postgres does) and keep -0. Exporting `Sealed`/`LagBytes` as metrics is
  left to D-58 (N-043).
- **N-040** · raised `2026-1109-1706-deep-look-audit-remediation-plan` · closed
  2026-11-09, `2026-1109-2355-deep-look-phases-5-6`. **Interruptible writer-lock waits (plan Phase 5,
  D-33).** Done engine-side, without the btypedb `BeginCtx`/`UpdateCtx`
  release the plan assumed: in single-writer mode the engine holds a
  one-slot write semaphore (`Engine.acquireWrite`) around every btypedb
  write transaction, and the wait for it honors the statement's context, so
  `statement_timeout`, `CancelRequest` and a canceled `ExecContext` end the
  wait with the context's error and the write never runs. New
  `WriteTxnCtx`, `WriteTxnSerializableCtx`, `BeginCtx`,
  `BeginSerializableCtx`; the SQL layer uses them. No btypedb tag needed.
- **N-039** · raised `2026-1109-1706-deep-look-audit-remediation-plan` · closed
  2026-11-09, `2026-1109-2330-deep-look-phases-2-to-4`. **One statement, one snapshot, none of its own
  writes (plan Phase 4, D-29 to D-32).** D-29: UPDATE evaluates every SET
  and CHECK before writing, DELETE computes RETURNING before deleting
  (UPDATE ... RETURNING still evaluates after each write: its values come
  from the engine). D-30: UNIONs read one snapshot; DML materializes the
  views it uses inside its write transaction. D-31: child-side FK checks
  queue to end of statement (only when SET touches an FK column), and the
  parent side accepts a key that still exists afterwards; the plan's
  key-swap example fails on the primary key in Postgres too. D-32: a
  multi-statement Query or pipeline segment is one implicit transaction,
  opened lazily at the first write; DDL in a unit commits and runs alone
  (documented). The difftest corpus has no Phase 4 annotation left.
- **N-038** · raised `2026-1109-1706-deep-look-audit-remediation-plan` · closed
  2026-11-09, `2026-1109-2330-deep-look-phases-2-to-4`. **Subqueries through the real SELECT path (plan
  Phase 3, D-27, D-28).** Every shape the reduced runners do not implement
  runs through `runSelectBody` with the outer row as correlation scope
  (`DB.outer`, decorrelated body cached per statement); simple shapes keep
  the runners' plan cache. D-09's interim refusal is gone. `IN
  (uncorrelated subquery)` probes a memoized hash set. All twelve D-27
  corpus annotations removed.
- **N-037** · raised `2026-1109-1706-deep-look-audit-remediation-plan` · closed
  2026-11-09, `2026-1109-2330-deep-look-phases-2-to-4`. **One static typing pass (plan Phase 2, D-23 to
  D-26).** `sql/typecheck.go` types every statement, subquery, CTE, view
  body, CHECK and USING expression; untyped literals take their context's
  type, dates are cast to timestamps where compared, impossible pairings
  are 42883; `coerceLiterals` is removed and the runtime fallback reports
  22P02. Casts know their operand type (`ExCast.From`), floats render as
  Postgres does (`bytdb.FormatFloat`), result and parameter descriptions
  come from the pass (untyped parameters are OID 0), and grouping/dedup
  keys follow SQL equality. Exit test met: every "What breaks" bullet
  matches Postgres 16.6 in the corpus, FuzzExec ran 10 minutes clean
  (18.2M executions), the pgx probes pass. `VACUUM (VERIFY)` /
  `DB.VerifyChecks` find rows a previously inert CHECK admitted.
- **N-029** · raised `2026-1015-1609-txn-write-cap` · closed 2026-11-09,
  `2026-1109-2330-deep-look-phases-2-to-4`. **UPDATE's pre-write materialization skipped the row budget.**
  Fixed with deep-look D-29: the scan callback now charges `d.budget()`
  next to `CheckWrites`, so the row cap and the `CheckMemory` poll apply
  (`TestStatementSeesItsStartingState`).

- **N-036** · raised `2026-1109-1706-deep-look-audit-remediation-plan` · closed
  2026-11-09, `2026-1109-2243-deep-look-phase0-phase1`. **Small isolated correctness and outage fixes (plan
  Phase 1, D-05 to D-22).** All eighteen landed, one commit each (two
  pairs), each with its audit repro ported as a test that fails before the
  fix: D-05 restore tolerates overlapping chunks; D-06 the parser bounds
  expression-tree *depth* at 10,000 (54001), not a statement-wide count, so
  wide statements are unaffected; D-07 any error in a block fails it
  (`Session.Fail`, pgwire's `sendError`), BEGIN in a failed block is 25P02;
  D-08 comment-only queries are empty; D-09 interim refusal of the subquery
  shapes the runner answers wrongly (narrower than the plan: `EXISTS (...
  LIMIT 1)` and `ARRAY(... ORDER BY x)` stay accepted); D-10 upsert checks the
  parent side through a shared `parentKeyRecheck`; D-11 failed uniqueness
  probes are serializable reads; D-12 DDL callbacks that write are refused
  (writerGID in default mode, a DDL goroutine set under OCC); D-13 CACHE
  batches reserve in O(1), CACHE capped at 1,000,000; D-14 accept backoff
  and a 2x MaxConns hard ceiling; D-15 the write deadline is re-armed per
  socket write; D-16 prepared statements revalidate on
  `Engine.CatalogVersion` (0A000); D-17 the wire never sends a value under a
  type it contradicts; D-18 sequence functions refused in CHECK/USING; D-19
  closed-Txn `ErrTxClosed`, deep-copied descriptors from the accessors
  (Row.Desc stays shared; DDL builds on the stored bytes), identity bump on
  UPDATE, and three smaller fixes; D-20 OCC allocator leaks; D-21 eight SQL
  items; D-22 hex/octal/binary and underscore literals, E-string escapes,
  signs, the `||` precedence level, `= NULL`, `ANY(NULL)`. Tests that pinned
  old non-Postgres behavior were updated to Postgres 16.6's answers.
- **N-035** · raised `2026-1109-1706-deep-look-audit-remediation-plan` · closed
  2026-11-09, `2026-1109-2243-deep-look-phase0-phase1`. **Safety net (plan Phase 0, D-01 to D-04).** D-03:
  `.github/workflows/ci.yml` in bytdb and btypedb (vet, race on Linux and
  macOS, gofmt, staticcheck, a hard no-`replace` check on pgwire, an
  allowed-to-fail `GOWORK=off` pgwire build and btypedb-main canary, the
  difftest job, nightly fuzzing); both repos' first runs green. D-02:
  `sql/difftest/` drives bytdb and Postgres through one pgx client and
  compares values and type classes; `-- @todo D-xx` annotations make the
  corpus the list of open divergences (a run fails when one starts
  matching). D-04: `BenchmarkDL*` baselines. D-01 was done differently from
  the plan, on purpose: with fixes landing in the same session, each repro
  was ported test-first into the fix's commit rather than parked behind a
  `Skip` line first. The audit repros for later phases stay in
  `ai_docs/todo/deep-look-repros/` until their phase ports them.
- **N-034** · raised `2026-1104-1351-n030-n031-estring-split-pgwire-replace` · closed
  2026-11-04, `2026-1104-1401-n032-n034-identity-restart-nested-comments`. **The lexer did not nest block comments.**
  Fixed: `lex` (`sql/lexer.go`) now hands each `/*` to `blockCommentLen`,
  which counts depth and ends the comment at the `*/` that closes the
  outermost level, the way Postgres, pgwire's splitter and the shell's
  scanner already did. "unterminated block comment" now covers a depth
  that never reaches 0, including `/*/`. A stored view whose verbatim text
  has an unbalanced inner `/*` would stop parsing. Postgres rejects such
  text too, and broken views are already handled
  (`TestSystemCatalogBrokenView`). Tests: `TestLexNestedBlockComments`
  (seven nesting forms plus three unterminated ones; five fail on the old
  lexer), and pgwire `TestSimpleQueryNestedComment` (a nested comment
  holding `;` in one simple-protocol buffer; the old lexer gave a syntax
  error). The README's dialect paragraph says block comments nest.
- **N-032** · raised `2026-1104-1337-bytdb-cli-shell` · closed 2026-11-04,
  `2026-1104-1401-n032-n034-identity-restart-nested-comments`. **`.dump` could not carry identity counters.** Fixed in two
  parts. First, the SQL: `ALTER TABLE t ALTER [COLUMN] c RESTART [[WITH] n]`
  (Postgres's identity syntax; a bare `RESTART` means 1), backed by
  `Txn.RestartIdentity` / `Txn.PeekIdentity` and Engine wrappers
  (`identity.go`). It moves a counter, not the schema, so it runs in the
  statement's transaction like `TRUNCATE … RESTART IDENTITY`: allowed in a
  block, undone by ROLLBACK, refused in a read-only block. In
  concurrent-writes mode the commit discards the cached allocator through
  `dirtySeqPrefixes`. Errors use Postgres's wording: `column "c" of
  relation "t" is not an identity column` → 55000, `RESTART value (0)
  cannot be less than MINVALUE (1)` → 22023. The 22023 row also covers
  ALTER SEQUENCE's RESTART errors, which were XX000. Second, the dump:
  `writeDump` (`pgwire/cmd/bytdb/schema.go`) emits one `RESTART WITH`
  line per identity column whose counter exists, right after that table's
  rows. Tests: `TestRestartIdentity` (both write modes: allocator
  invalidation, rollback, reopen, refusals), `sql.TestAlterColumnRestart`
  (spellings, BEGIN/ROLLBACK, read-only, refusals, tag), pgwire
  `TestAlterColumnRestartWire` and `TestSQLStateMapping` rows. In the shell,
  `TestSchemaAndDumpRoundTrip` now draws and deletes id 4 before the dump.
  The restore draws 5, and without the new line it re-issues 4. Docs:
  README grammar, `docs/features.md` Auto-increment, `docs/cli.md`
  `.dump`. **Release note:** the shell now uses unreleased root API, so
  pgwire's `GOWORK=off` build fails until the next root tag and pin bump
  (the N-031 trade-off). The workspace build is green. That release
  should be a minor one, since it adds SQL and API.
- **N-030** · raised `2026-1104-1337-bytdb-cli-shell` · closed 2026-11-04,
  `2026-1104-1351-n030-n031-estring-split-pgwire-replace`. **pgwire's simple-query splitter mis-split `E'...'`
  strings.** Fixed: `splitStatements` (`pgwire/split.go`) now hands each
  quoted region to `skipQuoted`. An `E` (or `e`) that starts its own token
  before the quote turns on `\` escapes, and a doubled quote is consumed
  inside the region, so `E'a''b\'; c'` stays one string. The token test is
  the lexer's `isIdentPart` (ASCII letters, digits, underscore), so
  `some'x\'` is still an identifier followed by a standard string. Tests:
  ten new `TestSplitStatements` cases, and `TestSimpleQueryEString`
  (`pgwire/server_test.go`), which sends a create, an insert of two
  E-strings holding `;` and a select in one simple-protocol buffer over pgx.
  It fails on the old splitter with "unterminated string literal". The
  shell scanner's comment (`pgwire/cmd/bytdb/scan.go`), which said pgwire
  didn't know E-strings, now says the two follow the same rules.
- **N-028** · raised `2026-1015-0809-memory-ceiling-result-row-cap` · closed
  2026-10-15, `2026-1015-1609-txn-write-cap`. **Cap the size of one write transaction.**
  `Engine.SetMaxTxnWrites(n)` / `MaxTxnWrites()` in `memguard.go`, default
  `DefaultMaxTxnWrites` = 10M (0 or less disables). The write past the cap
  fails with `bytdb.ErrTxnTooLarge` ("transaction write limit exceeded"),
  which pgwire maps to SQLSTATE 54000. The hint names the batching recipe.
  Counted on `Txn` (`Txn.writes`, charged in `InsertReturning` /
  `UpdateReturning` once a write is staged), not in `insertRow` /
  `updateRow`, because only `Txn` lives as long as a transaction. One-shot
  `Engine.Insert` / `Update` write a single row and cannot reach any cap.
  `Txn.Savepoint` / `RollbackTo` / `Release` mirror the kv savepoint stack
  with the count at each mark, so `ROLLBACK TO` gives writes back.
  `Txn.CheckWrites(n)` lets the SQL layer refuse early: `execUpdate`
  charges its pre-write materialization row by row, and a plain multi-row
  INSERT (no ON CONFLICT) checks its row count, so both fail before
  staging anything. **Decisions:** DELETE and TRUNCATE are exempt. A delete
  stages about 32 B/row against about 350–380 B/row for insert and update
  (measured, 200k rows, 3-column unindexed table), and it is how an
  operator frees space. The default is 10M rather than the backfill cap's 1M because dbc's
  `sql.write` loads a whole table in one transaction, so a few million rows
  is an ordinary load. `bytdbd -max-txn-writes`. Tests: `txnwrites_test.go`
  (counting, exemptions, savepoints, WriteTxn, disabled), `sql/txnwrites_test.go`
  (UPDATE / INSERT refused whole, ON CONFLICT DO NOTHING not over-charged,
  DELETE exempt, BEGIN block accumulation and ROLLBACK TO give-back), and
  `pgwire` `TestSQLStateMapping` / `TestResourceErrorsEndToEnd` (54000). Docs:
  README Monitoring memory, `docs/gotchas.md` Sizing, `docs/security.md`
  flag table. Released in `v0.22.0` (`3b2efdc`) and `pgwire/v0.22.0`
  (`bb7f808`).
- **N-027** · raised in dbc as its N-131 (dbc doc `2026-1006-1855-scripts-dir-resolution`) · closed
  2026-10-06, `2026-1006-1907-int-into-bool-coerce-v0.21.2`. **An integer 0/1 could not be stored in a bool column.** SQLite
  has no boolean type and returns a BOOLEAN column as int64 0/1, so dbc's
  `s.Copy("demo-sqlite", "demo-bytdb", "cats", …)` failed on `adopted` with
  "value does not fit column type" (`coerce` in `dml.go`). Fixed in `coerce`,
  so the Go API, `database/sql` and pgwire all accept it. An integer of any
  width that is 0 or 1 becomes false/true, the same rule as `database/sql`'s
  `driver.Bool`. Any other integer is refused with "integer for a bool column
  must be 0 or 1". Side effect: the SQL literal `INSERT … VALUES (1)` into a
  bool column is now accepted, where Postgres refuses it. Bound parameters and
  literals look the same by the time they reach `coerce`. Tests:
  `stdlib.TestIntParameterIntoBool` (INSERT and UPDATE via `$n`, plus the 2
  rejection) and new cases in `TestCoerceWidthsAndMismatches`. Both fail on
  the old `coerce`. `docs/features.md` notes the rule on the `BOOL` row. Released in `v0.21.2` (`38e193a`) and `pgwire/v0.21.2`
  (`b958b26`). dbc still has to bump to v0.21.2 to close its N-131.

- **N-026** · raised in dbc as its N-128 (dbc doc `2026-1006-1430-headless-dbc-copy`) · closed
  2026-10-06, `2026-1006-1520-date-input-accepts-timestamp-text`. **A `time.Time` could not be bound to a date column.**
  `stdlib.CheckNamedValue` sends every `time.Time` as `'2006-01-02
  15:04:05…'` UTC text, and `ParseDate` took only `YYYY-MM-DD`, so the insert
  failed with "invalid input syntax for type date". That blocked dbc's
  `dbc copy` / `s.Copy` of any date column into bytdb, since pgx reads
  Postgres dates as midnight-UTC `time.Time`. Fixed in `ParseDate` (`types.go`),
  not the driver. It now accepts every `ParseTimestamp` form, and keeps the date
  as written while dropping the time and zone, as Postgres does (checked
  against Postgres 17: `'2024-01-02 23:30:00-05'::date` is 2024-01-02, and
  `'2024-01-02 25:00:00'` is still an error). A bound `time.Time` therefore
  lands on its UTC day, the same day the Go API (`coerce` in `dml.go`)
  truncates one to. Tests: `TestValueTextRoundTrip/date_from_timestamp_text`,
  and `stdlib.TestTimeParameterIntoDate`, which fails on the old `ParseDate`.
  Released in `v0.21.1` and `pgwire/v0.21.1`. dbc still has to bump to
  v0.21.1 to close its N-128.

- **N-025** · raised `2026-1005-1022-add-column-if-not-exists-v0.20.0` ·
  closed 2026-10-05, `2026-1005-1107-compaction-stall-vacuum-verbose-notices`. **Skip notices go out at
  WARNING severity with SQLSTATE 01000.** Fixed: `noticeBody`
  (`pgwire/errors.go`) now sends:
  - `... already exists, skipping` as NOTICE 42P07, or 42701 for a column;
  - `... does not exist, skipping` as NOTICE 00000;
  - VACUUM VERBOSE's report as INFO 00000.

  VACUUM's `skipping "v" --- cannot vacuum non-tables` stays WARNING
  01000, because it is a WARNING in Postgres too. `sendNotice`
  (`pgwire/conn.go`) now sends a multi-line `Result.Notice` as one
  NoticeResponse per line. Before, VACUUM's joined skip warnings went out
  as a single message. Tests: `TestNoticeSeverities`
  (`pgwire/notice_test.go`) runs 13 statements over pgx and checks
  severity, code and message for each. Checked over the wire with
  bytdbd. Commit `5722154`.
- **N-024** · raised `2026-1003-0815-next-list-restore-sess-save-guard` ·
  closed 2026-10-05, `2026-1005-1107-compaction-stall-vacuum-verbose-notices`. **Nothing tells you whether a
  VACUUM is worth running, or what it reclaimed.** Fixed with the cheap
  version the item described:
  - btypedb v0.9.0 adds `LogStats()`, which returns epoch, size and
    `BaseSize` read under one lock.
  - bytdb's `Stats` gains `LogBaseBytes`, and `/metrics` gains
    `bytdb_log_base_bytes`.
  - VERBOSE is now kept rather than discarded (`(VERBOSE false)` turns it
    off), and `VACUUM VERBOSE` appends an INFO line:
    `compacted the storage log: B bytes before, A after (R reclaimed)`.

  `LogBytes - LogBaseBytes` is the growth auto-compaction measures. After
  a restart, the base is the whole file as found, so the growth
  understates the garbage until the first compaction. The `Stats` doc
  says so. `Engine.Compact` still returns only an error: VACUUM reads
  `LogState` on either side of it. Tests: `TestParseVacuum` (verbose
  cases), `TestVacuumVerbose`, `TestEngineCompact` (base assertions),
  `TestMetricsHandler`, and btypedb's `TestLogStats`. Commit `b841789`.
- **N-023** · raised `2026-1003-0815-next-list-restore-sess-save-guard` ·
  closed 2026-10-05, `2026-1005-1107-compaction-stall-vacuum-verbose-notices`. **Compaction's writer pause grows
  with database size.** The proposed fix (an early snapshot sync) wasn't
  enough on its own. Measured with a fast writer (`SyncNever`, 1 KB values),
  phase B's copy of the *tail* also grew with the database: ~400 MB of
  tail at 900 MB, because the tail is every append made while the snapshot
  streamed. With only the early sync, the worst stall got *worse*.
  btypedb v0.9.0 (`902e317`) runs an unlocked catch-up loop instead. It
  copies the tail so far and fsyncs, up to 4 rounds, until at most 1 MiB
  is left. Phase B then splices and syncs only that residue, and skips
  the sync when there is none. Measured phase-B lock hold at ~900 MB:
  v0.8.0 340–540 ms, v0.9.0 16–32 ms. Two follow-on fixes keep the
  auto-compaction policy unchanged:
  - `baseSize` excludes appends made after the snapshot streamed.
  - A successful run re-checks the policy, because a burst that ends
    during the catch-up has no later append to trigger it.

  Writers can still stall outside the lock when they outrun the disk (a
  `SyncNever` writer at memory speed saw 100–450 ms), probably from the
  kernel throttling writes. Under `SyncAlways`, each write pays its own
  F_FULLFSYNC, and old and new behave about the same. Tests: btypedb
  `TestCompactSyncsSnapshotBeforePausingWriters` (no tail, small tail,
  tail over the slack) and `TestAutoCompact` (now waits for chained runs).
  bytdb bumped to v0.9.0, `Engine.Compact`'s doc corrected (`d9631d3`).
- **N-022** · raised `2026-0925-1542-file-lock-release-v0.17.0` · closed
  2026-09-25, `2026-0925-1607-lock-in-btypedb-v0.18.0`. **The file lock covers only
  `bytdb.Open`.** Closed ahead of its trigger. The lock moved into btypedb
  v0.8.0 (`lock.go` there): `btypedb.Open` takes it, so raw opens are covered
  for every btypedb user. It sits on a new `fsys.Lock` seam, not
  `realFS.OpenFile` as first suggested, because locking the database file
  itself stops working after the first compaction rename. `Backup` locks its
  destination, and the new `btypedb.AcquireLock` guards `replicate.Restore`'s
  `destPath` through the rename. Both refuse a live file with `ErrLocked`.
  bytdb dropped its own copy (the lock is not reentrant) and aliases
  `ErrLocked = btypedb.ErrLocked`. Released in btypedb `v0.8.0` and bytdb /
  pgwire `v0.18.0`. Other btypedb consumers (cats, gonotes, grmob, …) get
  `ErrLocked` on a double open once they bump. Tests: btypedb `lock_test.go`,
  bytdb `TestLockRawKVOpen` / `TestLockBackupDestination`,
  `replicate/restore_lock_test.go`.

- **N-021** · raised in dbc, `2026-09-25` (no bytdb session doc) · closed
  2026-09-25, `2026-0925-1542-file-lock-release-v0.17.0`. **bytdb takes no file lock.** Two dbc processes
  (two TUIs, or a TUI and `dbc web`) opened the same `demo.bytdb` and both
  wrote its WAL without a word. Fixed: `Open` takes a non-blocking
  exclusive lock on a `<path>.lock` sidecar before btypedb touches the
  file, and fails with the new `bytdb.ErrLocked` (with `holder_pid`); `Close`
  releases it after the final fsync (`lock.go`, `lock_flock.go`,
  `lock_windows.go`, `lock_other.go`). Released in `v0.17.0`. dbc still has
  to bump to v0.17.0 and match `errors.Is(err, bytdb.ErrLocked)`. Tests:
  `lock_test.go`, including a real second process and a `kill -9` holder.
- **N-020** · raised `2026-0924-1717-drop-unique-constraint-bind-format-sqlstate` ·
  closed 2026-09-24, `2026-0924-1717-drop-unique-constraint-bind-format-sqlstate`.
  **`cannot drop a primary key column` maps to XX000.** Fixed, with the rest
  of the primary-key DDL errors. A wire probe found seven more that were
  mis-mapped:
  - **42P16** (the one-key invariant) now also covers a key column's
    DROP NOT NULL, ADD COLUMN ... PRIMARY KEY, CREATE TABLE with no key,
    and the parser's short "multiple primary keys".
  - **42703** covers a key naming an undeclared column
    (`primary key column not declared`) and Postgres's
    `column "x" of relation "t" does not exist` (ADD PRIMARY KEY, ALTER
    COLUMN).
  - **42701** covers a column named twice in a key and a RENAME onto a
    taken name. `duplicate primary key column` used to come back as 23505,
    because it contains the data-conflict wording "duplicate primary key".
    A client would have read a DDL typo as a unique violation. The 42701
    case now comes before 23505.

  Tests: new `TestSQLStateMapping` rows, including one pinning that a
  constraint's "does not exist" stays 42704. `TestPrimaryKeyDDLErrors`
  (`pgwire/server_test.go`) runs 11 statements over pgx.
- **N-019** · raised `2026-0924-1717-drop-unique-constraint-bind-format-sqlstate` ·
  closed 2026-09-24, `2026-0924-1717-drop-unique-constraint-bind-format-sqlstate`.
  **Dependency refusals map to XX000.** Fixed: `sqlstate`
  (`pgwire/errors.go`) maps every dependency refusal to 2BP01. When filed,
  the item named four. The code has eleven: the two "because other objects
  depend on it" wordings (DROP TABLE, DROP/RENAME COLUMN blocked by a
  CHECK), the two "a foreign key depends on" ones (unique index, PK
  replace), dropping an indexed column or an FK column, and the "referenced
  by a foreign key" refusals (drop/rename column, rename table, alter type).
  All eleven get 2BP01, even where Postgres would allow the change, because
  the remedy is the same in every case: remove the dependent object, then
  retry. TRUNCATE of a referenced table keeps Postgres's 0A000. Tests: 13
  new `TestSQLStateMapping` rows, and `TestDependencyRefusals`
  (`pgwire/server_test.go`), which triggers the refusals from real SQL over
  pgx. "Drop a column referenced by a foreign key" (`ddl.go:438`) is covered
  by a mapping row only. From SQL, an earlier check always fires first: a
  referenced column is always in the primary key ("cannot drop a primary key
  column") or in a unique index ("cannot drop an indexed column").
- **N-018** · raised `2026-0924-1559-pg-constraint-key-rows` · closed
  2026-09-24, `2026-0924-1717-drop-unique-constraint-bind-format-sqlstate`.
  **`DROP CONSTRAINT` refuses a unique constraint's name.** Fixed:
  `execDropConstraint` (`sql/check.go`) now tries unique indexes after
  checks and FKs, and drops a match through `Engine.DropIndex`, so it keeps
  DROP INDEX's refusal when an FK depends on the index. A plain index is not
  a constraint and still gets "does not exist". Test:
  `TestDropConstraintUnique`.
- **N-004** · raised `2026-0916-1557-sqlstate-gaps-constraint-catalog` ·
  closed 2026-09-24, `2026-0924-1717-drop-unique-constraint-bind-format-sqlstate`.
  **`bad parameter format code` maps to XX000.** Fixed: `sqlstate`
  (`pgwire/errors.go`) now maps it to 08P01 next to "wrong number of
  parameters". Tests: the `TestSQLStateMapping` row now expects 08P01, and a
  new case 3 in `TestBindFormatCountMismatchIsProtocolError` sends a Bind with
  format code 2 over the wire and checks the ErrorResponse's `C` field.
- **N-003** · raised `2026-0916-1557-sqlstate-gaps-constraint-catalog` ·
  closed 2026-09-24, `2026-0924-1559-pg-constraint-key-rows`. **`pg_constraint` omits primary
  keys and unique constraints.** Fixed: one `p` row per table and one `u` row
  per unique index, each with `conindid` set to the backing index and its oid
  reused as the constraint oid (`sql/syscat.go`). `conkey` and `confkey`
  (FK rows) are now filled as `{1,2}` literals; before, they were NULL on
  every row. `pg_get_constraintdef` renders `PRIMARY KEY (...)` and
  `UNIQUE (...)`. psql's `\d` index listing now joins the pkey to its
  constraint. Tests: `TestSystemCatalogKeyConstraints`, plus a new assertion
  in `TestPsqlDescribeTable`.
- **N-002** · raised `2026-0805-1820-benchmark-rerun-m1pro-doc-refresh` ·
  closed 2026-09-24, `2026-0924-1522-next-list-seed-view-catalog-v0.16.0`. **`bench/go.mod` pin goes stale on every release.**
  Tidied from v0.14.0 to v0.16.0, and bench now builds with and without
  `GOWORK=off`. The lasting fix is the new `/release` skill
  (`.claude/skills/release/SKILL.md`). It is the first written release
  routine, and step 5 tidies bench right after pgwire's pin bump, so the pin
  follows every release.
- **N-017** · raised in dbc, `2026-09-24` (no bytdb session doc) · closed
  2026-09-24, `2026-0924-1522-next-list-seed-view-catalog-v0.16.0`. **Views have no columns in the catalog.** Fixed:
  `pg_attribute` and `information_schema.columns` now list each view's
  output columns, and `information_schema.tables` lists views as `VIEW`. The
  columns come from describing the stored query with `staticView`, the same
  path EXPLAIN and wire Describe use, so nothing executes
  (`DB.viewColumns`, `sql/syscat.go`). A `catalogShapes` flag stops the
  recursion a view over `pg_attribute` would otherwise cause. Tests:
  `TestSystemCatalogViews` and `TestSystemCatalogBrokenView`. Checked over
  the wire with dbc's own column and table queries. dbc's bytdb-specific
  table query (`dbc/db/catalog.go:49`) can now fall back to the generic
  `information_schema.tables` one if wanted.
- **N-016** · raised `2026-0805-1854-occ-truncate-insert-race-flake-fix` ·
  closed 2026-09-24, `2026-0924-1522-next-list-seed-view-catalog-v0.16.0`. **Use Go 1.25's `WaitGroup.Go` for the
  `wg.Add(1)`/`go func` pairs in `seq_occ_test.go`.** gopls suggested it, and
  the item never reached a Next list. The rebuild found it already done:
  `seq_occ_test.go` has six `wg.Go(` calls and no `wg.Add(1)` left. They came in with
  commit `6145d14` (the `2026-0907-2325` backlog drain).

Closures before 2026-09-24 are recorded in the session docs' `## Next`
sections, for example
`2026-0916-1611` (release `v0.15.0`) and `2026-0916-1557` (SQLSTATE gaps,
`table_constraints`, bench tidy).
