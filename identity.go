package bytdb

import (
	"encoding/binary"
	"fmt"
	"math"
	"slices"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/serr"
)

// identity.go: identity (auto-increment) columns. Each identity column
// owns a durable counter in the system sequences table, keyed by
// (tableID, colID) — see identitySeqKey — created lazily on first
// draw and cleaned up by DropTable and DropColumn. Insert resolves
// identity columns before coercion: NULL draws the next value,
// starting at 1; an explicit non-negative value bumps the counter
// past itself so later draws cannot collide (an explicit negative
// value never touches the counter). RestartIdentity sets a counter
// outright (ALTER TABLE ... ALTER COLUMN ... RESTART) and PeekIdentity
// reads it without drawing; the shell's .dump pairs the two to carry
// counters across a dump and restore.
//
// In the default (serialized) mode draws are transactional: values
// drawn inside a rolled-back transaction are returned to the counter.
// In concurrent-writes mode they route to the engine's
// non-transactional allocators (seqalloc.go) — rolled-back draws are
// burned, as in Postgres — because a transactional counter would make
// every pair of concurrent inserts into the table conflict.

func (d *TableDesc) hasIdentity() bool {
	for i := range d.Columns {
		if d.Columns[i].Identity {
			return true
		}
	}
	return false
}

// fillIdentity resolves a row's identity columns, returning the
// resolved values (the input is not mutated) plus the ordinals of the
// columns it auto-drew (nil when none). A row with the wrong arity
// passes through untouched for coerceRow to report. Draws and bumps
// follow the mode split documented in the file header: through e's
// allocators in concurrent-writes mode; in serialized mode through ids,
// the transaction's in-memory counters, when the caller is a Txn, and
// straight through tx otherwise (a one-shot write draws once per
// transaction, so there is nothing to batch).
//
// drawn exists for error classification: a uniqueness collision on a
// value the ENGINE chose (as opposed to one the caller supplied) can
// only mean the draw raced a concurrent counter reset or bump — under
// concurrent writes, insertRow reports that as a retryable conflict
// rather than a data error.
func fillIdentity(e *Engine, tx *btypedb.Tx[string, []byte], ids *idCounters, desc *TableDesc, vals []any) (out []any, drawn []int, err error) {
	if !desc.hasIdentity() || len(vals) != len(desc.Columns) {
		return vals, nil, nil
	}
	out = slices.Clone(vals)
	for i := range desc.Columns {
		c := &desc.Columns[i]
		if !c.Identity {
			continue
		}
		if out[i] == nil {
			var v uint64
			var err error
			switch {
			case e.occ:
				v, err = e.allocCounter(identitySeqKey(desc.ID, c.ID), 1, identityName(desc, c), identityOwner(e, desc))
			case ids != nil:
				v, err = ids.draw(tx, desc, c)
			default:
				v, err = nextFromCounter(tx, identitySeqKey(desc.ID, c.ID), 1, identityName(desc, c))
			}
			if err != nil {
				return nil, nil, err
			}
			// The counter is unsigned but the column is int64.
			if v > math.MaxInt64 {
				return nil, nil, serr.New("identity column exhausted",
					"table", desc.Name, "column", c.Name)
			}
			out[i] = int64(v)
			drawn = append(drawn, i)
			continue
		}
		cv, err := coerce(out[i], TInt)
		if err != nil {
			return nil, nil, serr.Wrap(err, "table", desc.Name, "column", c.Name)
		}
		if n := cv.(int64); n >= 0 {
			if err := bumpIdentity(e, tx, ids, desc, c, uint64(n)+1); err != nil {
				return nil, nil, err
			}
		}
	}
	return out, drawn, nil
}

