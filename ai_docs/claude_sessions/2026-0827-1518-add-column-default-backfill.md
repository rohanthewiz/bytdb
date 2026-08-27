# Session: ADD COLUMN ... DEFAULT backfill, SET/DROP DEFAULT, SET/DROP NOT NULL

- Session ID: `28e17be5-a6c6-43fa-9792-20a3bc7f163c`
- Date: 2026-08-27
- Branch: `main` (started clean at `5b34bc2`, the v0.9.1 pgwire pin bump)
- Released as: **v0.10.0** (`ff1d722`) — backfill + SET/DROP DEFAULT
- Then **v0.11.0** (`4d97a49`) — SET/DROP NOT NULL, after a scaling question

One session, two releases: the second came out of asking what the new
backfill would cost on a 100M-row table. The measurements are recorded
below because they are the reason SET NOT NULL exists.

## Trigger

The user's GoNotes app, embedding bytdb v0.6.4, failed at startup:

```
adding a column with DEFAULT to a non-empty table is not supported
  column="version"  table="notes"
  gonotes/models.(*dbEngine).ensureColumn -> bytdb.(*Engine).AddColumn
```

Question asked: "Is this a limitation we have to live with?"

Answer: no — it was a self-imposed guard (still present unchanged on
main at `ddl.go:200-211`), not a structural one.

## Why the guard existed

`AddColumn` never rewrote stored rows. Row values are a sparse sequence
of `(columnID, value)` pairs (`encodeRowValue`, `dml.go:756`), and any
column absent from a row's value decodes as NULL (`decodeRow`,
`dml.go:768`). So on a non-empty table a DEFAULT would apply to *new*
inserts while existing rows read NULL — a silent divergence from
Postgres, which backfills. The engine refused rather than diverge.

Two fixes were on the table; the user chose the first:

1. **Eager backfill** — rewrite each row inside the DDL transaction.
   O(rows), but bytdb is in-memory and this is a one-time schema step.
2. **Postgres-style "fast default"** (`attmissingval`) — store a
   MissingVal on the column and have `decodeRow` substitute it for
   absent columns. O(1), but blocked by a real obstacle: `encodeRowValue`
   omits NULLs, so "absent" and "explicitly NULL" are identical on disk.
   A row later `SET c = NULL` would read back as the default unless
   columns carrying a MissingVal started writing an explicit NULL tag.

## What was implemented

### 1. `AddColumn` backfill (`ddl.go`)

Inside the same `alterDesc` closure that publishes the descriptor:

