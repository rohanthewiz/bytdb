# bytdb CLI shell, in the style of the DuckDB CLI

- **Session:** `002a5bca-5a5c-488f-9f84-25a315bfc783`
- **Date:** 2026-11-04
- **Scope:** From the user's backlog: "create a utility for operating
  bytdb similar to the DuckDB CLI". Built `bytdb`, an interactive SQL
  shell, in `pgwire/cmd/bytdb/`, with tests and docs.

## 1. Where it lives, and why

`pgwire/cmd/bytdb/`, next to `bytdbd`, in the pgwire module.

- **Not the root module.** A line editor is a third-party dependency,
  and the root library has none beyond btypedb and serr. Keeping it out
  keeps `go get github.com/rohanthewiz/bytdb` lean.
- **Not a new module.** A `cmd/bytdb` module of its own would make a
  third lockstep tag line for `/release`. pgwire is already released in
  lockstep and already hosts the other binary.
- **New pgwire dependencies:** `github.com/peterh/liner` v1.2.3 (pure Go,
  linenoise lineage, as DuckDB's and sqlite's editors are),
  `golang.org/x/term` v0.47.0 (terminal size and tty detection) and
  `github.com/mattn/go-runewidth` v0.0.31 (cell widths for CJK and
  emoji). `bench/go.mod` needed `go mod tidy`, which raised `x/sys` from
  v0.46.0 to v0.49.0. Nothing else changed there.

## 2. What it does

Files: `main.go` (flags, modes, exit status), `shell.go` (input loop,
execution, cancellation), `scan.go` (statement scanner), `render.go`
(output modes), `format.go` (value text, SQL literals, JSON),
`schema.go` (DDL regeneration, `.dump`, DESCRIBE), `dot.go` (commands
and psql aliases), `importcsv.go`, `complete.go`, `terminal.go`.

- **Invocation** follows DuckDB: `bytdb [OPTIONS] [FILENAME [COMMAND
  ...]]`. Options can come after the file name. The flag package is
  re-run after each positional argument it stops at. With no file, or
  `:memory:`, the shell opens a transient database: an ordinary one in
  a private temp directory with fsync off, deleted on close. bytdb has
  no pure in-memory mode. `-c`/`-s` (repeatable, stops at the first
  failure), `-cmd`, `-init`, `-bail`, `-echo`, `-batch`/`-interactive`,
  header and null options, one flag per output mode, `-sync`, and the
  same `-encryption-key-file`/`-env` key handling as `bytdbd`, copied
  from it.
- **Input loop:** a line that starts a statement with `.` or `\` is a
  command. Anything else is SQL, accumulated until a top-level `;`. Every
  source goes through one loop: the line editor, stdin, `.read`, and `-c`.
  At end of input, a script's final statement runs even without a `;`
  (`echo 'select 1' | bytdb`). At the prompt, Ctrl-D discards it, as
  psql does.
- **`scan.go`** knows strings, `E'...'` escape strings (where `\'` and
  `''` both escape), quoted identifiers, `--` comments, and nested block
  comments. pgwire's own splitter doesn't handle E-strings. That is
  raised as N-030. `historyLine` flattens a multi-line statement into a
  single history entry. It rewrites `--` comments as block comments so
  they can't swallow the rest of the joined line.
- **One Session** serves the whole shell, so `BEGIN ... COMMIT` spans
  prompts. The prompt shows the state: `bytdb*>` in a block, `bytdb!>`
  in a failed one. On exit, an open block is rolled back with a warning.
- **Ctrl-C:** each statement runs under a context that a SIGINT goroutine
  cancels. liner keeps ISIG enabled between prompts, so the signal arrives
  even while a query runs. If nothing is running, batch mode exits 130
  and the interactive shell ignores it. Cancelling inside `.read`, `-c`
  or a script stops that source (`Shell.aborted`). Exiting without
  `Close` is safe: commits are already fsynced, and btypedb's lock is an
  `flock`.
- **Terminal handoff:** `.shell` and `.edit` restore the cooked terminal
  mode captured before `NewLiner`, then reapply liner's raw mode
  afterwards.
- **Output modes:** duckbox (the default: a row of types, fitted to the
  terminal), box, table, markdown, csv, tabs, list, line, json,
  jsonlines, html, insert and trash. Values format through (value,
  declared type) pairs, because timestamp and date share int64 and uuid
  shares []byte. Floats print the Postgres way: no exponent within
  [1e-4, 1e15), and `Infinity`/`NaN` spelled out. JSON embeds jsonb as a
  document and text[] as an array.
- **Duckbox fitting** applies only when output goes to a terminal.
  Piped output is never cut. Rows: the first and last halves of
  `maxrows` (40), with three `·` rows between them. Width: a binary
  search for the column cap with a floor of 8, then middle columns
  collapse into a `…` column, keeping columns alternately from the left
  and right ends. The footer reads `N rows (M shown)   K columns (J shown)`.
- **Shell-level SQL:** `DESCRIBE t` and `SHOW TABLES`. Both are syntax
  errors to the engine, so intercepting them shadows nothing.
