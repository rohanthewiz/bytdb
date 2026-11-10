# Deep-look plan: Phase 7, part 1 (pushdown, decoding, caches, wire)

Session: `2c5b1603-fa95-4c81-b037-0bcdc19533ac` (continues
`2026-1109-2355-deep-look-phases-5-6`).

## What landed

- **D-37, timestamp/date/uuid key pushdown** (`75c9785`). `litFits`
  accepts their canonical runtime forms (int64; 16 bytes). The bind pass
  folds `col <op> 'lit'::type` into a Pred when the cast's type is the
  column's own. Join/correlated template caches key on `pushKind` (Go type
  plus uuid-sized), so a bytea column joined to a uuid key cannot rebind a
  short value into the bound. EXPLAIN prints typed literals. UUID point
  select 1636 µs -> 3.9 µs.
- **D-38, IN-list probes** (`4931b32`). One `col IN (consts)`, `col =
  ANY(array)` or same-column OR of equalities counts as an equality when
  scoring paths; keys are canonicalized, NULLs dropped, sorted in the path
  column's direction and de-duplicated; `scanPlan` runs the plan once per
  key. Order stays served (the probed column is ordered, not pinned). Join
  steps carry IN conjuncts in `joinStep.probes`. A constant IN list in a
  filter probes a per-statement hash set. Ten-key IN on a PK: 42 ms -> 12 µs.
- **D-39, decoding** (`7bb2dee`). `decodeRowCols` decodes straight into the
  row; one-allocation strings; `tuple.Skip`; `Txn.ScanWith(ScanOptions)`
  with a `Cols` mask; SELECT steps derive the mask from every column name
  the statement mentions. Count/sum over 100k rows 31.4 -> 14.5 ms.
  `docs/benchmarks.md` now says decoding, not the interpreter, was the gap.
- **D-40** (`5412ff1`). `Engine.TableShared` memoized per catalog version
  (capped at 4096 names); no blob copy on a descriptor-cache hit; an LRU
  parse cache (512 texts, SELECT/INSERT/UPDATE/DELETE only). Unprepared
  point select 3.57 -> 2.26 µs, prepared -15%.
- **D-41, wire rows** (`a4ca496`). DataRows appended into one reused
  buffer with back-patched lengths; `bytdb.Append{Float,Timestamp,Date,UUID}`;
  64 KiB writer. Wire scan text 82 -> 42 ms, binary 48 -> 27 ms.
- **D-42, Unix socket** (`c54e68b`). `pgwire.ListenUnix`/`SocketPath`,
  multi-listener `Serve`, local sockets exempt from `RequireTLS`, `bytdbd
  -socket`. Point read 24 µs (TCP) vs 12 µs (socket).

## Decisions

- **Column masks over-approximate by name.** Every column name anywhere in
  the statement (subqueries, CTEs, windows) counts, whatever its
  qualifier; a star, a whole-row-shaped name or an unknown expression node
  turns the mask off. Only SELECT executors set masks, and only once a step
  plans a real scan. A test runs 26 query shapes with masks on and off.
- **Typed-tree caching (D-40 step 3) deferred.** Typing runs after binding
  because it converts parameter values (text `$1` against a uuid column
  becomes 16 bytes); caching the typed tree would need that conversion
  redone per value. The descriptor memo removed most of what typing cost.
- **Parse cache limited to DML/query kinds,** guarded by a test that
  deep-compares each cached tree with a fresh parse after execution.
- **Descriptor memo safety** rests on every descriptor write going
  through `updateDDL`, which bumps the catalog version after commit; a
  lookup racing an uncommitted bump sees the catalog from just before it.
- **One IN conjunct drives probes;** others stay filters. NOT IN never
  probes.

## Measurements

Apple M3, benchstat n=6: BenchmarkDLUUIDPointSelect 1636 µs -> 3.93 µs;
DLInList10PK 42.0 ms -> 12.1 µs; DLAnyArrayPK 41.6 ms -> 10.7 µs;
DLCountSum100k 31.4 -> 14.5 ms (14 -> 4 allocs/row); DLPointSelect 3.57 ->
2.26 µs; DLPointSelectPrepared 2.68 -> 2.27 µs; DLWireScan100kText 82.5 ->
42.5 ms; DLWireScan100kBinary 48.1 -> 27.2 ms; DLWirePointReadUnix 12.3 µs vs
24.0 µs over TCP. Bench harness scan_agg (bytdb-lazy) 12.3 ms.
FuzzTupleRoundTrip 30 s clean with the new Skip check.

## Next

Closed: None. Declined: None. Raised: None. Deferred: None. Promoted: None.
Moved: None. Updated: N-042.
Full list: `ai_docs/todo/next-list.md`.
