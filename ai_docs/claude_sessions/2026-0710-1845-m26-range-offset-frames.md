# Session: Milestone 26 — RANGE offset window frames

- **Session ID**: `5e8d5804-e63c-493a-8e3f-062004987a80`
- **Date**: 2026-07-10
- **Prior context**: continued from the m25 session doc (explicit
  window frames) on post-`a6caff4` main. User picked `RANGE <offset>`
  frames as Milestone 26.
- **State at end**: implementation complete, all tests green in both
  modules, `go vet`/`gofmt` clean, verified end to end over the wire.
  Implementation + this doc committed and pushed at session close.

## Scope (Postgres 11-faithful)

- `RANGE <offset> PRECEDING/FOLLOWING` bounds: the offset is a
  distance measured on the window ORDER BY key, not a row/group
  count. Completes the m25 frame matrix — only frame `EXCLUDE`
  (beyond NO OTHERS) and window+GROUP BY remain unsupported.
- Requires exactly one ORDER BY column (parse-time, PG wording:
  "RANGE with offset PRECEDING/FOLLOWING requires exactly one ORDER
  BY column") of numeric type (exec-time: "... is not supported for
  column type string/bool/...", since exec has the scope).
- Offsets may be fractional (`RANGE 0.5 PRECEDING`, over int keys
  too — mixed numerics compare); direction flips under DESC; offsets
  must be non-null, numeric, non-negative, non-NaN ("invalid
  preceding or following size in window function", PG's runtime
  wording; null/non-numeric keep the house "frame starting/ending
  offset must ..." style).
- NULL sort keys per PG `in_range`: a NULL row's offset frame is
  exactly its peer group; non-NULL rows never reach NULLs through an
  offset bound; UNBOUNDED bounds from the other side still take them
  in.

## Implementation

- `sql/parser.go`: `frameBound`'s old RANGE rejection became the
  exactly-one-ORDER-BY-column check; `windowFrame` now takes
  `nOrderBy int` (GROUPS check uses `== 0`).
- `sql/ast.go`: FrameMode comment updated (no longer "rejected").
- `sql/window.go` (the core):
  - `frameSpec` grew `startNum, endNum any` — RANGE offsets keep
    their numeric type (int64/float64); ROWS/GROUPS keep the int64
    `startOff/endOff`. New `hasRangeOffsets()` helper gates the RANGE
    paths; `rangeOffset` validates (asFloat, `< 0 || NaN` → invalid
    size); `resolveFrame` branches on it and type-checks
    `exprType(env.sc, w.OrderBy[0].Ex)` ∈ {TInt, TFloat}.
  - The free `frameBounds` function became a `framer` struct
    (fs/size/groups/groupOf + RANGE-offset context: `keys []any`,
    `desc`, non-NULL run `[nnLo, nnHi)`) with a `bounds(n)` method.
    `newFramer` evaluates the sort key per position only when RANGE
    offsets are present; NULLs sort last asc / first desc, so the
    non-NULL run is found by trimming both ends.
  - `rangeEdge(n, off, preceding, end)`: binary search
    (`sort.Search`) over `[nnLo, nnHi)` for the position where the
    key crosses `key(n) ∓ off`; end bounds search strictly-past
    (first key > threshold), start bounds first-not-before (>=);
    DESC negates the comparison. Confining the search to the
    non-NULL run IS the NULL semantics: offset bounds can't reach
    NULLs, UNBOUNDED/clamped bounds on the other side still can. A
    NULL current row short-circuits to its peer-group edges.
  - `rangeThreshold(v, off, sub)`: exact int64 arithmetic with
    saturation to ±Inf float on overflow (off validated ≥ 0, so
    wraparound is detectable by direction; ±Inf compares correctly
    past every key). Any float operand → float math; NaN propagation
    matches cmpFloat's NaN-sorts-last, which lands exactly on PG's
    "NaN within any distance of NaN, and of nothing else" for free.
  - `assignValueFrame`/`assignAggFrame` now take `*framer`; the
    UNBOUNDED PRECEDING one-accumulator fast path still holds for
    RANGE offset ends (sorted keys → monotone thresholds → monotone
    searched edges → hi nondecreasing, including across the NULL-run
    transition at either end).
- `sql/describe.go`: `$n` frame offsets describe as the ORDER BY
  key's type when mode is RANGE (else TInt as before) — pgx encodes
  by described OID, so this is what lets a driver send 0.5 as float8
  instead of erroring/truncating.
- `sql/params.go` needed no change — m25's Frame deep-copy + offset
  binding already covers RANGE offsets.

Tests: 4 new funcs in `sql/window_frame_test.go` —
TestFrameRangeOffsets (ties/gaps vs ROWS, DESC, MaxInt64 saturation,
fractional offset over int key), TestFrameRangeFloat (float key,
value functions), TestFrameRangeNulls (peer-only NULL frames,
UNBOUNDED taking NULLs in, DESC nulls-first), TestFrameRangeParams
($n binding + describe inference float/int); +7 error cases in
TestFrameErrors (0/2 ORDER BY cols, text/bool key via `k::text` and
`k > 1`, negative/null/'x' offsets). Stale RANGE-rejection
expectations updated in window_frame_test.go and
window_cover_test.go.

## Docs

- README: status → Milestones 1–26 (RANGE offsets named); Milestone
  26 roadmap entry; Later list now order-aware index selection,
  frame EXCLUDE, standalone CREATE SEQUENCE.
- `docs/features.md`: frame section gained a `last_5min`
  (`RANGE 300 PRECEDING` over an epoch key) example and the RANGE
  offset rules (one numeric column, fractional, DESC, NULL peers,
  $n describes as key type).
- `docs/gotchas.md`: unsupported row narrowed to frame EXCLUDE +
  window+GROUP BY.

## Verification (wire-level)

Same recipe as m24/m25 (`.claude/skills/verify/SKILL.md`): built
`pgwire/cmd/bytdbd`, served a scratch db on 127.0.0.1:5439, drove it
with a pgx v5 client. Confirmed int/float RANGE offset frames, DESC,
and NULL-peer frames over the simple protocol; `$1 preceding`
fractional offsets over the extended protocol (float8 describe
inference is load-bearing — pgx encodes by described OID); and clean
wire errors for four rejection paths.

## Gotchas / notes for next time

- Milestone numbering: 26 = RANGE offset frames. Next arc starts
  at 27.
- `exprType` falls back to TString for unresolvable expressions, so
  the "not supported for column type X" message can say "string" for
  odd exprs — acceptable, PG-shaped.
- ORDER BY key type is checked statically (exprType), offset value
  dynamically; both once per window in resolveFrame, so errors
  surface even over zero rows (same as m25 offsets).
- The framer's `[nnLo, nnHi)` trim loops run on both ends
  unconditionally — NULLs are contiguous at exactly one end, so this
  is direction-agnostic and robust.
- assignAggFrame's UNBOUNDED fast path depends on hi being
  nondecreasing; that argument now covers RANGE offsets (monotone
  thresholds over sorted keys) — revisit if EXCLUDE lands, since
  per-row exclusion breaks contiguous `[lo, hi)` anyway.
- Still open on the window arc: frame `EXCLUDE CURRENT
  ROW/GROUP/TIES`, window + GROUP BY, DISTINCT in window calls.
- Still open elsewhere: standalone `CREATE SEQUENCE`, `pg_attrdef`
  rows (psql `\d` Default column), order-aware index path selection.
