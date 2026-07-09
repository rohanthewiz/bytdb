package bytdb

import (
	"bytes"
	"fmt"
	"iter"
	"slices"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// Row is one decoded table row: values in declared column order.
type Row struct {
	Desc *TableDesc
	Vals []any
}

// Col returns the value of the named column, or nil if no such column.
func (r Row) Col(name string) any {
	if i := r.Desc.ColIndex(name); i >= 0 {
		return r.Vals[i]
	}
	return nil
}

// Insert stores one row, vals in declared column order. Values are
// coerced to their column types (any Go int width into an int column,
// ints into float columns); nil is allowed in non-key columns. The
// primary key must not already exist. The row and its entry in every
// secondary index commit atomically.
//
// The descriptor is resolved inside the write transaction, so an
// insert serialized after a CreateIndex maintains the new index.
//
// An error does not guarantee the row was discarded: the kv store
// makes a commit visible to new snapshots before its WAL append and
// fsync, so a failure there can leave the row readable until the
// process restarts (replay then drops it). Treat an error as
// "durability unknown," not "definitely not applied."
func (e *Engine) Insert(table string, vals ...any) error {
	if err := e.checkReentrantWrite("insert"); err != nil {
		return err
	}
	err := e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		desc, err := e.descFromView(tx, table)
		if err != nil {
			return err
		}
		_, err = insertRow(tx, desc, vals)
		return err
	})
	if err != nil {
		return serr.Wrap(err, "op", "insert", "table", table)
	}
	return nil
}

// InsertReturning is Insert returning the row as stored: identity
// columns filled and values coerced to their column types. It is how
// an embedded caller learns the ID an identity column drew without
// opening a transaction.
func (e *Engine) InsertReturning(table string, vals ...any) (Row, error) {
	if err := e.checkReentrantWrite("insert"); err != nil {
		return Row{}, err
	}
	var row Row
	err := e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		desc, err := e.descFromView(tx, table)
		if err != nil {
			return err
		}
		stored, err := insertRow(tx, desc, vals)
		if err != nil {
			return err
		}
		row = Row{Desc: desc, Vals: stored}
		return nil
	})
	if err != nil {
		return Row{}, serr.Wrap(err, "op", "insert", "table", table)
	}
	return row, nil
}

// insertRow stages one row plus its index entries in tx, returning the
// row as stored — coerced, with identity columns filled — so callers
// that must report the final values (RETURNING) get engine truth
// rather than re-deriving it. Checks run before any write, so a failed
// insert leaves the transaction clean — except identity counter draws
// and bumps, which stay: they are harmless (at worst a gap) and roll
// back with the transaction anyway if it aborts.
func insertRow(tx *btypedb.Tx[string, []byte], desc *TableDesc, vals []any) ([]any, error) {
	vals, err := fillIdentity(tx, desc, vals)
	if err != nil {
		return nil, err
	}
	row, err := coerceRow(desc, vals)
	if err != nil {
		return nil, err
	}
	key, err := rowKey(desc, pkValues(desc, row))
	if err != nil {
		return nil, err
	}
	val, err := encodeRowValue(desc, row)
	if err != nil {
		return nil, err
	}
	if tx.Contains(key) {
		return nil, serr.New("duplicate primary key", "table", desc.Name)
	}
	type entry struct {
		key string
		val []byte
	}
	entries := make([]entry, len(desc.Indexes))
	for i := range desc.Indexes {
		ek, ev, enforced, err := indexEntry(desc, &desc.Indexes[i], row)
		if err != nil {
			return nil, err
		}
		if enforced && tx.Contains(ek) {
			return nil, serr.New("unique index violation", "table", desc.Name, "index", desc.Indexes[i].Name)
		}
		entries[i] = entry{ek, ev}
	}
	for _, en := range entries {
		if err := tx.Set(en.key, en.val); err != nil {
			return nil, err
		}
	}
	if err := tx.Set(key, val); err != nil {
		return nil, err
	}
	return row, nil
}

