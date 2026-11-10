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
// passes through untouched for coerceRow to report. Draws and bumps go
// through tx in serialized mode and through e's allocators in
// concurrent-writes mode — the mode split documented in the file
// header.
//
// drawn exists for error classification: a uniqueness collision on a
// value the ENGINE chose (as opposed to one the caller supplied) can
// only mean the draw raced a concurrent counter reset or bump — under
// concurrent writes, insertRow reports that as a retryable conflict
// rather than a data error.
func fillIdentity(e *Engine, tx *btypedb.Tx[string, []byte], desc *TableDesc, vals []any) (out []any, drawn []int, err error) {
	if !desc.hasIdentity() || len(vals) != len(desc.Columns) {
		return vals, nil, nil
	}
	out = slices.Clone(vals)
	for i := range desc.Columns {
		c := &desc.Columns[i]
		if !c.Identity {
			continue
		}
		key := identitySeqKey(desc.ID, c.ID)
		what := desc.Name + "." + c.Name
		if out[i] == nil {
			var v uint64
			var err error
			if e.occ {
				v, err = e.allocCounter(key, 1, what, identityOwner(e, desc))
			} else {
				v, err = nextFromCounter(tx, key, 1, what)
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
			if e.occ {
				err = e.bumpCounterAllocTo(key, uint64(n)+1, what, identityOwner(e, desc))
			} else {
				err = bumpCounterTo(tx, key, uint64(n)+1, what)
			}
			if err != nil {
				return nil, nil, err
			}
		}
	}
	return out, drawn, nil
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
// (rolled back with the update), through the non-transactional
// allocator under concurrent writes (they stand, at worst a gap).
// Negative values never collide with draws, which start at 1.
func bumpIdentityOnUpdate(e *Engine, tx *btypedb.Tx[string, []byte], desc *TableDesc, set map[string]any, newVals []any) error {
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
		c := &desc.Columns[ord]
		key := identitySeqKey(desc.ID, c.ID)
		what := desc.Name + "." + c.Name
		var err error
		if e.occ {
			err = e.bumpCounterAllocTo(key, uint64(n)+1, what, identityOwner(e, desc))
		} else {
			err = bumpCounterTo(tx, key, uint64(n)+1, what)
		}
		if err != nil {
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
