# Session: cold-run flake retired via captured cold repro; verify.mjs spawn-race fix applied in go-learn

- **Session ID:** `8439cf9a-addb-4091-8bae-93a00d35a00d`
- **Date:** 2026-07-31, 23:58
- **Deliverables:** bytdb main `088e0b4` + `f53a77e` (session-doc
  updates closing both open items); go-learn master `712ec9e`
  (verify.mjs hardening). Both repos pushed this session.

## Context

The two remaining open items from the v0.7.0 release / go-learn bump
sessions were: (1) the uncaptured bytdb root-test cold-run flake, and
(2) verify.mjs spawn-race hardening in go-learn. Both were confirmed
real and documented (validity check of a summary claim), then closed.

## Item 1: cold-run flake — retired, did not reproduce

Deliberate cold repro under the same conditions as the original
failure, but with output captured this time:

- `go clean -cache` (full build-cache wipe — also clears cached test
  results, so everything re-ran), then `GOWORK=off go test ./...`
  tee'd to a log.
- Result: **all 6 packages green, exit 0**, no FAIL/panic anywhere.
  Root package 15.4s, sql 9.6s — the long times confirm it genuinely
  ran cold (the original failure was blamed on CPU starvation during
  cold compile of the dep tree).
- Verdict: 9-for-10 lifetime (the single original failure never
  named its test); retired as non-reproducing. If it ever recurs:
  capture the FULL log — do not pipe to `tail` (that's how the test
  name was lost the first time).

Doc updates (commit `088e0b4`): retirement addendum on the flake note
in `2026-0731-1249-occ-release-and-reproducible-benchmarks.md`;
strike-through in the Open list of
`2026-0731-1418-golearn-v070-bump-yaegi-extract.md`.

## Item 2: verify.mjs spawn-race hardening — applied (go-learn `712ec9e`)

The fix suggested in the 1418 doc, now real in go-learn
`verify/verify.mjs` `run()`:

- `execFileSync(bin, ..., { timeout: 30_000 })` + exactly one retry on
  timeout. Items finish in ~10–25ms, so a 30s timeout can only mean
  the stuck-stdin deadlock (~1-in-500 spawns: child never receives
  its stdin pipe, parks in `read()` forever).
- Two deliberate details:
  1. The timeout check comes **before** the pre-existing exit-2
     `e.stdout` parse — a SIGTERM'd child can leave partial stdout
     that would otherwise be misparsed as an interpreter error.
  2. A second consecutive timeout **throws loudly** ("not the spawn
     race, investigate") instead of retrying forever.
- Timeout detection: `e.signal === 'SIGTERM' || e.code ===
  'ETIMEDOUT'` (node version differences in how the kill surfaces).
- Validated: `node verify/verify.mjs database` — static checks 27
  tracks + all 16 database items ALL PASS.

Doc update (commit `f53a77e`): 1418 doc's Open list now marks the item
applied with the go-learn commit hash.

## State

- bytdb main: `088e0b4`, `f53a77e` + this doc — pushed.
- go-learn master: `712ec9e` — pushed (own session doc there too).
- Open items from the release/bump sessions: **none remaining**
  (`.cats-todo/` stays untracked in both repos by design).
- Unrelated pending (from earlier memory, untouched this session):
  btypedb.Update recover, pgwire DoS hardening, replicate
  restore-completeness.
