# Session: yaegi/wasm compatibility check — will OCC bytdb still work in go-learn?

- **Session ID:** `bea4ff32-7478-4811-bec7-16f6f69be5b2`
- **Date:** 2026-07-31, 07:52
- **Type:** analysis only — no code changes in bytdb, btypedb, or go-learn.
- **Prior session:** `2026-0731-0313-occ-stage4-wire-surface.md` (OCC Stage 4,
  completing the 4-stage concurrent-writes plan; bytdb `occ-stage2` @ `b74fc5f`).

## Question

The sibling project `~/projs/go/go-learn` is a browser Go playground: the Go
toolchain compiles a runner to WASM, and user code is interpreted by yaegi
with bytdb exposed through pre-extracted symbol files. Will bytdb still work
there after the OCC (concurrent writes) work?

## Answer: yes, on both counts

**Nothing changes today.** go-learn does not use the local go.work workspace —
its own `go.mod` pins release versions: bytdb pseudo-version
`v0.0.0-20260712062454-861153926ff9` (2026-07-12, pre-OCC / pre-v0.6.x) and
btypedb `v0.5.0`. The OCC branches cannot affect the playground until those
pins are deliberately bumped.

**When the pins are bumped to the OCC code, it still works.** Verified:

1. **js/wasm build** — `GOOS=js GOARCH=wasm go build ./...` on bytdb
   `occ-stage2` (btypedb `occ-stage1` via go.work) compiles cleanly; no
   OS-specific code was introduced by the OCC work.
2. **yaegi symbols stay valid** — go-learn exposes bytdb to interpreted code
   via `yaegi extract`-generated files
   (`go-learn/wasm/symbols/github_com-rohanthewiz-bytdb*.go`: root, sql, and
   tuple packages). These reference exported names by reflection, so they only
   break if a name is *removed*. Compiled the actual symbol files from
   go-learn against the local OCC branch for js/wasm (scratchpad module with
   `replace` directives, `GOWORK=off`) — clean. OCC was purely additive
   (`ErrTxConflict`, `WithConcurrentWrites`, `BeginSerializable`,
   `TrackReads`, `Engine.ConcurrentWrites`); nothing renamed or dropped.
3. **Behavior unchanged by default** — concurrent writes are opt-in via
   `WithConcurrentWrites()` (`bytdb/engine.go:471`); without it the engine
   stays single-writer, which is what go-learn's database track exercises.
4. **Safe under wasm's cooperative scheduler** — js/wasm is single-threaded
   and hangs on busy-wait loops, but the OCC commit path in btypedb has no
   spin loops; it blocks on ordinary mutexes, which park goroutines correctly
   on wasm.

## Caveat for later

The new OCC API is not *visible* to interpreted playground code until
`yaegi extract` is re-run on the upgraded packages — the current symbol files
predate OCC. Only matters if a future track should teach serializable
isolation / concurrent writes; existing tracks are unaffected.

## State

- bytdb `occ-stage2` @ `b74fc5f`, btypedb `occ-stage1` — both unchanged this
  session. Release/merge/tagging decisions from Stage 4 still pending.
- Untracked `.cats-todo/` remains the user's local tooling; left uncommitted.
