---
name: release
description: Cut a bytdb release — tag the root module and pgwire in lockstep (vX.Y.Z and pgwire/vX.Y.Z), bump pgwire's bytdb pin, and tidy bench/go.mod so the benchmarks keep building. Use when asked to release, tag, or bump the version.
---

# Releasing bytdb

The repo holds three Go modules. Each one is released differently:

| module | path | released? |
|---|---|---|
| `github.com/rohanthewiz/bytdb` | `.` | tagged `vX.Y.Z` |
| `github.com/rohanthewiz/bytdb/pgwire` | `pgwire/` | tagged `pgwire/vX.Y.Z`, in lockstep with root |
| `github.com/rohanthewiz/bytdb/bench` | `bench/` | never tagged, but its pin must follow the release |

pgwire and bench both `require` bytdb at a version. Inside a workspace
(the root `go.work`, and bench's nested one), every build uses the working
tree whatever the pins say, so a stale pin doesn't show up locally. Outside
it (`GOWORK=off`), the pins decide:

- **pgwire has no `replace`, on purpose** (next-list N-031). pgwire ships
  the `bytdbd` and `bytdb` commands, and the Go toolchain refuses
  `go install …@version` and `go run …@version` of a module whose go.mod
  carries a `replace`. A `GOWORK=off` build of pgwire therefore compiles
  against the *released* bytdb its pin names, which is what an installing
  user gets. A pgwire change that needs unreleased root API builds in the
  workspace but fails with `GOWORK=off` until the root is tagged and the
  pin bumped (steps 3–4).
- **bench keeps `replace … => ../`** for bytdb and `=> ../pgwire` for
  pgwire. Nothing installs bench, so the `replace` costs nothing and keeps
  `bench/run.sh` on the working tree. Its pin still has to follow the
  release: `bench/run.sh`'s `go run` under `set -e` fails with "updates to
  go.mod needed" when the pin lags.

Release steps 4–7 exist for those reasons.

## Choose the version

- **Minor** (`v0.X+1.0`) when anything since the last tag adds behavior:
  new SQL, new catalog rows, new SQLSTATE mappings, new API.
- **Patch** (`v0.X.Y+1`) for fixes only.

Read `git log --oneline <last tag>..HEAD` to decide, and state the reason
in the root tag message. The tag lines run in lockstep: root and pgwire
always carry the same version. The missing pgwire `v0.10.0` / `v0.11.0`
tags stay unbackfilled by decision (next-list N-012).

## Steps

1. **Start clean and current.** Run `git fetch --tags` and make sure you
   are on `main`, level with `origin/main`, with a clean tree. Work that
   belongs in the release gets committed first. bytdb is worked on from
   more than one machine, so never tag a stale checkout.
2. **Test both released modules.** Run `go vet ./...` and `go test ./...`
   at the root and again in `pgwire/`. Everything must be green before
   anything is tagged.
3. **Tag root and push.**
   ```bash
   git tag -a vX.Y.Z -m "vX.Y.Z: <what the release adds>" HEAD
   git push origin main && git push origin vX.Y.Z
   GOWORK=off GOPROXY=https://proxy.golang.org go list -m github.com/rohanthewiz/bytdb@vX.Y.Z
   ```
   The proxy check has to resolve before step 4. pgwire's pin names this
   tag, and `GOWORK=off` builds fetch it from the proxy.
4. **Bump pgwire's pin.** In `pgwire/`:
   ```bash
   GOWORK=off go get github.com/rohanthewiz/bytdb@vX.Y.Z && GOWORK=off go mod tidy
   ```
   Both `go.mod` and `go.sum` change. With no `replace`, `go.sum` carries
   bytdb's checksums, and `go get` also raises any dependency the new
   bytdb needs at a higher version (btypedb, serr). When `GOPRIVATE`
   covers `github.com/rohanthewiz/*`, the fetch goes to GitHub rather than
   the proxy. GitHub has the tag since step 3.
5. **Tidy bench.** Run `cd bench && go mod tidy`. The bench pin rises to
   `vX.Y.Z` because bench requires pgwire, and pgwire now requires
   `vX.Y.Z` (minimum version selection). That is why this step comes after
   step 4 and not before it.
6. **Build both ways.** Run these in `pgwire/` and in `bench/`:
   ```bash
   go build ./... && GOWORK=off go build ./...
   ```
   Also run `go test ./...` in `pgwire/`. A `GOWORK=off` failure here is
   the stale-pin symptom this routine exists to prevent.
7. **Commit, tag pgwire, push.** Put the pgwire and bench changes in one
   commit, `pgwire, bench: bump bytdb to vX.Y.Z`. Then:
   ```bash
   git tag -a pgwire/vX.Y.Z -m "pgwire vX.Y.Z: tracks bytdb vX.Y.Z; <pgwire-side changes, if any>" HEAD
   git push origin main && git push origin pgwire/vX.Y.Z
   GOWORK=off GOPROXY=https://proxy.golang.org go list -m github.com/rohanthewiz/bytdb/pgwire@vX.Y.Z
   ```
8. **Record it.** In `ai_docs/todo/next-list.md`, close any item the
   release was waiting on. The session doc (`/sess-save`) names both tags
   and the commits they sit on.

## Gotchas

- **Tags are public once pushed.** The module proxy caches a version
  permanently, so a wrong tag can't be moved. Fix forward with the next
  patch version instead.
- **Tag root before bumping pgwire.** If pgwire names a version that
  doesn't exist yet, every `GOWORK=off` consumer of pgwire breaks.
- **Never add a `replace` back to `pgwire/go.mod`**, not even for local
  convenience: `go.work` already gives that, and a `replace` in a tagged
  pgwire breaks `go install …/cmd/bytdb@vX.Y.Z` for that version for
  good (N-031). Up to `pgwire/v0.22.0`, every pgwire tag has one.
- `bench/` has no LICENSE and no tag on purpose. It is an internal
  harness that nothing imports (next-list N-015).
