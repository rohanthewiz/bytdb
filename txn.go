package bytdb

import (
	"iter"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/serr"
)

// Txn is an engine transaction: one kv snapshot serving data and
// catalog alike. Descriptors resolve lazily from the snapshot itself,
// so the schema a transaction sees is exactly the schema of the data
// it sees — there is no second snapshot to tear. A writable
// transaction sees its own changes and commits them atomically; only
// one runs at a time, so isolation is serializable. Reads and scans
// are lock-free over the snapshot.
//
// DDL (CreateTable, CreateIndex, ...) cannot run inside a transaction
// — a schema change is its own transaction.
type Txn struct {
	tx *btypedb.Tx[string, []byte]
	e  *Engine
	// releaseW marks a writable transaction from Begin, so Commit and
	// Rollback clear the engine's reentrancy marker (writerGID).
	releaseW bool
	// descs memoizes descriptor resolutions for the transaction's
	// lifetime (including nil for absent tables — the snapshot cannot
	// change underneath, so a miss is a miss for good).
	descs map[string]*TableDesc
}

// WriteTxn runs fn in a writable transaction: committed if fn returns
// nil, rolled back on error or panic. Do not call the Engine's one-shot
// write methods (Insert, Update, Delete, DDL) inside fn — they would
// block behind this transaction; the engine detects that and returns
// an error instead of deadlocking. Use the Txn methods.
//
// A commit error does not guarantee the writes were discarded: the kv
// store makes a commit visible to new snapshots before its WAL append
// and fsync, so a failure there can leave the writes readable until
// the process restarts (replay then drops them). Treat a commit error
// as "durability unknown," not "definitely not applied."
func (e *Engine) WriteTxn(fn func(tx *Txn) error) error {
	if err := e.checkReentrantWrite("write transaction"); err != nil {
		return err
	}
	return e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		// The writer lock is ours from here to commit; mark the owning
		// goroutine so its own re-entrant writes fail fast.
		e.writerGID.Store(curGID())
		defer e.writerGID.Store(0)
		return fn(&Txn{tx: tx, e: e})
	})
}

// ReadTxn runs fn over a read-only snapshot: a point-in-time view of
// every table, unaffected by concurrent writes. Writes through it
// return btypedb.ErrTxNotWritable.
//
// Catalog and data share the one snapshot, so fn can never see, say,
// an index in the catalog whose backfill postdates its data.
func (e *Engine) ReadTxn(fn func(tx *Txn) error) error {
	return e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		return fn(&Txn{tx: tx, e: e})
	})
}

// Begin starts a transaction the caller must end with Commit or
// Rollback. A writable transaction takes the engine's single-writer
// lock at Begin and holds it until it ends: other writable
// transactions, one-shot writes, and DDL block behind it (reads and
// read-only transactions do not). Prefer WriteTxn and ReadTxn, which
// cannot leak the lock; Begin exists for callers whose transaction
// boundaries arrive from outside, like a SQL session's BEGIN/COMMIT.
func (e *Engine) Begin(writable bool) (*Txn, error) {
	if writable {
		if err := e.checkReentrantWrite("begin"); err != nil {
			return nil, err
		}
		tx, err := e.kv.Begin(true)
		if err != nil {
			return nil, err
		}
		e.writerGID.Store(curGID())
		return &Txn{tx: tx, e: e, releaseW: true}, nil
	}
	return e.readSnapshot()
}

// readSnapshot opens a read-only transaction. Catalog consistency is
// free: descriptors resolve from the same snapshot as the data.
func (e *Engine) readSnapshot() (*Txn, error) {
	tx, err := e.kv.Begin(false)
	if err != nil {
		return nil, err
	}
	return &Txn{tx: tx, e: e}, nil
}

