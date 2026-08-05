package bytdb

import (
	"iter"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// Txn is an engine transaction: one kv snapshot serving data and
// catalog alike. Descriptors resolve lazily from the snapshot itself,
// so the schema a transaction sees is exactly the schema of the data
// it sees — there is no second snapshot to tear. A writable
// transaction sees its own changes and commits them atomically.
//
// Isolation depends on the engine's write mode. Default: writable
// transactions run one at a time, so isolation is serializable. With
// WithConcurrentWrites they overlap under snapshot isolation, and
// Commit can fail with btypedb.ErrTxConflict — a retryable error
// meaning another transaction touching the same keys committed first;
// re-run the transaction from the top. A transaction can individually
// opt up to SERIALIZABLE (WriteTxnSerializable, BeginSerializable):
// its reads are then validated at commit too, closing the write-skew
// and phantom anomalies snapshot isolation admits, at the cost of more
// conflicts. Reads and scans are lock-free over the snapshot in every
// mode.
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
	// dirtySeqPrefixes are counter-key prefixes this transaction
	// deletes transactionally (today: Truncate's RESTART IDENTITY
	// range). In concurrent-writes mode the engine's in-memory
	// allocators must drop their caches when — and only when — those
	// deletes commit, so the prefixes are collected here and flushed by
	// the commit paths. Invalidation is always safe (a re-anchor just
	// re-reads stored state), so a savepoint rollback that undoes the
	// truncate needs no compensating bookkeeping.
	dirtySeqPrefixes []string
}

// flushSeqInvalidations discards allocators for every counter range
// the just-committed transaction deleted. Called only after a
// successful commit — invalidating for a rolled-back delete would be
// harmless but pointless.
func (t *Txn) flushSeqInvalidations() {
	for _, p := range t.dirtySeqPrefixes {
		t.e.invalidateCounterPrefix(p)
	}
	t.dirtySeqPrefixes = nil
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
	return e.writeTxn(false, fn)
}

// WriteTxnSerializable is WriteTxn at SERIALIZABLE isolation. Only
// meaningful with WithConcurrentWrites, where WriteTxn runs at
// snapshot isolation: here the transaction's reads — point gets, FK
// and uniqueness probes, scan ranges — are validated at commit
// alongside its writes, so it commits only if it could have run alone
// (write skew and phantoms conflict instead of committing). The cost
// is a higher conflict rate, particularly for scan-heavy writers, so
// it is opt-in per transaction, as in Postgres. The guarantee spans
// the transactions that ask for it: a snapshot-isolation transaction
// racing a serializable one can still write-skew (also as in
// Postgres). Conflicts surface as btypedb.ErrTxConflict; re-run the
// transaction from the top.
//
// Two engine-level reads stay outside the read set by design:
// sequence/identity draws are non-transactional under concurrent
// writes (see WithConcurrentWrites), and catalog reads need no
// per-transaction validation because every DDL commit conflicts every
// overlapping transaction wholesale.
//
// Without WithConcurrentWrites this is exactly WriteTxn — writers
// fully serialize anyway.
func (e *Engine) WriteTxnSerializable(fn func(tx *Txn) error) error {
	return e.writeTxn(true, fn)
}

func (e *Engine) writeTxn(serializable bool, fn func(tx *Txn) error) error {
	if err := e.checkReentrantWrite("write transaction"); err != nil {
		return err
	}
	// txn escapes the closure so the commit outcome can flush its
	// pending sequence invalidations (see Txn.dirtySeqPrefixes).
	var txn *Txn
	err := e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		if serializable {
			tx.TrackReads()
		}
		// Default mode: the writer lock is ours from here to commit;
		// mark the owning goroutine so its own re-entrant writes fail
		// fast. (In concurrent-writes mode the marker is vestigial —
		// no lock is held across fn and checkReentrantWrite ignores
		// it — but storing it is cheaper than branching on the mode.)
		e.writerGID.Store(curGID())
		// btypedb's Update rolls back on an error RETURN but not on a
		// panic — it has no recover — so a panic in fn would unwind past
		// it with the writer lock still held, wedging every future write
		// process-wide (reads still work, masking it). Roll back here to
		// release the lock, then re-panic so the original failure and its
		// stack are preserved for the caller. This is what makes the doc's
		// "rolled back on error or panic" guarantee true for WriteTxn.
		defer func() {
			if r := recover(); r != nil {
				tx.Rollback()
				panic(r)
			}
		}()
		// Registered after the recover defer so it runs FIRST on unwind
		// (defers are LIFO): the marker must be gone before Rollback
		// releases the writer lock, or a next writer that has already
		// acquired the lock and stored its own GID gets it clobbered
		// back to 0 — silently disabling checkReentrantWrite for it.
		// The normal path clears here too, before Update's commit
		// releases the lock, preserving the same ordering.
		defer e.writerGID.Store(0)
		txn = &Txn{tx: tx, e: e}
		return fn(txn)
	})
	if err == nil && txn != nil {
		txn.flushSeqInvalidations()
	}
	return err
}

