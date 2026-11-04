# The `bytdb` shell

`bytdb` is an interactive SQL shell for bytdb database files, modeled on
the DuckDB and sqlite3 command-line shells. It opens the file directly,
embedded in its own process. There is no server involved. It accepts
DuckDB's dot commands, and it also accepts the psql meta-commands that
Postgres users already type from habit.

```text
$ bytdb app.bytdb
bytdb v0.22.0 go1.26.1 darwin/arm64
Enter ".help" for usage hints.
bytdb> SELECT name, city
  ...>   FROM users WHERE age > 40;
┌───────┬──────┐
│ name  │ city │
│ text  │ text │
├───────┼──────┤
│ grace │ nyc  │
└───────┴──────┘
bytdb> BEGIN;
BEGIN
bytdb*> UPDATE users SET city = 'sf' WHERE name = 'grace';
UPDATE 1
bytdb*> COMMIT;
COMMIT
```

## Install

The shell lives in the pgwire module, next to `bytdbd`:

```sh
go install github.com/rohanthewiz/bytdb/pgwire/cmd/bytdb@latest   # → $(go env GOPATH)/bin/bytdb
```

`@latest` and `@vX.Y.Z` installs need a pgwire release newer than
v0.22.0. Up to v0.22.0, pgwire's `go.mod` carried a `replace` directive,
and the Go toolchain refuses `@version` installs of a module that has one.
`bytdbd` installs the same way. To build from a checkout instead, at any
version:

```sh
git clone https://github.com/rohanthewiz/bytdb && cd bytdb/pgwire
go install ./cmd/bytdb
```

## Running it

```text
bytdb [OPTIONS] [FILENAME [COMMAND ...]]
```

| invocation | what happens |
|---|---|
| `bytdb` | opens a transient in-memory database, which is deleted on exit |
| `bytdb app.bytdb` | opens `app.bytdb`, creating it if needed |
| `bytdb app.bytdb "SELECT 1" ".tables"` | runs the commands in order, then exits |
| `bytdb -c "SELECT 1" app.bytdb` | same as above; `-c` can be repeated |
| `bytdb app.bytdb < script.sql` | batch mode: no prompt, no line editing |
| `bytdb -csv app.bytdb -c "SELECT * FROM t" > t.csv` | exports a query result |

Options may come before or after the file name. `:memory:` names a
transient database, as it does in DuckDB and sqlite.

A file that `bytdbd` is serving is locked by the server. The shell
refuses to open it and tells you to connect with `psql` instead.

| option | meaning |
|---|---|
| `-c CMD`, `-s CMD` | run CMD (SQL or a dot command) and exit; repeatable |
| `-cmd CMD` | run CMD before reading input; repeatable |
| `-init FILE` | run FILE at startup (default: `~/.bytdbrc`, interactive sessions only) |
| `-bail` | stop at the first error |
| `-echo` | print each statement before running it |
| `-batch` / `-interactive` | force a mode (the default follows whether stdin and stdout are terminals) |
| `-header` / `-noheader` | show or hide column names |
| `-nullvalue TEXT` | text to show for NULL |
| `-separator SEP` | column separator for list and csv modes |
| `-duckbox -box -table -markdown -csv -tabs -list -line -json -jsonlines -html -insert -trash` | output mode |
| `-sync always\|never` | WAL fsync policy (default `always`) |
| `-encryption-key-file FILE`, `-encryption-key-env VAR` | key for an encrypted database; same formats as `bytdbd` |
| `-version` | print the version and exit |

In batch and `-c` runs, the exit status is `1` if any statement failed,
and `130` if Ctrl-C interrupted a statement. `-c` commands stop at the
first failure, because later commands usually depend on earlier ones.

## At the prompt

- A SQL statement ends with `;` and may span several lines. The
  continuation prompt is `  ...>`. The scanner knows about strings, `E''`
  escape strings, quoted identifiers, and comments, so a `;` inside any
  of them does not end the statement.
- The prompt shows the transaction state the way psql does: `bytdb*>`
  inside a `BEGIN` block, and `bytdb!>` in a failed block that needs
  `ROLLBACK`.
- **Ctrl-C** cancels the running statement. At the prompt, it discards
  the line you are typing. **Ctrl-D** exits. If a transaction block is
  open when you exit, it is rolled back and the shell warns you.
- **Tab** completes dot commands, SQL keywords, table, view, and column
  names (`orders.to<Tab>` completes `orders.total`), and file paths after
  commands that take one.
- History is saved to `~/.bytdb_history`. Set `BYTDB_HISTORY` to use
  another path, or set it to an empty string to turn history off. A
  statement typed over several lines is saved as one history entry.
- Command tags such as `INSERT 0 3` and `CREATE TABLE` are shown in
  interactive sessions (`.changes on`) and hidden in scripts.
- Parse errors point at the problem:

  ```text
  bytdb> SELECT * FROM users WHERE;
  Error: syntax error (want: an expression, got: end of input, pos: 25)
  LINE 1: SELECT * FROM users WHERE
                                   ^
  ```

The shell also answers `DESCRIBE table` and `SHOW TABLES` itself, since
bytdb's SQL has neither statement and DuckDB users type both.

## Output modes

