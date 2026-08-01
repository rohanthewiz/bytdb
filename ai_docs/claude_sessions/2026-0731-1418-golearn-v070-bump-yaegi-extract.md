# Session: go-learn bumped to bytdb v0.7.0 + yaegi re-extract; branch cleanup; verifier flake diagnosed

- **Session ID:** `819fa68d-114d-4da4-ac84-31a285a6439b`
- **Date:** 2026-07-31, 14:18
- **Deliverables:** occ branches deleted (local + origin, both repos);
  go-learn `master` @ `6f8d4a2` (pushed): pins → v0.7.0, yaegi symbols
  re-extracted, all 27 tracks / 538 items verified green.
- **Prior docs (same session):**
  `2026-0731-0846-benchmarks-head-to-head-readme.md`,
  `2026-0731-1249-occ-release-and-reproducible-benchmarks.md`.

## Branch cleanup

`git branch -d` (merge-verified) + `git push origin --delete`:
bytdb `occ-stage2` (was `259d855`) and btypedb `occ-stage1` (was
`3692710`) removed locally and on origin. Both repos are now main +
tags only.

## go-learn: pins → v0.7.0 + re-extract (closes the compat-check caveat)

- `go get bytdb@v0.7.0 btypedb@v0.7.0` — clean bump from the July 12
  pseudo-version / btypedb v0.5.0.
- `wasm/symbols/gen.sh` re-run: **only the two bytdb extract files
  changed** (+44/−19); every stdlib + go-styl extract regenerated
  byte-identical (good determinism check). New symbols = everything
  since July 12: OCC package-level surface (`ErrTxConflict`,
  `WithConcurrentWrites`) plus the v0.6.x API (jsonb/text[] helpers,
  LIKE ops, `FKCascade`, timestamp/UUID formatters, `AlterOwner`,
  `CanonJSONB`, ...). Methods (`BeginSerializable`, `TrackReads`,
  `WriteTxnSerializable`) need no named entries — they ride the
  `Engine`/`Txn` reflect type entries.
- `./build.sh`: go-learn.wasm rebuilt (22M, js/wasm, untracked build
  output); `go build ./...` green. go-learn has no Go unit tests —
  verification is `node verify/verify.mjs`.
- Committed `6f8d4a2` on **master** (go-learn uses master, not main),
  pushed.

## Verifier hang: diagnosed as pre-existing infra flake, NOT the bump

First full `verify.mjs` run hung. Diagnosis chain worth remembering:

1. node: 2.75s CPU in 15min → idle. `sample <pid>`: blocked in
   `SyncProcessRunner::Run → uv__io_poll → kevent` (inside
   execFileSync, normal-shaped wait).
2. child `runner`: **0:00.00 CPU since spawn**, `sample` shows main
   thread parked in `read` — it never received stdin. Deadlock: node
   waits for child exit, child waits for input that never arrives.
3. Same runner binary executes a trivial program in 0.1ms → binary
   healthy.
4. Hang position MOVED across 4 occurrences (after zig; twice right
   after `ios/optionals-chaining`; once in docker) — none of those
   tracks import bytdb; each hung item passes standalone
   (`ios/value-vs-reference` 5/5). The `database` track (the actual
   bytdb consumer: sequences-identity, wal-recovery, mvcc-snapshots,
   ...) passed **100% on every run**.

Conclusion: a rare `execFileSync` spawn race (child gets no stdin) at
~1-in-500 spawns, biting mostly on fast tracks (~1ms/item spawn
cadence). Pre-existing; unrelated to the pins.

**Final coverage:** all 27 tracks / 538 items green — 21 tracks + most
of ios in one full run, remainder (database, aiml, stats, zig, k8s,
docker, full ios) via scoped runs (`node verify/verify.mjs <track>`),
docker and ios needing one retry past the flake.

**Suggested fix for go-learn (not applied):** per-spawn timeout + one
retry in verify.mjs's `run()` (execFileSync accepts `timeout`); would
make full runs reliable. Watchdog pattern used here:
`(cmd & VPID=$!; (sleep N && kill $VPID) & ...)` since macOS lacks
`timeout(1)` in this shell.

## State

- go-learn `master` @ `6f8d4a2` pushed: bytdb v0.7.0 + btypedb v0.7.0,
  fresh symbols. The Database track can now teach the OCC API if a
  future lesson wants it (symbols present).
- bytdb main `8fd5c49` + this doc; btypedb main `3692710` (= v0.7.0);
  no occ branches anywhere.
- Open: verify.mjs spawn-race hardening (above); `.cats-todo/`
  untracked in both repos by design.
- ~~The uncaptured bytdb root-test cold-run flake~~ — **retired
  2026-07-31**: deliberate cold repro (`go clean -cache`, full suite,
  output tee'd) came back all green; see addendum in
  `2026-0731-1249-occ-release-and-reproducible-benchmarks.md`.