// bumpIdentity raises column c's counter to at least next, through the
// same three routes as a draw in fillIdentity.
func bumpIdentity(e *Engine, tx *btypedb.Tx[string, []byte], ids *idCounters, desc *TableDesc, c *Column, next uint64) error {
	switch {
	case e.occ:
		return e.bumpCounterAllocTo(identitySeqKey(desc.ID, c.ID), next, identityName(desc, c), identityOwner(e, desc))
	case ids != nil:
		return ids.bump(tx, desc, c, next)
	default:
		return bumpCounterTo(tx, identitySeqKey(desc.ID, c.ID), next, identityName(desc, c))
	}
}

// identityName is the "table.column" a counter error names. Built on
// demand: the batched draw path needs it only when it fails, and it
// used to cost an allocation per identity column per row.
func identityName(desc *TableDesc, c *Column) string { return desc.Name + "." + c.Name }

// idCounters holds a serialized-mode write transaction's identity
// counters in memory between draws (deep-look D-45). A draw used to be
// a read-modify-write of the counter's kv record, and every write
// stages a sealed WAL record in the transaction, so a 1000-row INSERT
// into a table keyed by an identity column logged 1000 counter
// records, all but the last of them dead on arrival. Now a counter is
// read from the transaction once, advanced in memory, and written back
// once:
//
//	draw/bump ──► idCounters (next, dirty) ──flush──► tx.Set(key, next)
//	                                          ▲
//	              Txn.Commit, WriteTxn's commit, Txn.Savepoint
//
// Correctness rests on every reader and writer of a counter inside the
// transaction going through this cache or settling with it first:
//
//   - Commit (Txn.Commit, and WriteTxn after its closure) flushes, so
//     the final values commit atomically with the rows that drew them.
//   - Savepoint flushes before taking the kv mark, so the mark holds
//     the counters as of that moment. RollbackTo restores the kv state
//     and then drops the cache; the next draw reloads the counter from
//     the restored state, exactly what the per-row writes used to give.
//   - Rollback needs nothing: the cache dies with the Txn.
//   - RestartIdentity and TRUNCATE ... RESTART IDENTITY write or delete
//     counter records directly, so they drop the affected entries
//     first; a stale entry flushed later would undo the restart.
//   - PeekIdentity reads a dirty entry before the stored record.
//
// DDL never runs inside a Txn (it commits as its own transaction), so
// nothing else touches identity counters while one is open, and in
// serialized mode no other writer runs either. Concurrent-writes mode
// never uses this: its draws go through the engine's allocators.
//
// A Txn is used by one goroutine at a time, like the kv transaction
// under it, so the map needs no lock.
type idCounters struct {
	m map[idCounterKey]*idCounter
}

// idCounterKey names one identity counter without building its kv key,
// which takes an encoding and an allocation.
type idCounterKey struct {
	table uint64
	col   uint32
}

// idCounter is one cached counter.
type idCounter struct {
	key   string // the counter's kv key, built once at load for the flush
	next  uint64 // the value the next draw hands out
	dirty bool   // next has moved since the counter was loaded or flushed
}

// load returns c's cached counter, reading it from tx on first use. An
// absent record reads as 1, the value nextFromCounter starts at.
func (ic *idCounters) load(tx *btypedb.Tx[string, []byte], desc *TableDesc, c *Column) (*idCounter, error) {
	k := idCounterKey{desc.ID, c.ID}
	if ctr, ok := ic.m[k]; ok {
		return ctr, nil
	}
	key := identitySeqKey(desc.ID, c.ID)
	next := uint64(1)
	if raw, ok := tx.Get(key); ok {
		if len(raw) != 8 {
			return nil, serr.New("sequence value is corrupt",
				"sequence", identityName(desc, c), "len", fmt.Sprint(len(raw)))
		}
		next = binary.BigEndian.Uint64(raw)
	}
	ctr := &idCounter{key: key, next: next}
	if ic.m == nil {
		ic.m = map[idCounterKey]*idCounter{}
	}
	ic.m[k] = ctr
	return ctr, nil
}