// Update modifies the row with the given primary-key values, setting
// the named columns, and reports whether the row existed. Changing a
// key column moves the row; every affected index entry moves with it,
// with uniqueness re-checked. All checks run before any write.
func (e *Engine) Update(table string, pkVals []any, set map[string]any) (bool, error) {
	if err := e.checkReentrantWrite("update"); err != nil {
		return false, err
	}
	updated := false
	err := e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		desc, err := e.descFromView(tx, table)
		if err != nil {
			return err
		}
		_, updated, err = updateRow(tx, desc, pkVals, set)
		return err
	})
	if err != nil {
		return false, serr.Wrap(err, "op", "update", "table", table)
	}
	return updated, nil
}

// updateRow stages an in-place or key-moving row update in tx,
// returning the row as stored — every SET value coerced onto the old
// row — so callers reporting the final values (RETURNING) get engine
// truth. Phase 1 computes and validates everything; phase 2 writes —
// so any error leaves the transaction unmutated. A missing row is
// (nil, false, nil), not an error.
func updateRow(tx *btypedb.Tx[string, []byte], desc *TableDesc, pkVals []any, set map[string]any) ([]any, bool, error) {
	oldKey, err := fullPKKey(desc, pkVals)
	if err != nil {
		return nil, false, err
	}
	oldVal, ok := tx.Get(oldKey)
	if !ok {
		return nil, false, nil
	}
	oldRow, err := decodeRow(desc, oldKey, oldVal)
	if err != nil {
		return nil, false, err
	}

	newVals := slices.Clone(oldRow.Vals)
	for col, v := range set {
		ord := desc.ColIndex(col)
		if ord < 0 {
			return nil, false, serr.New("no such column", "table", desc.Name, "column", col)
		}
		cv, err := coerce(v, desc.Columns[ord].Type)
		if err != nil {
			return nil, false, serr.Wrap(err, "table", desc.Name, "column", col)
		}
		if cv == nil {
			if desc.isPK(ord) {
				return nil, false, serr.New("primary key column may not be NULL", "table", desc.Name, "column", col)
			}
			// Identity implies NOT NULL.
			if desc.Columns[ord].NotNull || desc.Columns[ord].Identity {
				return nil, false, notNullErr(desc, col)
			}
		}
		newVals[ord] = cv
	}
	newKey, err := rowKey(desc, pkValues(desc, newVals))
	if err != nil {
		return nil, false, err
	}
	newVal, err := encodeRowValue(desc, newVals)
	if err != nil {
		return nil, false, err
	}
	if newKey != oldKey && tx.Contains(newKey) {
		return nil, false, serr.New("duplicate primary key", "table", desc.Name)
	}
	// Index entries that change: compute and uniqueness-check them all
	// before writing anything. An occupant of a changed unique key must
	// be another row — this row's own entry has the unchanged key.
	type move struct {
		oldKey, newKey string
		newVal         []byte
	}
	var moves []move
	for i := range desc.Indexes {
		idx := &desc.Indexes[i]
		ok, ov, _, err := indexEntry(desc, idx, oldRow.Vals)
		if err != nil {
			return nil, false, err
		}
		nk, nv, enforced, err := indexEntry(desc, idx, newVals)
		if err != nil {
			return nil, false, err
		}
		if ok == nk && bytes.Equal(ov, nv) {
			continue
		}
		if enforced && nk != ok && tx.Contains(nk) {
			return nil, false, serr.New("unique index violation", "table", desc.Name, "index", idx.Name)
		}
		moves = append(moves, move{ok, nk, nv})
	}

	for _, m := range moves {
		if _, err := tx.Delete(m.oldKey); err != nil {
			return nil, false, err
		}
		if err := tx.Set(m.newKey, m.newVal); err != nil {
			return nil, false, err
		}
	}
	if newKey != oldKey {
		if _, err := tx.Delete(oldKey); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Set(newKey, newVal); err != nil {
		return nil, false, err
	}
	return newVals, true, nil
}

// Get returns the row with the given primary-key values. Descriptor
// and row come from one snapshot, so the decode always matches the
// schema the row was stored under (as of that snapshot).
func (e *Engine) Get(table string, pkVals ...any) (Row, bool, error) {
	var row Row
	found := false
	err := e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		desc, err := e.descFromView(tx, table)
		if err != nil {
			return err
		}
		key, err := fullPKKey(desc, pkVals)
		if err != nil {
			return err
		}
		val, ok := tx.Get(key)
		if !ok {
			return nil
		}
		row, err = decodeRow(desc, key, val)
		found = err == nil
		return err
	})
	return row, found, err
}