- Evaluate the column's DEFAULT once (`columnDefaultValue`).
- NOT NULL check now keyed on the *evaluated* value: it fails only when
  no non-NULL default supplies a value. So `NOT NULL DEFAULT x` is now
  legal on a non-empty table; `NOT NULL` alone still requires an empty
  one; `DEFAULT NULL` skips the rewrite (that's what rows already read).
- `backfillColumn(tx, desc, colID, val)` appends one `(colID, value)`
  pair to each stored row value.

Design points worth keeping in mind:

- **Append, not decode/re-encode.** The column ID is freshly allocated,
  so no row can already carry a pair for it. Appending also happens to
  preserve column order, which is how `encodeRowValue` writes pairs
  (`decodeRow` does not depend on that).
- **No index maintenance.** The column is brand new, so no index can
  cover it, and no other column's value moves.
- **Keys collected before the first write**, so the iteration never
  walks a tree it is mutating.
- **All-or-nothing.** Rows and descriptor commit together; a failed
  evaluation leaves neither changed (tested).
- Cost moves from O(1) to O(rows) *only* when a default is present.

### 2. `default.go` (new) — engine-side literal evaluation

The engine cannot call the SQL layer (`sql` imports `bytdb`, not the
reverse), but the backfill needs the DEFAULT's *value*, not its text.
So `default.go` re-reads the narrow vocabulary `sql/parser.go`'s
`renderLit` can emit:

- `null`, `true`/`false`, ints, floats, `'quoted strings'` with the
  doubled-quote escape (`unquoteSQLString` rejects a quote that closes
  the literal early — that means an expression, not a constant)
- the evaluated markers `now()` and `current_date` (stored unquoted
  precisely so they cannot collide with a constant string default)

then runs the result through the engine's own `coerce` +
`enforceMaxLen`, i.e. exactly the path an insert value takes.

**Anything outside that vocabulary is an error, not a guess** — an
expression, a cast, `gen_random_uuid()` fails the ALTER rather than
leaving old rows NULL while new inserts get a value.

`now()` resolves once per statement, so every backfilled row shares one
instant (the same granularity a multi-row INSERT gets). `current_date`
truncates to midnight UTC before coercion, which lands correctly on
both date and timestamp columns.

This file is deliberately coupled to `sql/parser.go`'s `renderLit` /
`parseStoredLiteral`: **a new literal form there needs an arm here.**
Noted in the file header.

### 3. `SET` / `DROP DEFAULT`

Engine (`ddl.go`):

- `Engine.SetColumnDefault(table, column, literal string) error`
- `Engine.DropColumnDefault(table, column string) error`

Descriptor-only, per Postgres — they steer later inserts and never
touch stored rows (backfilling is `AddColumn`'s job, or an explicit
UPDATE). The literal is validated against the column type at DDL time
rather than at the first insert, so a typo fails the statement.
Identity columns are refused (the counter is already the value source).
`SetColumnDefault("")` delegates to the drop; `DropColumnDefault` is
idempotent and returns `nil, nil` from the closure when there is
nothing to publish (`alterDesc` then skips the descriptor write).

SQL layer:

- `sql/ast.go` — `AlterColumnDefault{Table, Col, Default, Drop}`.
- `sql/parser.go` `alterTable()` — new `ALTER [COLUMN] c SET DEFAULT
  expr | DROP DEFAULT` arm, reusing `defaultLiteral` so the accepted
  syntax is identical to CREATE TABLE's. `SET DEFAULT NULL` normalizes
  to `Drop` (same rule `colDef` already applied). Other Postgres column
  alterations (`SET NOT NULL`, `TYPE`, ...) are rejected **by name**
  with a message saying what is supported, not as a bare parse error.
- `sql/sql.go` — executor arm; DROP needs nothing from the descriptor,
  SET looks the column type up to render and type-check the literal.
- `renderDefault(v, typ, colName)` extracted from `toEngineColumn` and
  shared by both paths, so CREATE TABLE / ADD COLUMN / ALTER COLUMN
  accept exactly the same set of defaults.
- Registered in the three statement switches that enumerate DDL:
  `sql/session.go` `isDDL`, `sql/syscat.go` `writeTarget`,
  `sql/describe.go` (statement tag → `ALTER TABLE`).

## Tests

- `default_backfill_test.go` (new, root package): backfill correctness
  across every literal type (int/float/bool/string-with-quote/text[]/
  jsonb/NULL/now()/current_date); an index built *after* the backfill
  is the observable proof rows were rewritten rather than read through
  a descriptor fallback; all-or-nothing on a rejected default; NOT NULL
  interactions; SET/DROP/replace/reopen.
- `sql/default_test.go`: `TestAlterColumnDefault` added; the old
  "ADD COLUMN DEFAULT needs an empty table" assertion in
  `TestDefaultInteractions` became a backfill assertion.
- Full suite green: root, `sql`, `pgwire`, `stdlib`, `replicate`,
  `replicate/s3`, `tuple`.

## Wire verification (`/verify` skill)

Server: `pgwire/cmd/bytdbd` on 127.0.0.1:5439 over a scratch db, driven
by a pgx v5.10.0 client. Reproduced the reported GoNotes case:

```
alter table notes add column version int not null default 1
  -> [1 one 1] [2 two 1] [3 three 1]
alter table notes alter column version set default 7
  -> stored rows keep 1; the next insert gets 7
```

Also confirmed: one distinct `now()` across all backfilled rows; an
index created after the backfill sees the values; extended-protocol
`$1` over the backfilled column; DROP DEFAULT idempotent; the four
rejection paths error with Postgres-shaped messages (23502 for the
NOT NULL one). Then killed and restarted the server — the backfilled
values and the new default came back intact from the WAL.

## Docs

- `README.md` — statement list gained the ALTER COLUMN line.
- `docs/features.md` — ADD COLUMN's O(1) claim now qualified (O(1)
  without a DEFAULT, O(rows) with one); the "needs an empty table (no
  backfill)" sentence replaced; new bullet for SET/DROP DEFAULT.
