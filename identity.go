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
// value never touches the counter). Values drawn inside a rolled-back
// transaction are returned to the counter.

func (d *TableDesc) hasIdentity() bool {
	for i := range d.Columns {
		if d.Columns[i].Identity {
			return true
		}
	}
	return false
}

// fillIdentity resolves a row's identity columns in tx, returning the
// resolved values (the input is not mutated). A row with the wrong
// arity passes through untouched for coerceRow to report.
func fillIdentity(tx *btypedb.Tx[string, []byte], desc *TableDesc, vals []any) ([]any, error) {
	if !desc.hasIdentity() || len(vals) != len(desc.Columns) {
		return vals, nil
	}
	out := slices.Clone(vals)
	for i := range desc.Columns {
		c := &desc.Columns[i]
		if !c.Identity {
			continue
		}
		key := identitySeqKey(desc.ID, c.ID)
		what := desc.Name + "." + c.Name
		if out[i] == nil {
			v, err := nextFromCounter(tx, key, 1, what)
			if err != nil {
				return nil, err
			}
			// The counter is unsigned but the column is int64.
			if v > math.MaxInt64 {
				return nil, serr.New("identity column exhausted",
					"table", desc.Name, "column", c.Name)
			}
			out[i] = int64(v)
			continue
		}
		cv, err := coerce(out[i], TInt)
		if err != nil {
			return nil, serr.Wrap(err, "table", desc.Name, "column", c.Name)
		}
		if n := cv.(int64); n >= 0 {
			if err := bumpCounterTo(tx, key, uint64(n)+1, what); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