// Delete removes the row with the given primary-key values — and its
// secondary-index entries, atomically — reporting whether it existed.
func (e *Engine) Delete(table string, pkVals ...any) (bool, error) {
	if err := e.checkReentrantWrite("delete"); err != nil {
		return false, err
	}
	existed := false
	err := e.kv.Update(func(tx *btypedb.Tx[string, []byte]) error {
		desc, err := e.descFromView(tx, table)
		if err != nil {
			return err
		}
		existed, err = deleteRow(tx, desc, pkVals)
		return err
	})
	if err != nil {
		return false, serr.Wrap(err, "op", "delete", "table", table)
	}
	return existed, nil
}

// deleteRow stages the removal of one row and its index entries in tx.
func deleteRow(tx *btypedb.Tx[string, []byte], desc *TableDesc, pkVals []any) (bool, error) {
	key, err := fullPKKey(desc, pkVals)
	if err != nil {
		return false, err
	}
	val, ok := tx.Get(key)
	if !ok {
		return false, nil
	}
	if len(desc.Indexes) > 0 {
		row, err := decodeRow(desc, key, val)
		if err != nil {
			return false, err
		}
		for i := range desc.Indexes {
			ek, _, _, err := indexEntry(desc, &desc.Indexes[i], row.Vals)
			if err != nil {
				return false, err
			}
			if _, err := tx.Delete(ek); err != nil {
				return false, err
			}
		}
	}
	return tx.Delete(key)
}

// Scan iterates every row of the table in primary-key order. A decode
// failure is yielded once as the error and ends the sequence.
func (e *Engine) Scan(table string) iter.Seq2[Row, error] {
	return e.ScanRange(table, nil, nil)
}

// ScanRange iterates rows with fromPK <= pk < toPK in primary-key
// order. Bounds may be partial prefixes of a composite key (e.g. just
// the first key column); a nil bound is unbounded on that side. The
// scan runs on a consistent snapshot, descriptor included.
func (e *Engine) ScanRange(table string, fromPK, toPK []any) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		t, err := e.readSnapshot()
		if err != nil {
			yield(Row{}, serr.Wrap(err, "op", "begin scan"))
			return
		}
		defer t.Rollback()
		desc, err := t.desc(table)
		if err != nil {
			yield(Row{}, err)
			return
		}
		scanRows(t.tx, desc, fromPK, toPK)(yield)
	}
}

// scanRows iterates a table's rows from any kv view.
func scanRows(v kvView, desc *TableDesc, fromPK, toPK []any) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		prefix := tablePrefix(desc.ID)
		start := string(prefix)
		end := string(tuple.PrefixEnd(prefix))
		var err error
		if fromPK != nil {
			if start, err = boundKey(desc, prefix, fromPK); err != nil {
				yield(Row{}, err)
				return
			}
		}
		if toPK != nil {
			if end, err = boundKey(desc, prefix, toPK); err != nil {
				yield(Row{}, err)
				return
			}
		}
		for k, val := range v.Ascend(start) {
			if k >= end {
				return
			}
			row, err := decodeRow(desc, k, val)
			if !yield(row, err) || err != nil {
				return
			}
		}
	}
}

// ScanRangeRev iterates rows with fromPK <= pk < toPK in descending
// primary-key order — ScanRange read backward. toIncl closes the upper
// bound instead: rows whose leading key columns equal toPK (the whole
// toPK prefix group) are included — a region a partial-prefix exclusive
// bound cannot express, and the natural upper edge when reversing a
// scan pinned by column equality.
func (e *Engine) ScanRangeRev(table string, fromPK, toPK []any, toIncl bool) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		t, err := e.readSnapshot()
		if err != nil {
			yield(Row{}, serr.Wrap(err, "op", "begin scan"))
			return
		}
		defer t.Rollback()
		desc, err := t.desc(table)
		if err != nil {
			yield(Row{}, err)
			return
		}
		scanRowsRev(t.tx, desc, fromPK, toPK, toIncl)(yield)
	}
}

