package bytdb

import (
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
// value never touches the counter).
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
				v, err = e.allocCounter(key, 1, what)
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
				err = e.bumpCounterAllocTo(key, uint64(n)+1, what)
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
