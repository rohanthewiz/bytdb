# Replication & Backup

bytdb replicates the way [Litestream](https://litestream.io/) does: the storage
log is append-only between compactions, so incremental replication is **byte-range
shipping** — no page shadowing, no checkpoint racing. The `replicate` package
polls the engine's log cursor and uploads whatever appended since its watermark
as immutable chunk objects in any S3-compatible object store.

This is **recovery, not high availability**: a restored node comes up from
object-store state; it does not fail over live. There is no follower apply loop
and no leader election.

```mermaid
flowchart LR
    subgraph proc [your process]
        eng[bytdb Engine] --> kv[btypedb log file<br/>append-only between compactions]
        rep[Replicator<br/>ticker, default 5 s]
    end
    subgraph bucket [S3-compatible bucket]
        direction TB
        c1["prefix/gen/&lt;gen-id&gt;/0000…0000-0000…8000.wlog"]
        c2["prefix/gen/&lt;gen-id&gt;/0000…8000-0001…0000.wlog"]
        man["prefix/gen/&lt;gen-id&gt;/manifest.json<br/>(completeness marker)"]
    end
    rep -->|"LogState() → (epoch, size)"| eng
    rep -->|"ReadLogRange(epoch, from, max)"| eng
    rep -->|atomic PUT| c1 & c2 & man
```

## Quick start

```go
e, _ := bytdb.Open("site.db")

store, _ := s3.New(s3.Config{
    Endpoint:  "https://us-east-1.linodeobjects.com",
    Region:    "us-east-1",
    Bucket:    "db-replicas",
    AccessKey: os.Getenv("S3_ACCESS_KEY"),
    SecretKey: os.Getenv("S3_SECRET_KEY"),
})

r := replicate.New(e, store, replicate.Options{
    Interval: 5 * time.Second, // the data-loss window
    Prefix:   "sites/stjohns",
})
r.Start()
defer r.Close() // final flush; close before e.Close()

// Disaster recovery, elsewhere:
info, _ := replicate.Restore(ctx, store, "sites/stjohns", "site.db")
```

`Replicator.ShipNow(ctx)` forces a synchronous ship (after a critical
transaction, say), and `Status()` returns the current generation, epoch,
watermark, last ship time, and last error — ready for a health endpoint —
plus two fields that show how exposed the latest writes are:

- **`Sealed`** — the current generation has been certified complete. It is
  `false` after every process start and every compaction until the whole log
  has been uploaded once; until then `Restore` returns the *previous*
  complete generation, so nothing written since the roll is protected. On a
  large database over a slow link that window is the length of a full
  re-upload. Alert on `Sealed == false` lasting longer than you expect a full
  upload to take.
- **`LagBytes`** — log bytes not yet shipped as of the last ship attempt (log
  size minus watermark). It jumps to the whole log size at a roll and falls as
  chunks upload.

## What gets shipped

**Raw byte ranges of the single log file** — not logical records, not WAL
segments. Because btypedb's log *is* the database file, a restore that
concatenates the chunks is a byte-identical prefix of the source file, and it
opens exactly like a crash-recovered local database: every record is CRC-framed
with batch atomicity, so a torn or missing tail chunk costs seconds of data,
never validity.

The object layout (`Replicator.chunkKey` in `replicate/replicator.go`):

```
<prefix>/gen/<generation-id>/<start>-<end>.wlog   one shipped byte range (offsets as 16-digit hex)
<prefix>/gen/<generation-id>/manifest.json        completeness certificate
```

Generation IDs lead with a **sequence number**, then a UTC timestamp and a
random suffix (`g0000000000000042-20261109t150405.000000000-8f2a1c40`). At each
roll the replicator lists the store and takes one more than the largest
sequence number there, and that number is what orders generations for
restore and pruning. The timestamp is only for people reading a bucket
listing: ordering by it let a clock stepped backwards, or a replacement host
whose clock lags, make a stale generation look newest — restore then picked
it, and pruning deleted the live one. Names written before sequencing (plain
timestamps, `20060102t150405.000000000-8f2a1c40`) count as sequence 0: older
than any sequenced generation, ordered by name among themselves.

Because the first roll in a process needs that listing, a store that cannot
list fails the ship (retried next tick); a later roll in the same process
falls back to its own last number plus one, which is still the largest,
since one replicator owns a prefix.

The `Storage` contract only demands atomic PUT and ordered listing, which is
why the interface is four methods (`Put`, `Get`, `List`, `Delete`) and
anything S3-shaped can stand in.

## The engine's replication cursor

btypedb exposes exactly two primitives (`btypedb/replication.go`), re-exported
on `bytdb.Engine`:

- `LogState() (epoch uint64, size int64)` — the current log identity and how
  many fully-appended bytes are safe to ship. A torn append never advances
  `size`.
- `ReadLogRange(epoch, from, max, w)` — copy `[from, from+max)` of the log
  into `w`, but only if the log is still the one `epoch` names. The copy holds
  off **compaction only** (via `compactMu`); readers and writers are never
  blocked, because the bytes being copied are immutable.

The `epoch` is bumped in exactly one place: after compaction's atomic
rename swaps in the rewritten file (`btypedb/compact.go:93`). Epochs restart
at 0 on `Open`, so a process restart looks like a compaction to the
replicator — deliberately, since crash recovery may truncate a torn tail and
post-restart appends could diverge from previously shipped bytes.

```
writer:   |-- epoch 0 (append-only) --| compact |-- epoch 1 --| ...
follower: ship [0,a) [a,b) [b,c) ...   restart   ship [0,x) ...
```

## The ship cycle

Every tick (and every `ShipNow`) runs the same loop, serialized by a mutex so
manual and timed ships never interleave:

```mermaid
sequenceDiagram
    participant R as Replicator
    participant E as Engine (btypedb)
    participant S as Object store
    R->>E: LogState()
    E-->>R: (epoch, size)
    alt epoch changed (compaction) or first ship
        R->>S: LIST gen/ (largest sequence number)
        Note over R: roll generation:<br/>gen ID with seq = largest + 1,<br/>watermark = 0, Sealed = false
        R->>S: prune generations beyond RetainGenerations
    end
    loop while watermark < size
        R->>E: ReadLogRange(epoch, watermark, ≤8 MB)
        E-->>R: chunk bytes (holds off compaction only)
        R->>S: PUT gen/⟨id⟩/⟨watermark⟩-⟨end⟩.wlog
        alt PUT fails
            Note over R: watermark NOT advanced —<br/>range from the same offset re-ships next tick
        else
            Note over R: watermark += n
        end
    end
    opt log fully drained & not yet sealed at this size
        R->>S: PUT gen/⟨id⟩/manifest.json {generation, epoch, size, seq}
        Note over R: sealed = watermark, Sealed = true →<br/>idle ticks emit zero PUTs
    end
    opt MergeChunks adjacent small chunks of one tier
        R->>E: ReadLogRange(epoch, run start, run length)
        R->>S: PUT the run as one chunk
        Note over R: parts deleted after a 10-minute grace
    end
```

Design points worth knowing:

- **Chunks are immutable.** A failed `PUT` re-reads and re-uploads from the
  same offset next tick; atomic PUT means there is never a half-object. The
  retry is not always the identical range: a chunk's key names its end
  offset, and if the log grew in between, the retry reaches further. When the
  failed `PUT` had in fact been stored (a lost response), the generation then
  holds two chunks from the same offset, `[w,S1)` and `[w,S2)`. Restore
  tolerates that: at each offset it takes the chunk reaching furthest and
  trims any overlap with the previous one. Overlapping chunks of a generation
  are byte-identical over the overlap, because a generation is one log epoch
  and the log is append-only within an epoch.
- **`ErrEpochChanged` mid-read is not an error** — compaction won the race;
  the ship returns quietly and the next one rolls a fresh generation from
  offset zero.
- **The manifest certifies completeness.** It is written only once the log has
  been fully drained at some size, and re-written as the generation grows.
  Restore uses it to distinguish "complete generation" from "freshly rolled
  generation that has shipped one chunk."
- **Retry policy is the interval itself.** Failures are logged and retried
  next tick — no backoff state to corrupt. `Close()` performs a final flush
  on a fresh 30-second context so shutdown doesn't lose the tail.
- **Chunk size bounds compaction stalls.** `ReadLogRange` holds off compaction
  for the duration of one chunk copy, so `MaxChunkBytes` (default 8 MB) is
  also the knob for the maximum compaction hold.
- **Every store operation has a deadline.** An operation moving *n* bytes gets
  `OpTimeout + n / MinTransferRate` (defaults 30 s and 64 KiB/s: 158 s for an
  8 MB chunk). Without it, a PUT whose connection is black-holed mid-body
  waits for TCP to give up — many minutes — while holding the ship lock, so
  `ShipNow` callers and the shutdown flush queue behind it. Set
  `MinTransferRate` below your uplink's real worst case: a deadline shorter
  than an honest upload fails every attempt and replication never progresses.
  `OpTimeout < 0` disables the bound. Restore uses the defaults.

### Chunk merging

Each ship uploads what appended since the last as a new object, so a database
written a little at a time used to grow one object per active tick until the
next compaction — up to ~17,000 a day at the default interval — and restore
downloaded each one. The replicator now merges them, size-tiered like an LSM
tree:

```
tier 0:  [a][b][c][d][e][f][g][h]     MergeChunks (default 8) adjacent tier-0 chunks
             │ re-read [a.start, h.end) from the log, PUT as one object
             ▼
tier 1:  [a ──────────────────── h]   parts deleted after a 10-minute grace
tier 1 × 8 → tier 2, and so on, while the merged object fits MaxChunkBytes
```

A byte is re-uploaded once per tier it climbs, and tier *k* spans 8^*k* ticks,
so a day of steady small writes is about five tiers of at most seven chunks
each, plus the full-size chunks. A run whose merge would exceed
`MaxChunkBytes` is not merged (its first chunk is pinned and the rest wait
for later arrivals), so no object grows past the limit or the memory it
implies. `MergeChunks < 0` disables merging.

Merging needs no coordination with a concurrent restore: the merged chunk
holds the same immutable log bytes as its parts, restore's chain builder
takes the furthest-reaching chunk at each offset and ignores the ones it
covers, and parts outlive the merge by the grace period. Merged-away parts
still pending when the process exits are left behind — redundant, harmless,
and removed with their generation.

### Generation lifecycle

```mermaid
stateDiagram-v2
    [*] --> Rolling: first ship after Open,<br/>or epoch change (compaction)
    Rolling --> Shipping: new generation ID minted,<br/>older generations pruned
    Shipping --> Shipping: chunk PUT,<br/>watermark advances
    Shipping --> Sealed: log drained →<br/>manifest.json written
    Sealed --> Shipping: new appends →<br/>more chunks, manifest re-certified
    Sealed --> Rolling: compaction / restart<br/>bumps epoch
    Rolling --> [*]: generation pruned once<br/>RetainGenerations newer exist
```

Retention defaults to 3 generations (floor of 2), counted in sequence order;
pruning runs at generation roll, before the new generation's first chunk, and
a pruning failure is logged but never blocks shipping. The newest complete
(manifested) generation is never pruned, wherever it falls in the window.

## Restore

`replicate.Restore(ctx, store, prefix, destPath)` picks the newest **complete**
generation and reassembles it:

```mermaid
flowchart TD
    list[List generations under prefix,<br/>newest first by sequence number] --> pass1{generation has<br/>manifest.json?}
    pass1 -- yes --> chain["build contiguous chunk chain from offset 0<br/>(a gap truncates the chain there)"]
    chain --> complete{"chain end ≥<br/>manifest.Size?"}
    complete -- yes --> asm[assemble: GET chunks 6 at a time,<br/>write in order, verify size == declared range,<br/>write .restore-tmp, fsync,<br/>atomic rename]
    complete -- "no — chunks missing<br/>below certified size" --> nextGen[try next-older generation]
    pass1 -- "no (legacy, pre-manifest)" --> defer2[defer to pass 2]
    nextGen --> pass1
    defer2 --> pass2{any manifested<br/>generation seen at all?}
    pass2 -- no --> legacy[newest generation with a<br/>contiguous-from-zero chain wins] --> asm
    pass2 -- "yes, but all were short" --> incomplete([ErrIncompleteReplica —<br/>refuse to restore a fragment])
    legacy -- none --> none([ErrNoReplica])
    asm --> open(["bytdb.Open(destPath)<br/>— replays like a crash-recovered file"])
```

The guarantees this flow encodes:

- **No silent roll-backward.** A freshly rolled generation that has shipped
  only its first chunk is never chosen over a complete older one.
- **No silent fragments.** A certified generation later missing chunks is
  detected (`ErrIncompleteReplica`) rather than restored partially; a chunk
  whose body size disagrees with its declared range is a hard
  "chunk size mismatch" failure.
- **Torn tails are fine.** The chain may extend *past* the certified size —
  extra tail chunks are valid appends, and WAL replay handles any torn final
  record exactly as it would locally.
- **Parallel downloads, ordered writes.** Up to six chunk GETs run at once
  and the file is written strictly in chain order; at most six fetched
  chunks are held in memory (six times `MaxChunkBytes` at worst).
- **Safe beside a live replicator.** If a chosen chunk vanishes mid-restore —
  the replicator pruned that generation or deleted merged-away parts — the
  listing was stale, so Restore lists again and chooses afresh, once.
- **Never over a live database.** Restore takes the database lock on
  `destPath` first and fails with `ErrLocked` if an engine has it open,
  before downloading anything. Renaming the restored file over a live one
  would leave that engine appending to the unlinked original. Stop the
  engine (or restore to a new path) first.

Restoring a database that was opened with `WithEncryptionKey` requires the
same key at `Open` — see [Encryption & Security](security.md). Chunks of an
encrypted database are ciphertext automatically (the replicator ships raw log
bytes; `replicate/encrypt_replication_test.go` asserts no plaintext ever
reaches the store).

## The bundled S3 client

`replicate/s3` is a dependency-free SigV4 client (stdlib only): virtual-host
or path-style addressing, ListObjectsV2 with continuation paging, and real
payload hashing (never `UNSIGNED-PAYLOAD`). Temporary credentials (an IAM
role, STS `AssumeRole`, an instance profile) work through
`Config.SessionToken`, sent as a signed `X-Amz-Security-Token`; the client
does not refresh them, so rebuild it with fresh credentials before they
expire. A listing page marked truncated but carrying no continuation token
is an error, not the end of the listing — both restore and pruning act on
what a listing lacks. Its HTTP client deliberately sets
**no whole-request timeout** — that would cap a large restore's body
transfer — but bounds every stall point: 10 s dial, 10 s TLS handshake, 30 s
response-header wait. A black-holed endpoint fails a ship in seconds and the
next tick retries, rather than wedging the replicator. Supply your own
`Config.HTTPClient` to override.

There is no multipart upload, and chunks are staged in memory — keep
`MaxChunkBytes` (default 8 MB) well under the single-PUT limit.

## Failure-mode matrix

| Event | Behavior |
|---|---|
| Compaction between ships | Epoch differs → new generation from offset 0; old ones pruned |
| Compaction during a chunk read | `ErrEpochChanged` → quiet return; next ship rolls the generation |
| `PUT` fails | Watermark not advanced; the range from the same offset re-ships next tick (longer if the log grew; restore takes the longest chunk and trims overlap) |
| Manifest write fails | Logged; chunks are already durable, manifest retried on next catch-up |
| Prune fails | Logged only; never blocks shipping |
| Source DB closed | `Run` returns `ErrClosed` (terminal) |
| Shutdown (`Close`) | Final flush on a fresh 30 s context, then stop |
| Black-holed endpoint | Dial/TLS/header timeouts fail the ship in seconds; retried next tick |
| Upload stalls mid-body | Per-operation deadline (`OpTimeout + n / MinTransferRate`) cancels it; retried next tick |
| Store cannot list at the first roll | Ship fails (no generation can be numbered); retried next tick |
| Clock steps backwards / replacement host | No effect on ordering: generations are ordered by sequence number |
| Merge fails | Logged; parts are untouched, the merge is retried on a later ship |
| Chunk vanishes mid-restore | Restore lists again and chooses afresh, once |
| Restore, no data | `ErrNoReplica` |
| Restore, only fragments | `ErrIncompleteReplica` |

## Online backup

Two engine APIs complement replication for one-shot snapshots, both consistent
and non-blocking (writers and readers proceed; only compaction is briefly held
off):

- **`Engine.Backup(destPath)`** — writes a point-in-time copy via temp file +
  fsync + atomic rename. Restoring is just `Open` on the copy.
- **`Engine.BackupTo(w io.Writer)`** — streams the same bytes to any writer,
  for direct-to-bucket snapshots without a local temp file; the caller owns
  destination atomicity (upload to a temp key, or rely on the store's atomic
  PUT).

Both capture only fully-appended bytes (`walSize`), so a backup taken while a
write is mid-append simply stops short of the torn record — the copy always
opens cleanly. **Never copy the live file by hand** while the process runs; a
raw `cp` can catch a torn tail mid-append.