// scanRowsRev iterates a table's rows in descending primary-key order,
// mirroring scanRows. Descend starts at the last key <= the exclusive
// upper bound: the first yield lands at or past it, so keys >= end are
// skipped, and the walk stops once it drops below the inclusive lower
// bound. toIncl widens the upper bound to the end of toPK's prefix
// group (every key with that prefix sorts below its PrefixEnd).
func scanRowsRev(v kvView, desc *TableDesc, fromPK, toPK []any, toIncl bool) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		prefix := tablePrefix(desc.ID)
		start := string(prefix)
		end := string(tuple.PrefixEnd(prefix))
		var err error
		if fromPK != nil {
			if start, err = boundKey(desc, prefix, fromPK); err != nil {
				yield(Row{}, err)
				return
			}
		}
		if toPK != nil {
			if end, err = boundKey(desc, prefix, toPK); err != nil {
				yield(Row{}, err)
				return
			}
			if toIncl {
				end = string(tuple.PrefixEnd([]byte(end)))
			}
		}
		for k, val := range v.Descend(end) {
			if k >= end {
				continue
			}
			if k < start {
				return
			}
			row, err := decodeRow(desc, k, val)
			if !yield(row, err) || err != nil {
				return
			}
		}
	}
}

// --- row/key plumbing ---

// coerceRow validates arity and coerces every value to its column
// type, rejecting nil in primary-key and NOT NULL columns.
func coerceRow(desc *TableDesc, vals []any) ([]any, error) {
	if len(vals) != len(desc.Columns) {
		return nil, serr.New("wrong number of values",
			"table", desc.Name, "want", fmt.Sprint(len(desc.Columns)), "got", fmt.Sprint(len(vals)))
	}
	out := make([]any, len(vals))
	for i, v := range vals {
		c := desc.Columns[i]
		cv, err := coerce(v, c.Type)
		if err != nil {
			return nil, serr.Wrap(err, "table", desc.Name, "column", c.Name)
		}
		if cv == nil {
			if desc.isPK(i) {
				return nil, serr.New("primary key column may not be NULL",
					"table", desc.Name, "column", c.Name)
			}
			// Identity implies NOT NULL (insert never gets here with one:
			// fillIdentity already drew the value).
			if c.NotNull || c.Identity {
				return nil, notNullErr(desc, c.Name)
			}
		}
		out[i] = cv
	}
	return out, nil
}

// notNullErr is the NOT NULL violation, worded as Postgres words it.
func notNullErr(desc *TableDesc, col string) error {
	return serr.New(`null value in column "` + col + `" of relation "` +
		desc.Name + `" violates not-null constraint`)
}

// coerce maps v onto the canonical Go type for a column type.
func coerce(v any, t ColType) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch t {
	case TBool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
	case TInt:
		switch n := v.(type) {
		case int:
			return int64(n), nil
		case int8:
			return int64(n), nil
		case int16:
			return int64(n), nil
		case int32:
			return int64(n), nil
		case int64:
			return n, nil
		case uint8:
			return int64(n), nil
		case uint16:
			return int64(n), nil
		case uint32:
			return int64(n), nil
		}
	case TFloat:
		switch n := v.(type) {
		case float64:
			return n, nil
		case float32:
			return float64(n), nil
		case int:
			return float64(n), nil
		case int64:
			return float64(n), nil
		}
	case TString:
		if s, ok := v.(string); ok {
			return s, nil
		}
	case TBytes:
		if b, ok := v.([]byte); ok {
			return b, nil
		}
	}
	return nil, serr.New("value does not fit column type",
		"type", string(t), "value_type", fmt.Sprintf("%T", v))
}

// pkValues extracts the key columns from a full coerced row, in key
// order.
func pkValues(desc *TableDesc, row []any) []any {
	pk := make([]any, len(desc.PKCols))
	for i, ord := range desc.PKCols {
		pk[i] = row[ord]
	}
	return pk
}

