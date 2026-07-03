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
}

// WriteTxn runs fn in a writable transaction: committed if fn returns
// nil, rolled back on error or panic. Do not call the Engine's one-shot
// write methods (Insert, Update, Delete, DDL) inside fn — they would
// block behind this transaction; use the Txn methods.
func (e *Engine) WriteTxn(fn func(tx *Txn) error) error {
	return e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		return fn(&Txn{tx: tx, tables: e.catalogSnapshot()})
	})
}

// ReadTxn runs fn over a read-only snapshot: a point-in-time view of
// every table, unaffected by concurrent writes. Writes through it
// return btypedb.ErrTxNotWritable.
func (e *Engine) ReadTxn(fn func(tx *Txn) error) error {
	return e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		return fn(&Txn{tx: tx, tables: e.catalogSnapshot()})
	})
}

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
