# WAL Compression Analysis (Decision: Not Now)

**Date:** 2026-07-30
**Status:** Deferred — keeping the WAL format simple. Revisit if workloads skew toward multi-KB jsonb/text documents or storage/replication bandwidth becomes a pain point.

## Question

Should btypedb's WAL support value compression (mutually exclusive with encryption)? A ratio experiment on representative jsonb data was run to decide before committing any format bytes.

## Design sketch (if ever built)

Compression would sit at the same seam encryption uses — `appendSealedRecord` / `openRecord` in `btypedb/encrypt.go` — as a value-payload transform that leaves op/klen/vlen framing, the key, and the trailing CRC untouched. That preserves crash recovery, `scanForRecord`'s torn-tail-vs-bitrot discrimination, and the byte-range replication protocol (`ReadLogRange` is a bare `io.Copy`, so compressed records would ship to S3 compressed for free).

- **Per-record tag, not an op change:** 1-byte tag prefixed to the value (`0` = raw, `1` = compressed), mirroring the `opSetTTL` deadline-prefix precedent. Raw fallback caps worst case at +1 byte. Do not widen the op range — `readRecord`/`scanForRecord` rely on the tight `[1,4]` op check.
- **Header:** v2 flags word already reserves bits (encrypted = bit 0, cipher id bits 1–4, scope bit 5). Compressed-plaintext = v2 header, encrypted bit clear, compression bit + algorithm id set. No key → header stays a compile-time constant, preserving torn-header-as-strict-prefix detection. KCV field should become conditional on the encrypted flag.
- **Mutual exclusion with encryption enforced at `Open`:** ciphertext is incompressible, and compress-before-encrypt leaks content-dependent length (CRIME/BREACH class). Configuring both = error.
- **Rollout ordering:** followers must upgrade to a compression-aware binary before the primary enables it (`ErrNewerFormat` fail-fast, same as encryption's v2).
- **Ruled out:** block/frame compression across records (breaks record framing and `scanForRecord` re-synchronization) and whole-file compression (breaks random access, truncate-on-recovery, and replication offsets).

## Experiment

Standalone harness (scratchpad, session 2026-07-30): built a real bytdb database via `Engine.CreateTable`/`Insert` so values are actual tuple-encoded row bytes, then re-parsed the WAL framing and compressed every value payload independently.

Corpus: 3,000 tiny attr docs (~80 B), 5,000 telemetry events (~340 B), 3,000 user profiles (~500 B), 1,200 orders with line items (~2.7 KB). WAL = 4.96 MB; value payloads = 90.2% of file bytes. Fixed seed (42), `klauspost/compress` v1.17.11, zstd frame CRC disabled (WAL CRC already covers the bytes). Dictionary = 16 KB `dict.BuildZstdDict` trained on every 8th value; flate dict = ~32 KB concatenated samples via `flate.NewWriterDict`.

### Compressed size as % of raw, by value size bucket

| bucket | count | s2 | zstd-1 | zstd-3 | zstd-3+dict | flate-6 | flate-6+dict |
|---|---|---|---|---|---|---|---|
| < 128 B | 3,004 | 103.6% | 115.7% | 110.9% | **59.7%** | 100.4% | 16.3%* |
| 128–255 B | 4 | 88.0% | 80.1% | 77.6% | 75.0% | 72.8% | 71.9% |
| 256–511 B | 8,000 | 98.5% | 97.1% | 90.9% | **42.8%** | 78.3% | 47.2% |
| 512 B–1 K | 393 | 68.8% | 53.4% | 51.8% | **32.1%** | 50.3% | 46.6% |
| 1–4 K | 807 | 51.1% | 38.8% | 37.8% | **25.8%** | 36.0% | 33.7% |

\* flate+dict on tiny docs is inflated by overfitting — the raw concatenated-sample dictionary contained near-duplicates of the generator's tiny docs.

### Whole-file projections (framing/keys/CRCs stay raw)

| scheme | WAL file size vs original |
|---|---|
| per-record zstd-3, threshold 256 B | 76.0% |
| per-record zstd-3 + trained dict, no threshold | **44.7%** |
| whole-file zstd-3 (incompatible upper bound) | 17.3% |

### Encode throughput (single core, per-record blocks)

s2 ~180 MB/s · zstd-1 ~205 MB/s · zstd-3 ~140 MB/s · zstd-3+dict ~28 MB/s · flate-6 ~16 MB/s · flate-6+dict ~6 MB/s

## Findings

1. **Plain per-record compression is a weak win (~24% file reduction).** The dominant bucket (256–511 B typical jsonb rows) barely compresses in isolation (zstd 91%, s2 99%): a single small JSON document is mostly UUIDs, timestamps, and short enums. The redundancy lives *across* records (repeated field names, shared vocabulary), unreachable per-record.
2. **A shared dictionary recovers most of the gap: file → 45% (2.2×)** while keeping per-record framing. Even sub-128 B values compress (60%), making the size threshold unnecessary (T=0 beat T=256).
3. **Dictionary costs:** the dict must be persisted + versioned in the file (natural home: a dictionary block after the header, retrained at `Compact` from live rows); klauspost's dict-assisted zstd encoder runs only ~28 MB/s single-core (Go-implementation weakness, likely still fine behind group commit). Untested follow-up: zstd-1 + dict may recover much of that speed.
4. **"3–10× for jsonb" intuition only holds** for multi-KB documents with internal repetition (orders: 2.6× per-record) or block compression.

## Decision

Leave compression out. The compelling version is the compaction-trained dictionary scheme (~55% savings), which is a substantially bigger format commitment than a tag byte — not worth it while priorities are elsewhere. If the format is ever touched for other reasons, reserve a compression algorithm id + dictionary id in the v2 flags word so the dictionary scheme can slot in without another version bump.