- `docs/gotchas.md` — schema-change edges rewritten for the NOT NULL
  rule; new bullet warning that SET DEFAULT does *not* backfill; new
  "deliberately not there" row for the other ALTER COLUMN forms.

## Follow-ups / notes

- **GoNotes needs a version bump to v0.10.0** (or a `replace`) before
  `ensureColumn` stops erroring — it was pinned at v0.6.4.
- Not implemented, still absent: `ALTER COLUMN SET/DROP NOT NULL`,
  `ALTER COLUMN TYPE`, `ADD COLUMN` with an identity column (existing
  rows would need drawn values — the backfill machinery is now there,
  but the counter semantics were not in scope).
- If the O(rows) backfill ever becomes a problem for a large table, the
  fast-default route from the analysis above is the alternative — it
  needs `encodeRowValue` to write an explicit NULL tag for columns
  carrying a MissingVal.


---

# Part 2 — the 100M-row question, and SET NOT NULL (v0.11.0)

## The question

> "What if my database already contains 100 million rows? What would
> be the estimated cost of this migration?"

Measured rather than estimated: a scratch harness loaded 100k / 400k /
1.6M rows (~60-byte rows: `id int pk, body text`) and timed
`AddColumn` with a DEFAULT while sampling live heap (`HeapAlloc`) on a
2 ms ticker. All costs are linear in row count, so per-row constants
extrapolate. Run on an M1.

## What the backfill actually costs

| | per row | x 100M |
|---|---|---|
| Wall time | 0.6-0.8 us | ~60-80 s |
| **Transient peak heap** | **+710-950 B** | **+71-95 GB** |
| WAL / disk growth | ~130-150 B | ~13-15 GB, one commit |
| Settled heap growth (int col) | ~60 B | ~6 GB |
| (table itself, live) | ~370 B | ~37 GB |

**Time is not the problem; memory is.** The +710-950 B/row is
irreducible: COW node copies and new row buffers stay *live* until the
single transaction commits. Confirmed not to be GC slack by re-running
at `GOGC=25` — the peak barely moved (950 -> 710 B/row). At 100M rows
that is a ~110-130 GB peak: it OOMs. The all-or-nothing guarantee is
exactly what makes it unaffordable, since there is no partial state to
release.

Contrast, same table, chunked `UPDATE` in 5k-row transactions:

| | one-shot | chunked |
|---|---|---|
| Transient peak (GOGC=25) | +710 B/row | **+93 B/row** |
| Wall time | 0.7 us/row | 2.6 us/row |

The chunked peak is dominated by GC pacing (heap targets ~2x live),
not by the migration — which is why lowering GOGC moves it and does
not move the one-shot number. `EXPLAIN` confirmed each chunk is an
`Index Scan using t_pkey` with `Index Cond`, so the loop is O(n)
overall, not O(n^2), and it is restartable after an interruption.

## The gap that surfaced

The batched path had no way to end at a **required** column: bytdb had
no `ALTER COLUMN SET NOT NULL`, so the only route to NOT NULL was the
one-shot `ADD COLUMN ... NOT NULL DEFAULT x` — precisely the statement
you cannot afford at 100M rows. Hence v0.11.0.

## What was implemented

### `Engine.SetColumnNotNull` / `DropColumnNotNull` (`ddl.go`)

`SetColumnNotNull` validates every existing row inside the transaction
that publishes the flag, so no write can slip between the check and
the publish. A single NULL aborts with the wording AddColumn already
used for the equivalent case (extracted to `notNullValueErr`):
`column "c" of relation "t" contains null values`, SQLSTATE 23502.