// Commit publishes the transaction's writes atomically and releases
// it. Only for transactions from Begin; WriteTxn and ReadTxn finish
// theirs themselves.
//
// A commit error does not guarantee the writes were discarded — see
// WriteTxn on why a failed commit can leave them visible until the
// process restarts.
func (t *Txn) Commit() error {
	err := t.tx.Commit()
	t.releaseWriter()
	return err
}

// Rollback discards the transaction's writes and releases it. Rolling
// back a finished transaction is a no-op.
func (t *Txn) Rollback() error {
	err := t.tx.Rollback()
	t.releaseWriter()
	return err
}

// releaseWriter clears the engine's reentrancy marker once a writable
// Begin transaction ends, whatever the outcome — the writer lock is
// released either way. A no-op for read transactions and for the
// closure-scoped WriteTxn, which clears its own marker.
func (t *Txn) releaseWriter() {
	if t.releaseW {
		t.e.writerGID.Store(0)
	}
}

// Savepoint marks a point within a transaction that RollbackTo can
// restore: an O(1) copy-on-write snapshot of the transaction's state.
// The catalog needs no mark of its own — DDL cannot run inside a
// transaction, so the schema cannot change between the mark and the
// rollback.
type Savepoint = btypedb.Savepoint[string, []byte]

// Savepoint captures the transaction's current state. Savepoints
// nest; rolling back to or releasing an earlier one destroys the
// later ones, and any still outstanding at Commit or Rollback are
// cleaned up with the transaction.
func (t *Txn) Savepoint() (*Savepoint, error) { return t.tx.Savepoint() }

// RollbackTo restores the transaction to the state sp captured,
// discarding every change made after it. sp itself stays valid.
func (t *Txn) RollbackTo(sp *Savepoint) error { return t.tx.RollbackTo(sp) }

// Release discards sp — and every savepoint created after it — while
// keeping all of the transaction's changes.
func (t *Txn) Release(sp *Savepoint) error { return t.tx.Release(sp) }

// Table returns the descriptor for a table name in the transaction's
// view, or nil if absent. A corrupt stored descriptor also reads as
// nil here; desc, which every data path uses, surfaces it as an error.
func (t *Txn) Table(name string) *TableDesc {
	d, _ := t.table(name)
	return d
}

// table resolves and memoizes one descriptor from the transaction's
// snapshot; nil means the table does not exist there.
func (t *Txn) table(name string) (*TableDesc, error) {
	if d, ok := t.descs[name]; ok {
		return d, nil
	}
	d, err := t.e.tableFromView(t.tx, name)
	if err != nil {
		return nil, err
	}
	if t.descs == nil {
		t.descs = map[string]*TableDesc{}
	}
	t.descs[name] = d
	return d, nil
}

func (t *Txn) desc(table string) (*TableDesc, error) {
	d, err := t.table(table)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, serr.New("no such table", "table", table)
	}
	return d, nil
}

// Insert stores one row within the transaction (see Engine.Insert).
func (t *Txn) Insert(table string, vals ...any) error {
	_, err := t.InsertReturning(table, vals...)
	return err
}

// InsertReturning is Insert, additionally returning the row as stored:
// values coerced to their column types and identity columns filled.
// This is what INSERT ... RETURNING reports — the engine is the only
// party that knows a drawn identity value, so it must hand the final
// row back rather than have callers reconstruct it.
func (t *Txn) InsertReturning(table string, vals ...any) (Row, error) {
	desc, err := t.desc(table)
	if err != nil {
		return Row{}, err
	}
	stored, err := insertRow(t.tx, desc, vals)
	if err != nil {
		return Row{}, serr.Wrap(err, "op", "insert", "table", table)
	}
	return Row{Desc: desc, Vals: stored}, nil
}

// Update modifies a row within the transaction (see Engine.Update).
// A failed update stages no writes, so the transaction remains
// committable if the error is handled.
func (t *Txn) Update(table string, pkVals []any, set map[string]any) (bool, error) {
	_, updated, err := t.UpdateReturning(table, pkVals, set)
	return updated, err
}

