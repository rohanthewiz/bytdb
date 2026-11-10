# Deep-look remediation plan: complete

Session: `2c5b1603-fa95-4c81-b037-0bcdc19533ac`. This is the wrap-up of the
session that implemented `ai_docs/todo/2026-1109-deep-look-remediation-plan.md`
(D-01 to D-62, Phases 0–9), taking the plan's recommendation on each of its
six decisions (Q1–Q6). Each stretch has its own doc:

| Doc | Covers |
|---|---|
| `2026-1109-2243-deep-look-phase0-phase1` | Phase 0 (difftest corpus, CI, baselines) and Phase 1 |
| `2026-1109-2330-deep-look-phases-2-to-4` | Typed bind pass, subqueries, atomicity |
| `2026-1109-2355-deep-look-phases-5-6` | Context-bounded lock waits, replication and server hardening |
| `2026-1110-0512-deep-look-phase7-part1` | Performance, first half |
| `2026-1110-0555-deep-look-phase7-part2-phase8-start` | Performance, second half; D-54, D-55 |
| `2026-1110-0700-deep-look-phase8-d52-d53-d51` | Everyday SQL, functions, aggregates, session surface |
| `2026-1110-0737-deep-look-phase8-d51-d56` | Catalog surface, protocol negotiation, Go API |
| `2026-1110-0757-deep-look-phase8-close` | Driver, bytdbd, Phase 9, repro cleanup |

## Where it stands

- Every phase is on `main` and pushed; the last code commit is `b32913a`.
  Nothing is tagged. The release waits for the user's go-ahead (N-044),
  together with a btypedb v0.10.0 tag for D-48's faster replay (btypedb
  `main` `ad77fd8`).
- Unit tests, the Postgres 16 difftest corpus (files 70–76 added this
  session), vet and staticcheck pass across the root module, `sql/difftest`
  and `pgwire`. pgwire's `GOWORK=off` build stays allowed-to-fail until the
  root is tagged, as before.
- The audit's repro modules were run against `main` before deletion: every
  test passes, is refused by its fix (with a ported test), or only measures.
  Restore with `git checkout 2388d34 -- ai_docs/todo/deep-look-repros`.

## Release-note items collected in N-044

- Error messages use Postgres's wording (`relation "t" does not exist`,
  `duplicate key value violates unique constraint "t_pkey"`, `syntax error at
  or near "x"`); match on the new `errors.Is` sentinels, not text.
- The database/sql driver scans `date` as `time.Time` (midnight UTC).
- A top-level `oid::regclass` renders the relation name, typed text.
- Bare `user` / `current_user` read as the session user.
- Newly refused shapes from the typed bind pass (`1 < 'a'`, `timestamp + 1`,
  `integer || integer`, `1 = true`), CHECKs that start being enforced (run
  `VACUUM (VERIFY)` after upgrading), and the implicit transaction for
  multi-statement batches (D-32).

## Open after the plan

- N-044: the release decision.
- N-045 to N-047: performance pieces deferred with reasons (typed-tree cache,
  shared read snapshot needing a btypedb release, catalog row memo, identity
  batching).
- N-048: `numeric` / `decimal` columns, which need the user's choice between
  a lossy float8 alias, a better refusal, and a real decimal type.
- N-049: capturing GORM v2, golang-migrate and atlas statements verbatim,
  which needs network access to their sources.

## Next

Closed: None. Declined: None. Raised: None. Deferred: None. Promoted: None.
Moved: None. Updated: None. Full list: `ai_docs/todo/next-list.md`.