// draw hands out c's next value: nextFromCounter's semantics, with
// the write deferred to flush.
func (ic *idCounters) draw(tx *btypedb.Tx[string, []byte], desc *TableDesc, c *Column) (uint64, error) {
	// Refuse where the immediate write would have. insertRow already
	// refuses a read-only transaction before drawing, so this is the
	// backstop for any other caller: a cached draw there would
	// "succeed", leave the cache dirty, and fail later at commit, far
	// from the insert that caused it.
	if !tx.Writable() {
		return 0, btypedb.ErrTxNotWritable
	}
	ctr, err := ic.load(tx, desc, c)
	if err != nil {
		return 0, err
	}
	if ctr.next == math.MaxUint64 {
		return 0, serr.New("sequence exhausted", "sequence", identityName(desc, c))
	}
	v := ctr.next
	ctr.next++
	ctr.dirty = true
	return v, nil
}

// bump raises c's counter to at least next: bumpCounterTo's semantics,
// with the write deferred to flush. An absent counter loads as 1, so a
// bump to 1 or below leaves it absent, as bumpCounterTo does.
func (ic *idCounters) bump(tx *btypedb.Tx[string, []byte], desc *TableDesc, c *Column, next uint64) error {
	ctr, err := ic.load(tx, desc, c)
	if err != nil {
		return err
	}
	if ctr.next >= next {
		return nil
	}
	// Writable is checked only when the bump would write, matching
	// bumpCounterTo, whose no-op path never reaches tx.Set.
	if !tx.Writable() {
		return btypedb.ErrTxNotWritable
	}
	ctr.next, ctr.dirty = next, true
	return nil
}

// flush writes every counter that moved since its load or the last
// flush back to tx. Entries stay cached and clean, so draws after a
// savepoint keep reading from memory.
func (ic *idCounters) flush(tx *btypedb.Tx[string, []byte]) error {
	for _, ctr := range ic.m {
		if !ctr.dirty {
			continue
		}
		if err := tx.Set(ctr.key, binary.BigEndian.AppendUint64(nil, ctr.next)); err != nil {
			return serr.Wrap(err, "op", "write identity counter")
		}
		ctr.dirty = false
	}
	return nil
}

// reset drops every cached counter, flushed or not. For use after the
// kv state was rewound (RollbackTo): what the cache holds belongs to
// the discarded branch.
func (ic *idCounters) reset() { clear(ic.m) }

// forget drops one counter without flushing it, for a caller about to
// overwrite its record (RestartIdentity).
func (ic *idCounters) forget(tableID uint64, colID uint32) {
	delete(ic.m, idCounterKey{tableID, colID})
}

// forgetTable drops every counter of one table without flushing, for a
// caller about to delete their records (TRUNCATE ... RESTART IDENTITY).
func (ic *idCounters) forgetTable(tableID uint64) {
	for k := range ic.m {
		if k.table == tableID {
			delete(ic.m, k)
		}
	}
}

// peek reports c's cached next value when the cache holds an unflushed
// one; a clean or absent entry means the stored record is current.
func (ic *idCounters) peek(tableID uint64, colID uint32) (uint64, bool) {
	if ctr, ok := ic.m[idCounterKey{tableID, colID}]; ok && ctr.dirty {
		return ctr.next, true
	}
	return 0, false
}

// identityOwner returns the reservation guard for desc's identity
// counters (see counterAlloc.reserveLocked): creating one is allowed
// only while the table — this table, by ID, not a same-named successor
// — still exists in the reservation's snapshot. Losing that race is
// reported as the retryable conflict it is: the inserting transaction
// cannot commit across the DROP anyway, and its retry sees the table
// gone.
func identityOwner(e *Engine, desc *TableDesc) func(kvView) error {
	return func(v kvView) error {
		d, err := e.tableFromView(v, desc.Name)
		if err != nil {
			return err
		}
		if d == nil || d.ID != desc.ID {
			return serr.Wrap(btypedb.ErrTxConflict,
				"reason", "table was dropped concurrently", "table", desc.Name)
		}
		return nil
	}
}