| mode | output |
|---|---|
| `duckbox` (default) | boxed table with a row of column types under the header, fitted to the terminal |
| `box` / `table` | boxed table drawn with Unicode / ASCII lines |
| `markdown` | GitHub-flavored Markdown table |
| `csv` / `tabs` | RFC 4180 CSV / tab-separated values |
| `list` | values separated by `.separator` (default `\|`), no quoting |
| `line` | `column = value`, one value per line (psql's `\x`) |
| `json` / `jsonlines` | a JSON array of objects / one object per line. jsonb values are embedded as JSON documents, and text[] values become JSON arrays |
| `html` | an HTML `<table>` |
| `insert [TABLE]` | one `INSERT` statement per row |
| `trash` | no output; useful together with `.timer on` |

duckbox fits itself to the terminal. If a result has more than 40 rows,
it shows the first and last 20 and marks the elided middle with `·`. If
the table is too wide, it truncates cells with `…`, and then collapses
middle columns into a single `…` column. A footer reports the totals,
for example `1000 rows (40 shown)   9 columns (4 shown)`. Fitting happens
only when output goes to a terminal, so piped or redirected output is
always complete. `.maxrows` and `.maxwidth` override the defaults.

## Dot commands

Commands can be shortened to any unique prefix (`.hea off`).

| command | what it does |
|---|---|
| `.tables [PATTERN]` | list tables and views (PATTERN uses LIKE syntax) |
| `.schema [PATTERN]` | print CREATE statements for tables, indexes, sequences, and views |
| `.describe TABLE` | show a table's columns: type, nullability, key, default |
| `.indexes [TABLE]` | list primary keys and secondary indexes |
| `.dump [TABLE ...]` | write the database (or just the named tables) as a SQL script |
| `.import FILE TABLE [opts]` | load CSV or TSV rows into TABLE (see below) |
| `.mode [MODE [TABLE]]` | set the output mode; with no argument, list the modes |
| `.headers on\|off` | show or hide column names |
| `.nullvalue TEXT` | text to show for NULL |
| `.separator COL [ROW]` | list and csv separators; `\t` and `\n` escapes work |
| `.maxrows N\|auto`, `.maxwidth N\|auto` | duckbox fitting limits |
| `.output [FILE]`, `.once FILE` | send results to FILE, either until changed or for the next command only |
| `.read FILE` | run the SQL and commands in FILE |
| `.open [FILE]` | switch to another database; with no FILE, open a transient one |
| `.backup FILE` | write a consistent copy of the database while it stays online |
| `.stats` | show engine statistics: log size, growth since the last compaction, heap |
| `.databases` | show the open file |
| `.timer`, `.echo`, `.bail`, `.changes` `[on\|off]` | toggle a setting; with no argument, flip it |
| `.edit` | edit the last statement in `$VISUAL` or `$EDITOR`, then run it |
| `.shell CMD`, `.system CMD` | run CMD in the system shell |
| `.cd DIR`, `.print TEXT`, `.prompt MAIN [CONT]` | the usual |
| `.show`, `.help [PATTERN]`, `.version` | show settings, help, or the version |
| `.quit`, `.exit [CODE]` | leave the shell |

psql meta-commands map onto these. `\q`, `\d`, `\d TABLE`, `\dt`, `\di`,
`\i FILE`, `\o FILE`, `\! CMD`, `\e`, `\l`, and `\echo` each do what the
same command does in psql. `\x` switches between `line` mode and the
previous mode, and `\timing` toggles `.timer`.

## `.dump`: a portable SQL snapshot

`.dump` reads every table inside one read transaction, so the script is
a consistent snapshot even while other writers are running. Replaying it
into an empty database rebuilds an identical one, and dumping that copy
again produces the same script byte for byte. The test suite checks this.
The script is ordered so that every constraint is checked exactly once
during replay:

1. sequences, with `setval` to restore each one's position
2. `CREATE TABLE`, including the primary key, NOT NULL, DEFAULT, and CHECK
3. the rows, as `INSERT`s wrapped in `BEGIN`/`COMMIT` every 10,000 rows
4. indexes, including the unique indexes behind UNIQUE constraints
5. foreign keys, as `ALTER TABLE … ADD CONSTRAINT`. They come after the
   data, so self-references and mutual references load cleanly.
6. views, ordered so that each view comes after the views it reads

Identity (`serial`) counters resume after the highest restored value.
The one exception is a value that was drawn and then deleted before the
dump: it can be handed out again after a restore.

## `.import`: CSV and TSV

```text
.import FILE TABLE [--csv|--tsv] [--header|--no-header] [--skip N] [--pk COL[,COL]]
```

- **All or nothing.** The whole file loads in one transaction. A bad row
  rolls back the import and reports the file line it came from. A table
  that the import created is dropped again.
- **New tables.** The header row supplies the column names, folded to
  lower case the way Postgres folds unquoted identifiers. Column types
  are inferred from the first 10,000 rows: bigint, double precision,
  boolean, date, timestamp, or text. Integers with a leading zero, such
  as ZIP codes, stay text. bytdb requires a primary key on every table,
  so the import chooses one: the `--pk` columns if given, otherwise a
  column named `id` whose sampled values are all present and distinct,
  otherwise a `rowid serial` column that it adds.
- **Existing tables.** The first row counts as a header only when every
  field in it names a column of the table. This means re-importing the
  same file works without extra options. With a header, fields are
  matched to columns by name.
- **Values.** An empty field is NULL. Every value is coerced to its
  column's type, so uuid, bytea (`\x…`), text[] (`{a,b}`), and jsonb all
  import.
- The delimiter is chosen from the file extension (`.tsv` or `.tab`
  means tab-separated) unless `--csv` or `--tsv` says otherwise. A UTF-8
  byte-order mark at the start of the file is ignored.
