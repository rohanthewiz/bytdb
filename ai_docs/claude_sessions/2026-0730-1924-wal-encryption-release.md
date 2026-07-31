# WAL encryption release: btypedb v0.6.2 → bytdb/pgwire v0.6.4

- **Session ID:** `686d0694-6ff9-4a07-88ee-af201ca7e138`
- **Date:** 2026-07-30
- **Scope:** Cut the release for the value-only WAL encryption feature and
  document it. Continuation of the implementation session
  (`2026-0722-1303-wal-encryption-at-rest.md`), which had committed+pushed the
  code but left it untagged.

## What shipped (versioning)

Full release chain, all pushed:

| Module | Tag | Commit |
|--------|-----|--------|
| btypedb | `v0.6.2` | `f67e2b6` (the encryption commit) |
| bytdb   | `v0.6.4` | `f5253cd` |
| pgwire  | `pgwire/v0.6.4` | `f5253cd` |

User chose **v0.6.2** for btypedb (patch, not v0.7.0) — the change is
backward-compatible: encryption is opt-in and the plaintext path is
byte-for-byte unchanged.

## Steps

1. **btypedb v0.6.2** — lightweight tag on `f67e2b6`, pushed.
2. **Repin** — `GOWORK=off GOPRIVATE=github.com/rohanthewiz/btypedb go get
   btypedb@v0.6.2` in both bytdb root and pgwire; bumped btypedb `v0.6.1 →
   v0.6.2` in both go.mod + go.sum. Verified both modules build with
   `GOWORK=off` (the published-consumer path, not the workspace copy). Pin-bump
   commit `9c3a3d7`, pushed.
3. **Docs** (user interrupted the tagging to ask for this) — added an
   "Encryption at rest" section to both `README.md` and the bytdb skill
   (`.claude/skills/bytdb-fast-memory-based-db/SKILL.md`), plus a Status-line
   mention and a skill-description tweak. Covers: `WithEncryptionKey`, value-only
   scope (PK stays cleartext), per-record `nonce ‖ AES-256-GCM(value)` seal, v2
   header with shared 16-byte compat prefix + key-check value, ciphertext that
   flows to replicas/backups, `ErrWrongKey`/`ErrKeyRequired`/`ErrNotEncrypted`,
   no in-place conversion or online rotation yet, and the bytdbd
   `-encryption-key-file` / `-encryption-key-env` flags.
4. **bytdb v0.6.4 + pgwire/v0.6.4** — release commit `f5253cd` bundled the doc
   updates + pgwire's bytdb pin bump `v0.6.3 → v0.6.4`; both tags placed on that
   one commit (matching the prior release pattern where root + pgwire tag the
   same commit, pgwire's `replace => ../` resolving bytdb locally). Pushed commit
   + both tags.

## Final pin state

- root go.mod → btypedb `v0.6.2`
- pgwire/go.mod → btypedb `v0.6.2`, bytdb `v0.6.4` (+ `replace => ../`)
- Latest tags: btypedb `v0.6.2`, bytdb `v0.6.4`, pgwire/`v0.6.4`

## Notes for next time

- Release flow reminder: tag btypedb first, `GOWORK=off` `go get` to repin per
  module, tag bytdb root + pgwire together on the pin-bump commit.
- Still deferred on the feature itself (from the impl doc): online key rotation
  (needs a v3 wrapped-DEK header — `Compact`'s raw tail-copy is invalid across
  keys), plaintext↔encrypted migration helper, key+value scope, ChaCha20.
