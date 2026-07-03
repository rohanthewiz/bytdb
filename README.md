# bytdb

A relational layer over [btypedb](https://github.com/rohanthewiz/btypedb),
built the way CockroachDB layers SQL over Pebble: tables, rows, and
(eventually) secondary indexes are all encoded into **one ordered key
space**, so relational operations become key scans against an ordered
store that already provides atomic batches, O(1) copy-on-write snapshot
reads, range deletes, and power-loss-tested durability.

Embedded and in-process, for datasets that fit in memory — the SQLite
niche, not the CockroachDB niche.

## Status

Milestone 1: the storage foundation.

- **`tuple`** — an order-preserving binary encoding for composite keys:
  for any two tuples, `bytes.Compare` on their encodings equals
  element-wise semantic comparison. NULL, bool, int64, float64, string,
  and []byte elements; property-tested (random tuples, encode → byte
  order ≡ semantic order).
- **table layer** — a persistent catalog (descriptors stored as rows of
  a system table), `CreateTable`/`DropTable`, `Insert` with type
  coercion and PK uniqueness, `Get`/`Delete` by primary key, and
  `Scan`/`ScanRange` in primary-key order with partial-prefix bounds on
  composite keys.
- **secondary indexes** — `CreateIndex` (with atomic backfill over
  existing rows), `DropIndex`, unique indexes, and `ScanIndex` with
  range bounds; every insert and delete maintains all indexes in the
  same atomic commit as the row.
- **row updates and transactions** — `Update` sets columns by name
  (primary-key changes move the row), with every affected index entry
  moved and uniqueness re-checked before anything is written;
  `WriteTxn`/`ReadTxn` run multi-statement work on a serializable
  snapshot of data and catalog, with own-write visibility.
- **schema changes without rewrites** — row values are sparse
  (column ID, value) pairs with NULLs omitted, so `AddColumn` and
  `DropColumn` touch only the descriptor: old rows read added columns
  as NULL, dropped-column data is skipped on decode, and a re-added
  name gets a fresh ID so stale data can never resurface.

No SQL yet — the API is Go calls. See Roadmap.

## Example

```go
e, err := bytdb.Open("app.db")
defer e.Close()

_, err = e.CreateTable("events", []bytdb.Column{
    {Name: "org", Type: bytdb.TString}, {Name: "seq", Type: bytdb.TInt}, {Name: "note", Type: bytdb.TString},
}, "org", "seq") // composite primary key

err = e.Insert("events", "acme", 1, "signup")
err = e.Insert("events", "acme", 10, "upgrade")

row, ok, err := e.Get("events", "acme", 1)

// Ordered range scan: all of acme's events, seq order, via one key range.
for row, err := range e.ScanRange("events", []any{"acme"}, []any{"acmf"}) {
    if err != nil { break }
    fmt.Println(row.Col("seq"), row.Col("note"))
}

// Secondary index: backfilled from existing rows, then maintained by
// every write in the same atomic commit.
_, err = e.CreateIndex("events", "by-note", false, "note")
for row, err := range e.ScanIndex("events", "by-note", []any{"s"}, []any{"t"}) {
    // notes in ["s", "t"), note order
}

// Row update by primary key: named columns, indexes maintained.
updated, err := e.Update("events", []any{"acme", 1}, map[string]any{"note": "signup+trial"})

// Transactions: serializable, atomic, own writes visible inside.
err = e.WriteTxn(func(tx *bytdb.Txn) error {
    if err := tx.Insert("events", "acme", 11, "invite"); err != nil {
        return err
    }
    _, err := tx.Update("events", []any{"acme", 10}, map[string]any{"note": "upgraded"})
    return err // nil commits both; error rolls both back
})
```

## How it maps onto the key space

Everything lives in a single `btypedb.DB[string, []byte]`:

```
tuple(tableID, 1, pk cols...)             → tuple(colID, val, ...)  table rows (primary index)
tuple(tableID, idxID, indexed..., pk...)  → ()                      secondary index entry
tuple(tableID, idxID, indexed...)         → tuple(pk cols...)       unique index entry
tuple(1, 1, tableName)                    → JSON descriptor         catalog
tuple(0)                                  → next table ID           ID sequence
```

Row values tag every non-NULL, non-key column with its stable column
ID rather than relying on position — the CRDB value-encoding idea that
makes `ALTER TABLE` metadata-only.

A unique index enforces uniqueness by key collision — the primary key
moves into the value. Rows with NULL in an indexed column fall back to
the pk-suffixed form even in a unique index, so NULLs never conflict
(SQL semantics); the two entry forms are distinguished by tuple arity
on decode.

The `tuple` encoding is what makes this work: integers are
sign-flipped big-endian so negatives sort first, floats get the
standard order-preserving bit transform, strings escape embedded zero
bytes with an ordered terminator so `"a" < "a\x00" < "ab"`, and a
tuple that is a prefix of another sorts first. Because a composite
key's encoding is ordered per-column, a partial tuple is a valid scan
bound — `ScanRange(t, []any{"acme"}, []any{"beta"})` is one contiguous
key range.

Type tags are persistent format; they are never renumbered.

What btypedb supplies underneath (the same contract CockroachDB asks
of Pebble): ordered iteration with pivots, atomic multi-key commits
(row + future index writes, all-or-nothing in the WAL), snapshot
isolation via O(1) COW snapshots, `DeleteRange` for `DROP TABLE`, and
fsync-before-ack durability with group commit.

## Roadmap

- [x] **Milestone 1**: order-preserving tuple encoding; catalog; create/drop table; insert/get/delete; ordered scans with range bounds
- [x] **Milestone 2**: secondary indexes as key ranges, backfilled and maintained in the same atomic commit as the row; unique indexes (NULLs exempt); `ScanIndex` with partial-prefix bounds
- [x] **Milestone 3**: `Update` by primary key (pk moves included) with check-before-write index maintenance; `WriteTxn`/`ReadTxn` engine transactions over btypedb's — serializable via single-writer, catalog snapshotted at begin (DDL stays outside transactions)
- [x] **Milestone 4**: column-ID-tagged sparse row values; `AddColumn`/`DropColumn` as metadata-only changes (no row rewrites), with never-reused column IDs
- [ ] **Milestone 5**: SQL frontend — either a hand-rolled subset or the [go-mysql-server](https://github.com/dolthub/go-mysql-server) storage interface for a full dialect
- [ ] Later: DESC key columns (byte inversion), CHECK/NOT NULL constraints, savepoints, EXPLAIN-able planner with filter pushdown to key ranges

## Design notes

- **One writer at a time** (btypedb's model) means serializable
  isolation comes free, SQLite-style. MVCC for concurrent writers is
  explicitly out of scope until a real need appears.
- **btypedb's comparator indexes are not used** — SQL indexes will be
  key ranges, which makes them persistent and replayed from the WAL
  like all other data.
- Columns are typed, so cross-type key ordering never arises within a
  column; the tuple encoding still defines it (by type tag) so that
  corrupt or mixed data fails loudly rather than undefined-ly.