// UpdateReturning is Update, additionally returning the row as stored
// (a zero Row and false when no row matched). RETURNING reports these
// values instead of re-applying the SET map itself so it can never
// drift from the engine's own coercion.
func (t *Txn) UpdateReturning(table string, pkVals []any, set map[string]any) (Row, bool, error) {
	desc, err := t.desc(table)
	if err != nil {
		return Row{}, false, err
	}
	newVals, updated, err := updateRow(t.tx, desc, pkVals, set)
	if err != nil {
		return Row{}, false, serr.Wrap(err, "op", "update", "table", table)
	}
	if !updated {
		return Row{}, false, nil
	}
	return Row{Desc: desc, Vals: newVals}, true, nil
}

// Delete removes a row within the transaction (see Engine.Delete).
func (t *Txn) Delete(table string, pkVals ...any) (bool, error) {
	desc, err := t.desc(table)
	if err != nil {
		return false, err
	}
	existed, err := deleteRow(t.tx, desc, pkVals)
	if err != nil {
		return false, serr.Wrap(err, "op", "delete", "table", table)
	}
	return existed, nil
}

// Get returns the row with the given primary-key values in the
// transaction's view, including its own uncommitted writes.
func (t *Txn) Get(table string, pkVals ...any) (Row, bool, error) {
	desc, err := t.desc(table)
	if err != nil {
		return Row{}, false, err
	}
	key, err := fullPKKey(desc, pkVals)
	if err != nil {
		return Row{}, false, err
	}
	val, ok := t.tx.Get(key)
	if !ok {
		return Row{}, false, nil
	}
	row, err := decodeRow(desc, key, val)
	return row, err == nil, err
}

// Scan iterates every row of the table in the transaction's view, in
// primary-key order.
func (t *Txn) Scan(table string) iter.Seq2[Row, error] {
	return t.ScanRange(table, nil, nil)
}

// ScanRange iterates rows with fromPK <= pk < toPK in the
// transaction's view (see Engine.ScanRange).
func (t *Txn) ScanRange(table string, fromPK, toPK []any) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		desc, err := t.desc(table)
		if err != nil {
			yield(Row{}, err)
			return
		}
		scanRows(t.tx, desc, fromPK, toPK)(yield)
	}
}

// ScanIndex iterates rows in the named index's order in the
// transaction's view (see Engine.ScanIndex).
func (t *Txn) ScanIndex(table, index string, from, to []any) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		desc, err := t.desc(table)
		if err != nil {
			yield(Row{}, err)
			return
		}
		idx := desc.Index(index)
		if idx == nil {
			yield(Row{}, serr.New("no such index", "table", table, "index", index))
			return
		}
		scanIndexRows(t.tx, desc, idx, from, to)(yield)
	}
}

// ScanRangeRev iterates rows with fromPK <= pk < toPK in descending
// primary-key order in the transaction's view (see Engine.ScanRangeRev,
// including toIncl's prefix-group upper bound).
func (t *Txn) ScanRangeRev(table string, fromPK, toPK []any, toIncl bool) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		desc, err := t.desc(table)
		if err != nil {
			yield(Row{}, err)
			return
		}
		scanRowsRev(t.tx, desc, fromPK, toPK, toIncl)(yield)
	}
}

// ScanIndexRev iterates rows in descending order of the named index in
// the transaction's view (see Engine.ScanIndexRev).
func (t *Txn) ScanIndexRev(table, index string, from, to []any, toIncl bool) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		desc, err := t.desc(table)
		if err != nil {
			yield(Row{}, err)
			return
		}
		idx := desc.Index(index)
		if idx == nil {
			yield(Row{}, serr.New("no such index", "table", table, "index", index))
			return
		}
		scanIndexRowsRev(t.tx, desc, idx, from, to, toIncl)(yield)
	}
}
