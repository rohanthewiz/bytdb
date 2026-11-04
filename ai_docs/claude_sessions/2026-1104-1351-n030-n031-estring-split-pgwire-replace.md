# N-030 and N-031: E-string splitting, pgwire without a replace

- **Session:** `91a4689b-1716-48c9-add4-1494c0e1f385`
- **Date:** 2026-11-04
- **Scope:** `/work-next-list`, first batch. Triage of the Next list:
  N-030, N-031 and N-032 doable; N-033 skipped (hands-on Linux and Windows
  terminal runs; the Docker daemon is not running here and there is no
  Windows host). This doc covers N-030 and N-031. N-032 and the newly
  raised N-034 follow in the next batch.

## 0. Workspace repair

`go test` in `pgwire/` failed at first with `undefined: btypedb.ErrLocked`
and `LogStats undefined`. The sibling `~/projs/go/btypedb` checkout was 3
commits behind its own `origin/main` (`49d0e36`, `902e317`, `2383665`), and
the root `go.work` uses `../btypedb`. The tree was clean and `origin/main`
was exactly `v0.9.0`, the version bytdb pins, so it was fast-forwarded
(`git merge --ff-only origin/main`). Nothing in btypedb was changed or
pushed.

## 1. N-030: the simple-query splitter and `E'...'` strings

`splitStatements` (`pgwire/split.go`) treated every `'` as a standard-string
quote, so `\'` inside an escape string closed it early and the `;` after it
split the statement. psql sends everything through the simple protocol.

- Each quoted region now goes through `skipQuoted`. An `E`/`e` that starts
  its own token, just before the quote, turns on `\` escapes
  (`isEStringPrefix`). A doubled quote is consumed inside the region, so
  an E-string keeps its escape mode across `''`. Reading it as a close and
  a reopen would forget the mode.
- The token test uses the lexer's `isIdentPart` (ASCII letters, digits,
  underscore), so `some'x\'` is still an identifier followed by a standard
  string. The lexer rejects every other unquoted byte, so they can only
  disagree on statements that fail to parse anyway. The one exception is a
  digit: the lexer reads `1e'x'` as `1` then `E'x'`, where the splitter
  reads a standard string. It is a syntax error either way; checked with
  the shell.
- The file header now lists the opaque regions, as `scan.go` does. The
  shell scanner's comment no longer says pgwire doesn't know E-strings.
- Tests: ten new `TestSplitStatements` cases (E and e, `\\`, `''` inside an
  E-string, identifier-ending-in-e, standard strings and quoted identifiers
  where `\` is literal, an unterminated E-string ending in a lone
  backslash). `TestSimpleQueryEString` (`pgwire/server_test.go`) sends a
  create, a two-row insert of E-strings holding `;`, and a select in one
  `PgConn().Exec` buffer. With the old `split.go` swapped back in, it fails
  with `unterminated string literal (SQLSTATE 42601)`.

**Raised N-034** while checking the splitter against the lexer: `lex`
ends a block comment at the first `*/`, but Postgres nests them, and so do
the splitter and the shell scanner. `select 1 /* a /* b */ c */ as x` is a
syntax error in bytdb.

Commit `3740d2c`.

## 2. N-031: `go install …@version` and the pgwire `replace`

The item offered two fixes: drop `replace github.com/rohanthewiz/bytdb =>
../` from `pgwire/go.mod`, or move the commands into a module without one.
The item's text gave the reasons for dropping it, and it is easy to reverse
and keeps the install paths, so I took it as a minor decision.

- `pgwire/go.mod` lost the replace. `GOWORK=off go mod tidy` added bytdb
  v0.22.0's two lines to `go.sum`. Workspace builds still use the working
  tree through `go.work`.
- `GOWORK=off go build`, `go vet` and `go test` in `pgwire/` pass against
  the released bytdb v0.22.0. Nothing in pgwire needs unreleased root API
  yet (the root changes since v0.22.0 are memguard internals and docs).
  bench builds both ways. It keeps its own replaces, since nothing installs
  it.
- **Install check without tagging.** I served the pgwire tree as fake
  versions from a local file GOPROXY: v0.22.98 with the old go.mod and
  v0.22.99 with the new one. The script lives in the scratchpad, at
  `n031/mkproxy.py`: a zip of `git ls-files` with the module prefix, plus
  `.mod`, `.info` and `list`. Then I ran `go install` outside any module
  with `GOPRIVATE=none.invalid GONOPROXY=none.invalid
  GONOSUMDB=github.com/rohanthewiz/bytdb/pgwire`. This machine's `go env`
  has `GOPRIVATE=github.com/rohanthewiz/*`, which skips every proxy, and
  an *empty* `GOPRIVATE=` falls back to the go env file, so it had to
  match nothing. Results: v0.22.98 fails with "The go.mod file for the
  module providing named packages contains one or more replace
  directives". v0.22.99 installs `bytdb` and `bytdbd`, and both run. `go
  run …/cmd/bytdb@v0.22.99 -c "select 1"` works.
- `.version` reports `(devel)` in a workspace build and `v0.22.0` in
  `GOWORK=off` and installed builds. The `(local build)` branch in
  `versionString` stays for a consumer's own replace, and its comment says
  so.
- Release skill: the rationale paragraph now explains why pgwire has no
  replace and bench keeps one. Step 4 is now `GOWORK=off go get
  github.com/rohanthewiz/bytdb@vX.Y.Z && GOWORK=off go mod tidy`, because
  `go.sum` changes too and `go get` raises shared deps. With GOPRIVATE set,
  the fetch goes to GitHub, which has the tag since step 3. A new gotcha
  says never to re-add the replace.
- Docs: README (bytdbd `go run …@latest` with a note, shell `go install
  …@latest`), `docs/cli.md` Install, `docs/index.md`, `docs/features.md`,
  and the repo's bytdb skill. The note says `@version` needs a pgwire
  release newer than v0.22.0, and that from a checkout you can drop
  `@latest`.

N-031 went to **Validate**, not Closed. No tag was cut: the item didn't ask
for a release. Until the next pgwire tag, `@latest` still resolves to
v0.22.0, which has the replace.

Commit `c5785ae`.

## Next

Closed: N-030. Declined: None. Raised: N-034. Deferred: None.
Promoted: None. Moved: N-031 → Validate. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