- **Dot commands:** `.tables .schema .describe .indexes .dump .import
  .mode .headers .nullvalue .separator .maxrows .maxwidth .output .once
  .read .open .backup .stats .databases .timer .echo .bail .changes
  .edit .shell/.system .cd .print .prompt .show .help .version
  .quit/.exit`. Any unique prefix works. psql aliases: `\q \d [t] \dt
  \di \x \timing \i \o \! \e \l \echo`.
- **Command tags** are printed with `.changes`, which is on in interactive
  sessions. The rule is psql's: a tag for every statement that returns
  no rows, plus DML with RETURNING. No tag after SELECT, EXPLAIN or SHOW.
- **Errors** use `bytdb.ErrText`. A parse error's `pos` attribute becomes
  a psql-style `LINE n:` excerpt with a caret under the position. If the
  file is locked, the error says to use psql against `bytdbd`.
- **Exit status:** 1 if a batch or `-c` statement failed, 130 on
  Ctrl-C, or the code given to `.exit N`.

## 3. `.dump` and `.schema`

DDL is regenerated from descriptors (`TableDesc`, `ViewDesc`, `SeqDesc`).
bytdb doesn't keep the original CREATE text.

Order: sequences with `setval`, then `CREATE TABLE` with PK, NOT NULL,
DEFAULT and CHECK, then rows streamed from one `ReadTxn` scan as
`INSERT`s in `BEGIN`/`COMMIT` batches of 10,000, then indexes, then
foreign keys via `ALTER TABLE ... ADD CONSTRAINT`, then views
topologically sorted by whole-word name mentions. Batching avoids
fsync-per-row on replay and stays far below the 10M per-transaction
write cap. Deferring FKs handles self-references and mutual references.
UNIQUE constraints come back as the unique indexes they are stored as.
Identifiers are quoted only when needed: anything other than a plain
lowercase word, or a reserved word.

Verified: dump, then restore into an empty file, then dump again, and the
two dumps are byte-identical. The test schema had quoted mixed-case
names, a CHECK, varchar(n), `DEFAULT now()`, a self-referencing FK, an
FK with ON DELETE CASCADE, a DESC index, every type, a sequence that had
been advanced, and views stacked on views. After restore, the sequence
continued (105), the identity counter continued (4), and cascades fired.
The one gap is N-032: a drawn-then-deleted identity value can be reused
after restore.

## 4. `.import`

The whole file loads in one transaction, or joins the user's open block.
On failure it rolls back, reports the file line, and drops a table it
created.

Two findings from smoke tests shaped it:

- **bytdb requires a primary key on every table.** A new table uses
  `--pk COL[,COL]` if given. Otherwise it uses a column named `id` whose
  sampled values are present and distinct. Otherwise it prepends
  `rowid serial`. When importing into an existing table without a
  header, identity columns are skipped if the record width matches the
  non-identity columns.
- **Re-importing a file into its own table** treated the header row as
  data. Now, for an existing table, the first row is a header when every
  field names a column.

Column types are inferred from 10,000 sampled rows: bigint, double
precision, boolean, date, timestamp, else text. Integers with a leading
zero stay text, because they are codes such as ZIPs. Header names are
folded to lower case, Postgres-style. An empty field is NULL. Values go
in as strings, and the engine coerces them (probed: bigint, float, bool,
bytea `\x..`, timestamp, date, uuid, text[], jsonb all coerce from string
params). The UTF-8 BOM is stripped.

## 5. Verification

- `go test` for `pgwire/cmd/bytdb`. Coverage: the scanner table, the
  `historyLine` round trip, value formatting, duckbox golden and elision
  output, a `fitColumns` property test (5000 random cases: never over
  budget, order kept, no column dropped when the floors fit), alignment
  with wide runes, every other mode, and end-to-end tests through `run()`.
  The end-to-end tests cover batch and `-c`, error exit codes and
  carets, `-bail`, transactions, shorthands and psql aliases, the dump
  round trip, import (including rollback and the dropped table),
  `.output`/`.once` (also covering dot commands), the `.read` nesting
  limit, prompts and transient cleanup, the locked file, completion,
  and `.changes` tags.
- Interactive mode, driven through a pty with a Python driver on macOS
  at 80 columns: continuation prompt, `*` marker, Ctrl-C at the prompt,
  Ctrl-C cancelling a 4-way 100M-row cross join, Tab completion,
  `.shell`, width fitting, and the history file. Linux and Windows only
  cross-compile (`GOOS=linux`/`windows go build`). Running them is
  N-033.
- The full pgwire suite (`go test ./...`, including `bytdbd`) passed.
  `go vet` is clean, and bench builds after the tidy.
- Every build and test here ran with `GOWORK=off`. The local
  `../btypedb` clone (used by `go.work`) is 3 commits behind origin and
  lacks `ErrLocked`, `LogStats` and `AcquireLock`, so workspace builds
  fail on this machine. The clone was left untouched. Pull it to restore
  workspace builds.

## 6. Docs

`docs/cli.md` is the full reference: install, invocation, prompt
behavior, modes, commands, and `.dump`/`.import` semantics. It has a
mkdocs nav entry ("The bytdb Shell"). The README gained an "Or just open
it in the shell" subsection under "Show me the code", plus a link in
Documentation.

## Next

Closed: None. Declined: None. Raised: N-030, N-031, N-032, N-033.
Deferred: None. Promoted: None. Moved: None.
Updated: None. Full list: `ai_docs/todo/next-list.md`.