// bumpIdentityOnUpdate raises the counter of every identity column an
// update sets, so later draws land past the new value. Bumps follow the
// same mode split as fillIdentity: transactional in serialized mode
// (rolled back with the update; through ids when the caller is a Txn),
// through the non-transactional allocator under concurrent writes
// (they stand, at worst a gap). Negative values never collide with
// draws, which start at 1.
func bumpIdentityOnUpdate(e *Engine, tx *btypedb.Tx[string, []byte], ids *idCounters, desc *TableDesc, set map[string]any, newVals []any) error {
	if !desc.hasIdentity() {
		return nil
	}
	for col := range set {
		ord := desc.ColIndex(col)
		if ord < 0 || !desc.Columns[ord].Identity {
			continue
		}
		n, ok := newVals[ord].(int64)
		if !ok || n < 0 {
			continue
		}
		if err := bumpIdentity(e, tx, ids, desc, &desc.Columns[ord], uint64(n)+1); err != nil {
			return err
		}
	}
	return nil
}

// drawnRaced reports whether a uniqueness collision on the key columns
// cols can be blamed on an identity draw racing concurrent counter
// movement — true when this insert auto-drew any of those columns under
// concurrent writes. Draws there are non-transactional (seqalloc.go),
// so OCC validation cannot see them; the two ways a drawn value can
// collide are both races with another session, not caller mistakes:
//
//   - TRUNCATE ... RESTART IDENTITY commits between another writer's
//     draw and the engine-level allocator invalidation that follows it
//     (Txn.Truncate defers the flush to commit). The restarted counter
//     re-issues values the stale cache handed out moments earlier.
//   - An explicit value inserted concurrently collides with a draw that
//     happened just before its counter bump landed.
//
// Either way the uniqueness check holds — nothing commits twice — but
// the loser deserves btypedb.ErrTxConflict ("re-run, you raced"), not a
// duplicate-key error that reads as corruption for a value the caller
// never chose. In serialized mode draws are transactional and
// single-writer, so a collision there can only follow a deliberate
// backward SetSeq; the plain duplicate error stands (as in Postgres).
func drawnRaced(e *Engine, cols, drawn []int) bool {
	if !e.occ {
		return false
	}
	for _, d := range drawn {
		if slices.Contains(cols, d) {
			return true
		}
	}
	return false
}

// identityCol resolves table.column to its descriptor and ordinal,
// refusing a column that is not an identity column with Postgres's
// wording (SQLSTATE 55000 over the wire).
func identityCol(desc *TableDesc, table, column string) (*Column, error) {
	ord := desc.ColIndex(column)
	if ord < 0 {
		return nil, serr.New(`column "`+column+`" of relation "`+table+`" does not exist`,
			"table", table, "column", column)
	}
	c := &desc.Columns[ord]
	if !c.Identity {
		return nil, serr.New(`column "`+column+`" of relation "`+table+`" is not an identity column`,
			"table", table, "column", column)
	}
	return c, nil
}