`columnHasNoNulls` reads the stored value tuples **directly** rather
than going through `decodeRow`. The trick is the same storage fact the
backfill exploits from the other side: `encodeRowValue` omits NULL
columns, so the *presence* of a pair tagged with the column ID is
exactly what "not NULL" means. One tuple decode per row, no row
materialized — which is the whole point on a table too large to
rewrite. Key columns skip the scan entirely (`coercePK` rejects NULL,
so they are non-NULL by construction).

`DropColumnNotNull` is a descriptor flip, refused on a primary-key
column (the key encoding has no NULL to encode) and on an identity
column (its counter is what guarantees a value). Both operations are
idempotent (`nil, nil` from the closure, so `alterDesc` skips the
write).

Scope note: only `SET NOT NULL` was asked for; `DROP NOT NULL` came
along because Postgres pairs them, it is a one-line flip, and the
parser's error message would otherwise have been odd.

### SQL layer

- `sql/ast.go` — `AlterColumnNotNull{Table, Col, NotNull}`.
- `sql/parser.go` — the ALTER COLUMN arm's SET/DROP branches became
  inner switches; each rejects unknown sub-clauses by name
  ("only SET DEFAULT and SET NOT NULL are supported by ALTER COLUMN").
- `sql/sql.go` — executor arm; registered in the three DDL switches
  (`isDDL`, `writeTarget`, statement tag) as `AlterColumnDefault` was.

### Measured cost of the new path

| | per row | x 100M |
|---|---|---|
| `SET NOT NULL` scan | 0.17-0.20 us, +90 B transient | ~20 s, ~9 GB |
| `ADD COLUMN ... NOT NULL DEFAULT` | 0.7 us, +710 B **live** | ~70 s, +71 GB |

The 90 B/row here is GC pacing over per-row decode garbage, not
retained state, so `GOMEMLIMIT` bounds it — unlike the backfill, whose
memory is live to the commit and cannot be capped.

## The migration this completes

None of these steps rewrites the table in one transaction:

```sql
ALTER TABLE t ADD COLUMN c int;               -- O(1), rows read NULL
ALTER TABLE t ALTER COLUMN c SET DEFAULT 1;   -- O(1), future inserts covered
UPDATE t SET c = 1 WHERE c IS NULL AND id >= $lo AND id < $hi;  -- batched
ALTER TABLE t ALTER COLUMN c SET NOT NULL;    -- read-only validating scan
```

## Tests and verification

- `default_backfill_test.go` — `TestSetDropColumnNotNull` (refusal over
  a NULL with nothing published, the flag rejecting later INSERT and
  UPDATE-to-NULL, idempotence, reopen, key-column no-op, primary-key
  and unknown-column rejections) and `TestSetNotNullIdentity`.
- `sql/default_test.go` — `TestAlterColumnNotNull`, ending with the
  full four-statement migration above. One stale assertion in
  `TestAlterColumnDefault` that expected `SET NOT NULL` to be
  unsupported now tests `SET UNIQUE` instead.
- Wire (pgx over `bytdbd`): the migration end to end, the flag live for
  new writes (23502 on both INSERT and UPDATE-to-NULL), a column still
  holding NULLs refusing the constraint, and the rejection paths.

## Docs

`README.md` statement list; `docs/features.md` gained the SET/DROP NOT
NULL bullet with the migration recipe; `docs/gotchas.md` narrowed the
"deliberately not there" ALTER COLUMN row and gained the measured
reason to prefer the batched path on a large table.

## Still absent after v0.11.0

- `ALTER COLUMN TYPE`, `SET STORAGE`, and friends.
- `ADD COLUMN` of an identity column (the backfill machinery now
  exists, but the counter semantics were not in scope).
- Nothing in the engine warns when a DEFAULT backfill is about to be
  enormous. A row-count threshold that errors with a pointer to the
  batched recipe would be a reasonable follow-up — it was raised as an
  option, not implemented.
