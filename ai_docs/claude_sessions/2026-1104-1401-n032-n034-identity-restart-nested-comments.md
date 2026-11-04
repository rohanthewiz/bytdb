# N-032 and N-034: identity RESTART for .dump, nested block comments

- **Session:** `91a4689b-1716-48c9-add4-1494c0e1f385`
- **Date:** 2026-11-04
- **Scope:** `/work-next-list`, second and final batch, after
  `2026-1104-1351-n030-n031-estring-split-pgwire-replace` (N-030 closed,
  N-031 to Validate, N-034 raised). This batch closes N-032 and N-034, which
  leaves Open empty. N-033 was skipped all run because it needs hands-on
  Linux and Windows terminal runs.

## 1. N-032: ALTER COLUMN ... RESTART, and `.dump` carries identity counters

A restored `serial` column resumed just past the largest restored id, so a
value that was drawn and then deleted before the dump could be handed out
again. bytdb had no SQL to set an identity counter.

**SQL surface.** `ALTER TABLE t ALTER [COLUMN] c RESTART [[WITH] n]`, the
Postgres syntax for identity columns. A bare `RESTART` means the start value,
1. It is parsed in the ALTER COLUMN switch (`sql/parser.go`) the way
`seqOptions` parses ALTER SEQUENCE's RESTART, into `AlterColumnRestart`
(`sql/ast.go`).

**Design choice: a write, not DDL.** It moves counter state and leaves the
schema alone, so it runs through `d.write` in the statement's transaction,
exactly like `TRUNCATE … RESTART IDENTITY`:

- `isDDL` is false, so a BEGIN block runs it instead of refusing it.
- `isWrite` is true, so a read-only block refuses it.
- ROLLBACK undoes it.
- It is in `writeTarget`, so the system-catalog guard applies, and its
  command tag is `ALTER TABLE`.

**Engine.** `Txn.RestartIdentity(table, column, next int64)` and
`Txn.PeekIdentity(table, column) (uint64, bool, error)` live in `identity.go`,
with Engine wrappers.

- Restart writes the 8-byte counter in the transaction. Under
  concurrent writes it also appends the key to `dirtySeqPrefixes`, so the
  commit discards the cached allocator. That is the same mechanism and the
  same documented race as Truncate.
- Peek under concurrent writes reports the live allocator, like `PeekSeq`.
  The stored record is a watermark there. A dump pairing it with a row
  snapshot gets a counter at or past every stored id, which is the safe
  direction.
- Range: `next >= 1`. int64 caps the top.
- Errors use Postgres's wording: `column "c" of relation "t" is not an
  identity column` and `RESTART value (n) cannot be less than MINVALUE (1)`.

**SQLSTATEs** (`pgwire/errors.go`): `is not an identity column` → 55000.
The `RESTART value (` prefix → 22023, which also fixes ALTER SEQUENCE's
RESTART range errors; those were XX000.

**Dump.** `dumpIdentityCounters` (`pgwire/cmd/bytdb/schema.go`) runs after
each table's rows. It writes one `ALTER TABLE t ALTER COLUMN c RESTART WITH
n;` per identity column whose counter exists. A counter that was never used
gets no line, and an exhausted one (above MaxInt64) gets a comment. The line
has to come after the rows: replayed explicit ids bump the counter past
themselves and would overwrite a RESTART placed earlier. A replayed dump
still dumps back byte-identical.

**Tests.**
- `identity_restart_test.go` `TestRestartIdentity` runs in both write
  modes. It covers peek before any draw, allocator invalidation on commit,
  a rolled-back restart (with an in-transaction peek: serialized mode sees
  its own write, concurrent mode the allocator), a restart below stored ids
  colliding, the refusals, and reopen.
- `sql.TestAlterColumnRestart` covers the spellings, bare RESTART on an
  emptied table, BEGIN/ROLLBACK, read-only refusal, the error cases, the
  tag, and isDDL/isWrite.
- pgwire `TestAlterColumnRestartWire` checks the tag and the two SQLSTATEs
  over pgx, and `TestSQLStateMapping` gained three rows.
- Shell `TestSchemaAndDumpRoundTrip` now inserts and deletes id 4 before
  dumping. The restored database draws 5. With the new line removed it
  re-issues 4, and the test fails on all three assertions (checked).
- Ran `go test -race` on the identity and truncate tests.

**Docs.** README grammar, a `docs/features.md` Auto-increment bullet, the
`docs/cli.md` `.dump` paragraph (it used to describe the limitation), the
grammar comment in `sql/sql.go`, and the dump-order header in `schema.go`.

**Release consequence.** The shell now calls the new `Txn.PeekIdentity`. So
pgwire's `GOWORK=off go build` fails until root is tagged and pgwire's pin
bumped. That is the N-031 trade-off, and the release skill describes it. The
workspace build and every suite are green. The next release should be a
minor one, since it adds SQL and API.

Commit `14ff0b1`.

## 2. N-034: the lexer nests block comments

`lex` ended a `/* */` comment at the first `*/`. Postgres nests them, and so
did the splitter and the shell scanner. `blockCommentLen` (`sql/lexer.go`)
counts depth, and `/*/` is unterminated, as in Postgres.

One compatibility edge: a view stored with an unbalanced inner `/*` in its
verbatim text would stop parsing. Postgres rejects such text too, and broken
views are already handled.

Tests: `TestLexNestedBlockComments` (five cases fail on the old lexer), and
pgwire `TestSimpleQueryNestedComment`, which gets a syntax error on the old
lexer. The README's dialect paragraph says block comments nest.

Commit `6174453`.

## 3. State at the end of the run

- Open is empty; the list now says *Nothing open.*
- Validate: N-031 (check `go install …@latest` once a pgwire tag newer
  than v0.22.0 exists) and N-033 (Linux and Windows terminal runs of the
  shell).
- Unreleased on `main` since v0.22.0: the E-string splitter, the dropped
  pgwire replace, ALTER COLUMN RESTART with the new Engine API and the
  SQLSTATEs, nested comments, and the shell. No tag was cut, because none
  of the items asked for one.

## Next

Closed: N-032, N-034. Declined: None. Raised: None. Deferred: None.
Promoted: None. Moved: None. Updated: None.
Full list: `ai_docs/todo/next-list.md`.
