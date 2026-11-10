# Deep-look plan: Phase 0 (safety net) and Phase 1 (small fixes)

Session: `2c5b1603-fa95-4c81-b037-0bcdc19533ac`

Asked to implement `ai_docs/todo/2026-1109-deep-look-remediation-plan.md`.
This doc covers the first checkpoint: Phase 0 and all of Phase 1, which is
the plan's "release A" content. Work continues in the same session with
Phase 2.

## Decisions

- **Q1–Q6 taken at the plan's recommendations.** The request was to
  implement the plan as written, and the plan says it assumes them.
- **D-01 done test-first per fix, not as a batch of `Skip` lines.** With the
  fixes landing in the same session, parking each repro behind a `Skip`
  first and deleting it minutes later bought nothing. Every Phase 1 commit
  carries its repro as a test, and each was checked to fail on the old code
  (several by running the new test against `HEAD` in a scratch worktree).
  Repros for later phases stay in `ai_docs/todo/deep-look-repros/`.
- **D-06 bounds depth, not a statement-wide operator count.** The plan
  suggested counting chained operators per statement. That would refuse a
  20,000-row `INSERT` with a `$n::int` per cell, which is a real shape.
  Depth is what costs stack, so each expression-level parse step reports
  the depth it built (`parser.lastDepth` / `measure`). Long OR/AND lists
  are n-ary and stay shallow.
- **D-09 narrower than "reject LIMIT/OFFSET in any subquery".** The existing
  suite showed the runners get some shapes right, and applications use them
  (`EXISTS (... LIMIT 1)`, `ARRAY(SELECT x ... ORDER BY x)`, LIMIT/OFFSET
  without ORDER BY). Only the shapes answered wrongly are refused.
- **D-17 renders rather than refuses for text columns.** A non-string value
  under a text-described column (today's "described as text" fallback for
  `coalesce`, `nullif`, ...) is sent as its text rendering in both formats.
  Refusing it would have broken many working queries before Phase 2 lands;
  rendering also fixes binary-format clients. Other contradictions refuse.
- **D-19: `Row.Desc` stays the shared descriptor.** A deep copy per `Get`
  or scanned row costs more than it buys. `Engine.Table`, `Txn.Table` and
  `ReferencingFKs` return deep copies; `TableDesc.Clone` is exported; DDL
  builds on a fresh parse of the stored bytes (`descForDDL`), so a mutation
  through `Row.Desc` can no longer be persisted.
- **D-16 needed D-40 step 2 early.** `Engine.CatalogVersion` (bumped after
  every DDL commit; readers load it before resolving the catalog) is what
  pgwire revalidates prepared statements against.
- **No release tagged.** Tagging publishes to the module proxy for good;
  raised as N-044 for the user's go-ahead.

## Findings worth keeping

- The differential harness confirmed 70 of the plan's "Postgres gives"
  claims against Postgres 16.6, and corrected one: `2 || 3 + 4` is an error
  in Postgres too (`integer || integer` does not exist). The precedence fix
  was still right; the remaining divergence is typing (D-23).
- Postgres agrees with three behavior changes that older tests had pinned
  the other way: `x = NULL` returns no rows, `-9223372036854775808 / -1` is
  "bigint out of range", and `body -> 'tags' - 'go'` needs parentheses.
- pgwire's `GOWORK=off` CI job is red until the next root release (pgwire
  uses `Session.Fail`, `DB.CatalogVersion`, `PeekIdentity`); it is marked
  allowed-to-fail on purpose.
- A pre-fix test run left a `git stash` applied while it hung in the
  background (the old `Serve` exited, so the client waited forever). It was
  stopped and the stash restored cleanly; later old-code checks ran in a
  scratch `git worktree` instead.

## Commits

`db481b4` D-02, D-03 · `cf0203f` D-13 · `f5eb022` D-11 · `b66df75` D-12 ·
`66fc7a2` D-19, D-20 · `9b2845b` D-05 · `872a73e` D-06 · `4a9de51` D-07 ·
`c3cf8a1` D-08 · `3ef07a3` D-09 · `8957fd2` D-10 · `bcbad02` D-14, D-15 ·
`ceea2cb` D-16 · `1273e88` D-17 · `b2fcc1f` D-18 · `e4cc6f1` D-21 ·
`bbb5952` D-22 · `4f0beeb` D-04 · btypedb `8eda6b0` D-03.

## Next

Closed: N-035, N-036. Declined: None. Raised: N-044. Deferred: None.
Promoted: None. Moved: None. Updated: N-037, N-038, N-042.
Full list: `ai_docs/todo/next-list.md`.