// fullPKKey coerces user-supplied PK values and builds the row key.
// All key columns are required (unlike scan bounds, which may be
// partial).
func fullPKKey(desc *TableDesc, pkVals []any) (string, error) {
	if len(pkVals) != len(desc.PKCols) {
		return "", serr.New("wrong number of primary key values",
			"table", desc.Name, "want", fmt.Sprint(len(desc.PKCols)), "got", fmt.Sprint(len(pkVals)))
	}
	coerced, err := coercePK(desc, pkVals)
	if err != nil {
		return "", err
	}
	return rowKey(desc, coerced)
}

// boundKey encodes a possibly-partial PK tuple as a scan bound.
func boundKey(desc *TableDesc, prefix []byte, pkVals []any) (string, error) {
	if len(pkVals) > len(desc.PKCols) {
		return "", serr.New("too many primary key values in scan bound", "table", desc.Name)
	}
	coerced, err := coercePK(desc, pkVals)
	if err != nil {
		return "", err
	}
	buf, err := tuple.Append(prefix, coerced...)
	if err != nil {
		return "", serr.Wrap(err, "op", "encode scan bound")
	}
	return string(buf), nil
}

func coercePK(desc *TableDesc, pkVals []any) ([]any, error) {
	out := make([]any, len(pkVals))
	for i, v := range pkVals {
		c := desc.Columns[desc.PKCols[i]]
		cv, err := coerce(v, c.Type)
		if err != nil {
			return nil, serr.Wrap(err, "table", desc.Name, "column", c.Name)
		}
		if cv == nil {
			return nil, serr.New("primary key value may not be NULL",
				"table", desc.Name, "column", c.Name)
		}
		out[i] = cv
	}
	return out, nil
}

// encodeRowValue encodes the non-key columns as the row's stored
// value: a sparse sequence of (column ID, value) pairs, NULL columns
// omitted. Key columns live in the key alone. Tagging by stable ID —
// not position — is what lets AddColumn and DropColumn leave rows
// untouched.
func encodeRowValue(desc *TableDesc, row []any) ([]byte, error) {
	buf := []byte{}
	var err error
	for i := range desc.Columns {
		if desc.isPK(i) || row[i] == nil {
			continue
		}
		if buf, err = tuple.Append(buf, int64(desc.Columns[i].ID), row[i]); err != nil {
			return nil, serr.Wrap(err, "op", "encode row value", "column", desc.Columns[i].Name)
		}
	}
	return buf, nil
}

// decodeRow reassembles a full row from its key and value: key columns
// from the key tuple, the rest from the value's (column ID, value)
// pairs. Columns absent from the value read as NULL (never written, or
// added after the row was); pairs with unknown IDs are skipped (their
// column was dropped).
func decodeRow(desc *TableDesc, key string, val []byte) (Row, error) {
	keyVals, err := tuple.Decode([]byte(key))
	if err != nil {
		return Row{}, serr.Wrap(err, "op", "decode row key", "table", desc.Name)
	}
	if len(keyVals) != 2+len(desc.PKCols) {
		return Row{}, serr.New("row key has wrong arity", "table", desc.Name)
	}
	pairs, err := tuple.Decode(val)
	if err != nil {
		return Row{}, serr.Wrap(err, "op", "decode row value", "table", desc.Name)
	}
	if len(pairs)%2 != 0 {
		return Row{}, serr.New("row value has dangling column tag", "table", desc.Name)
	}
	vals := make([]any, len(desc.Columns))
	for i, ord := range desc.PKCols {
		vals[ord] = keyVals[2+i]
	}
	for j := 0; j < len(pairs); j += 2 {
		id, ok := pairs[j].(int64)
		if !ok {
			return Row{}, serr.New("row value column tag is not an ID", "table", desc.Name)
		}
		if ord := desc.colOrdinalByID(uint32(id)); ord >= 0 {
			vals[ord] = pairs[j+1]
		}
	}
	return Row{Desc: desc, Vals: vals}, nil
}
