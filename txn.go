package bytdb

import (
	"iter"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/serr"
)

// Txn is an engine transaction: a consistent snapshot of both data and
// catalog, taken at begin. A writable transaction sees its own changes
// and commits them atomically; only one runs at a time, so isolation
// is serializable. Reads and scans are lock-free over the snapshot.
//
// DDL (CreateTable, CreateIndex, ...) cannot run inside a transaction
// — a schema change is its own transaction.
type Txn struct {
	tx     *btypedb.Tx[string, []byte]
	tables map[string]*TableDesc
	// e is set only on writable transactions from Begin, so Commit and
	// Rollback can clear the engine's reentrancy marker (writerGID).
	e *Engine
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
		return fn(&Txn{tx: tx, tables: e.catalogSnapshot()})
	})
}

// ReadTxn runs fn over a read-only snapshot: a point-in-time view of
// every table, unaffected by concurrent writes. Writes through it
// return btypedb.ErrTxNotWritable.
//
// The catalog and data snapshots are taken atomically (see
// readSnapshot): fn never sees, say, an index in the catalog whose
// backfill postdates the data snapshot.
func (e *Engine) ReadTxn(fn func(tx *Txn) error) error {
	for {
		tables, ver := e.stableCatalogSnapshot()
		retry := false
		err := e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
			// The kv snapshot exists now; if the catalog version still
			// matches, tables describes exactly this snapshot's schema.
			if e.catalogVersion() != ver {
				retry = true
				return nil
			}
			return fn(&Txn{tx: tx, tables: tables})
		})
		if retry {
			continue // a DDL publish raced the snapshot; retake both
		}
		return err
	}
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
		// A writable Begin needs no version dance: it blocks on the
		// kv writer lock, which every DDL holds through commit and
		// publish, so the catalog read after Begin returns is always
		// exactly the schema of the acquired snapshot.
		tx, err := e.kv.Begin(true)
		if err != nil {
			return nil, err
		}
		e.writerGID.Store(curGID())
		return &Txn{tx: tx, tables: e.catalogSnapshot(), e: e}, nil
	}
	return e.readSnapshot()
}

// readSnapshot opens a read-only transaction whose catalog and data
// views are mutually consistent. The two snapshots cannot be taken
// under one lock (see catVer for why), so it validates seqlock-style:
// take the catalog at a stable (even) version, open the kv snapshot,
// and retry if the version moved in between. An unchanged even version
// proves no DDL published across the pair — without this a concurrent
// CreateIndex could hand the reader an index whose backfill the data
// snapshot predates, and index scans would silently miss every row.
func (e *Engine) readSnapshot() (*Txn, error) {
	for {
		tables, ver := e.stableCatalogSnapshot()
		tx, err := e.kv.Begin(false)
		if err != nil {
			return nil, err
		}
		if e.catalogVersion() == ver {
			return &Txn{tx: tx, tables: tables}, nil
		}
		tx.Rollback() // a DDL publish raced the snapshot; retake both
	}
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
	if t.e != nil {
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
// catalog snapshot, or nil if absent.
func (t *Txn) Table(name string) *TableDesc {
	return t.tables[name]
}

func (t *Txn) desc(table string) (*TableDesc, error) {
	d := t.tables[table]
	if d == nil {
		return nil, serr.New("no such table", "table", table)
	}
	return d, nil
}

// Insert stores one row within the transaction (see Engine.Insert).
func (t *Txn) Insert(table string, vals ...any) error {
	desc, err := t.desc(table)
	if err != nil {
		return err
	}
	if err := insertRow(t.tx, desc, vals); err != nil {
		return serr.Wrap(err, "op", "insert", "table", table)
	}
	return nil
}

// Update modifies a row within the transaction (see Engine.Update).
// A failed update stages no writes, so the transaction remains
// committable if the error is handled.
func (t *Txn) Update(table string, pkVals []any, set map[string]any) (bool, error) {
	desc, err := t.desc(table)
	if err != nil {
		return false, err
	}
	updated, err := updateRow(t.tx, desc, pkVals, set)
	if err != nil {
		return false, serr.Wrap(err, "op", "update", "table", table)
	}
	return updated, nil
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