// guardPanic wraps a kv transaction closure with the recover→rollback→
// re-panic pattern writeTxn documents: btypedb's Update/UpdateExclusive
// have no recover of their own, so an escaping panic would hold the
// writer lock forever. Used by updateDDL, whose closures can run
// caller-supplied callbacks (AddCheck's validate, AlterSequence's
// mutate) that this package cannot vouch for.
func guardPanic(fn func(tx *btypedb.Tx[string, []byte]) error) func(tx *btypedb.Tx[string, []byte]) error {
	return func(tx *btypedb.Tx[string, []byte]) error {
		defer func() {
			if r := recover(); r != nil {
				tx.Rollback()
				panic(r)
			}
		}()
		return fn(tx)
	}
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
// Rollback. In the default mode a writable transaction takes the
// engine's single-writer lock at Begin and holds it until it ends:
// other writable transactions, one-shot writes, and DDL block behind
// it (reads and read-only transactions do not). In concurrent-writes
// mode Begin never blocks; contention surfaces at Commit as
// btypedb.ErrTxConflict instead. Prefer WriteTxn and ReadTxn, which
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

// BeginSerializable starts a writable transaction at SERIALIZABLE
// isolation — Begin(true) with the transaction's reads validated at
// commit; see WriteTxnSerializable for the guarantee and its scope.
// It exists for callers whose transaction boundaries arrive from
// outside (a SQL session's BEGIN ISOLATION LEVEL SERIALIZABLE);
// embedded callers should prefer WriteTxnSerializable, which cannot
// leak the transaction.
func (e *Engine) BeginSerializable() (*Txn, error) {
	t, err := e.Begin(true)
	if err != nil {
		return nil, err
	}
	t.tx.TrackReads()
	return t, nil
}

// TrackReads upgrades an open writable transaction to SERIALIZABLE:
// from this call on, its reads join commit-time validation exactly as
// under BeginSerializable. It exists for the one caller that learns
// the isolation level after Begin — a SQL session handling SET
// TRANSACTION ISOLATION LEVEL SERIALIZABLE as its block's first
// statement. Reads performed before the call are NOT retroactively
// tracked, so the guarantee only holds when nothing has been read
// yet; the SQL layer enforces "before any query" (as Postgres does)
// rather than this method trying to detect it.
func (t *Txn) TrackReads() { t.tx.TrackReads() }

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
	// Clear the reentrancy marker BEFORE commit releases the writer
	// lock. Cleared after, the store races with the next writer: it can
	// acquire the lock and store its own GID in the gap, and our late
	// Store(0) would erase it — silently disabling checkReentrantWrite
	// for that transaction. Clearing early is safe: the owning goroutine
	// issues no further engine writes once commit begins, and other
	// goroutines were always allowed to block on the lock.
	t.releaseWriter()
	err := t.tx.Commit()
	if err == nil {
		t.flushSeqInvalidations()
	}
	return err
}

// Rollback discards the transaction's writes and releases it. Rolling
// back a finished transaction is a no-op.
func (t *Txn) Rollback() error {
	t.releaseWriter() // before the lock is released — see Commit
	return t.tx.Rollback()
}

// releaseWriter clears the engine's reentrancy marker once a writable
// Begin transaction ends, whatever the outcome — the writer lock is
// released either way. A no-op for read transactions and for the
// closure-scoped WriteTxn, which clears its own marker. One-shot:
// releaseW flips off on first use so the common `defer tx.Rollback()`
// after a successful Commit cannot re-clear a marker that by then
// belongs to the next writer.
func (t *Txn) releaseWriter() {
	if t.releaseW {
		t.releaseW = false
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
	stored, err := insertRow(t.e, t.tx, desc, vals)
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

// ReferencingFKs returns every foreign key, on any table in the
// transaction's catalog view, that references the named table —
// including the table's references to itself. The SQL layer's
// DELETE/UPDATE/TRUNCATE enforcement is built on it.
func (t *Txn) ReferencingFKs(table string) ([]FKRef, error) {
	return t.e.referencingFKs(t.tx, table, false)
}

// Truncate removes every row of the table — and its entries in every
// secondary index — as one range delete over the table's key space,
// leaving the schema untouched. Unlike a row-at-a-time DELETE, it
// never decodes a row, so it is O(rows) in the kv store's bulk delete
// rather than in decode+per-index maintenance. restartIdentity
// additionally deletes the table's identity counters, so the next
// insert draws from 1 again (TRUNCATE ... RESTART IDENTITY); without
// it counters keep counting, as Postgres defaults (CONTINUE IDENTITY).
func (t *Txn) Truncate(table string, restartIdentity bool) error {
	desc, err := t.desc(table)
	if err != nil {
		return err
	}
	// One prefix covers primary rows and every index: the key space is
	// tuple(tableID, indexID, ...), and this deletes all of tableID.
	prefix := tableSpace(desc.ID)
	if _, err := t.tx.DeleteRange(string(prefix), string(tuple.PrefixEnd(prefix))); err != nil {
		return serr.Wrap(err, "op", "truncate", "table", table)
	}
	if restartIdentity {
		idPrefix := identitySeqTablePrefix(desc.ID)
		if _, err := t.tx.DeleteRange(string(idPrefix), string(tuple.PrefixEnd(idPrefix))); err != nil {
			return serr.Wrap(err, "op", "truncate", "table", table)
		}
		if t.e.occ {
			// The engine's in-memory allocators must drop this table's
			// counters when the delete commits, or cached draws would
			// keep counting past the restart. Deferred to commit: an
			// aborted truncate must not disturb them. (A concurrent
			// insert that draws between our commit and the flush keeps
			// its high value — it raced the truncate and lost nothing
			// but gaplessness; the primary key check still guards
			// against collisions either way.)
			t.dirtySeqPrefixes = append(t.dirtySeqPrefixes, string(idPrefix))
		}
	}
	return nil
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
	// A pure read either way — marked hit or miss, since the absence of
	// a row is as much information as its contents (serializable mode).
	t.tx.MarkRead(key)
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