// RestartIdentity sets the value the identity column column of table
// draws next: ALTER TABLE t ALTER COLUMN c RESTART [WITH next]. next
// must lie in 1..math.MaxInt64, the range an identity column draws
// from (it starts at 1 and its values are int64). As with SetSeq,
// restarting below values already in the table makes later draws
// collide with them; that is the caller's contract (it exists for
// restore, where .dump replays the counter after the rows).
//
// The write is part of the transaction in both write modes, so a
// rollback (or ROLLBACK TO a savepoint before it) undoes it, the same
// as TRUNCATE ... RESTART IDENTITY, which this mirrors. In
// concurrent-writes mode the engine's cached allocator for the counter
// is discarded when the transaction commits (see dirtySeqPrefixes),
// so the next draw re-anchors on the stored value. The race that
// leaves open is Truncate's: a draw served from the old cache between
// the commit and the flush can repeat a restarted value, and the
// uniqueness probe turns that into a retryable conflict (drawnRaced).
func (t *Txn) RestartIdentity(table, column string, next int64) error {
	desc, err := t.desc(table)
	if err != nil {
		return err
	}
	c, err := identityCol(desc, table, column)
	if err != nil {
		return err
	}
	// MINVALUE is always 1 for a bytdb identity, and int64 caps the
	// top, so only the bottom can be violated here.
	if next < 1 {
		return serr.New("RESTART value ("+fmtInt(next)+") cannot be less than MINVALUE (1)",
			"table", table, "column", column)
	}
	key := identitySeqKey(desc.ID, c.ID)
	// The record is overwritten below; a cached entry flushed after it
	// would put the old position back (see idCounters).
	t.ids.forget(desc.ID, c.ID)
	if err := t.tx.Set(key, binary.BigEndian.AppendUint64(nil, uint64(next))); err != nil {
		return serr.Wrap(err, "op", "restart identity", "table", table, "column", column)
	}
	if t.e.occ {
		// A full key works as a prefix: invalidation is always safe,
		// so matching a neighbour's key too would only burn its cache.
		t.dirtySeqPrefixes = append(t.dirtySeqPrefixes, key)
	}
	return nil
}

// PeekIdentity reports the value the identity column column of table
// would draw next, without drawing it. ok is false when the counter
// does not exist yet: nothing has been drawn or inserted explicitly
// since the table was created (or truncated with RESTART IDENTITY), so
// the next draw is 1. next can be math.MaxInt64+1 for a counter that
// has handed out its last value; the next draw then fails with
// "identity column exhausted".
//
// In concurrent-writes mode the answer comes from the live allocator
// when one is loaded, as for PeekSeq: the stored record is a high
// watermark there, ahead of the next draw by the unhanded cache. That
// state is engine-wide, not this transaction's snapshot, so it can be
// later than the snapshot. A reader that pairs it with a snapshot of
// the rows (.dump) gets a counter at or past every value those rows
// hold, which is the safe direction.
func (t *Txn) PeekIdentity(table, column string) (next uint64, ok bool, err error) {
	desc, err := t.desc(table)
	if err != nil {
		return 0, false, err
	}
	c, err := identityCol(desc, table, column)
	if err != nil {
		return 0, false, err
	}
	// Serialized mode: draws since the last flush live only in the
	// transaction's counter cache, ahead of the stored record.
	if v, ok := t.ids.peek(desc.ID, c.ID); ok {
		return v, true, nil
	}
	key := identitySeqKey(desc.ID, c.ID)
	if t.e.occ {
		if v, ok, handled := t.e.peekCounterOCC(key); handled {
			return v, ok, nil
		}
		// No live allocator: nothing is cached, so the stored value is
		// exactly the next draw.
	}
	raw, ok := t.tx.Get(key)
	if !ok {
		return 0, false, nil
	}
	if len(raw) != 8 {
		return 0, false, serr.New("sequence value is corrupt",
			"sequence", table+"."+column, "len", fmt.Sprint(len(raw)))
	}
	return binary.BigEndian.Uint64(raw), true, nil
}

// RestartIdentity is Txn.RestartIdentity in its own write transaction.
func (e *Engine) RestartIdentity(table, column string, next int64) error {
	return e.WriteTxn(func(tx *Txn) error { return tx.RestartIdentity(table, column, next) })
}

// PeekIdentity is Txn.PeekIdentity in its own read transaction.
func (e *Engine) PeekIdentity(table, column string) (next uint64, ok bool, err error) {
	err = e.ReadTxn(func(tx *Txn) error {
		next, ok, err = tx.PeekIdentity(table, column)
		return err
	})
	return next, ok, err
}
