package bytdb

import (
	"iter"
	"slices"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// Secondary indexes are key ranges, CRDB-style. Two entry forms share
// an index's key space:
//
//	tuple(tableID, indexID, indexed..., pk...) -> ()          non-unique
//	tuple(tableID, indexID, indexed...)        -> tuple(pk...) unique, all indexed values non-NULL
//
// A unique index enforces uniqueness by key collision. Rows with NULL
// in any indexed column take the non-unique form even in a unique
// index, so NULLs never conflict — SQL semantics. Entry arity tells
// the two forms apart on decode.

// IndexCol names one index key column and its direction.
type IndexCol struct {
	Name string
	Desc bool
}

// CreateIndex registers a secondary index over table (all key columns
// ascending) and backfills it from existing rows. The backfill, any
// uniqueness check, and the descriptor update commit atomically:
// either the index exists complete, or not at all.
func (e *Engine) CreateIndex(table, name string, unique bool, cols ...string) (*IndexDesc, error) {
	keys := make([]IndexCol, len(cols))
	for i, c := range cols {
		keys[i] = IndexCol{Name: c}
	}
	return e.CreateIndexCols(table, name, unique, keys)
}

// CreateIndexCols is CreateIndex with per-column directions: a Desc
// key column orders descending in the index's key space.
func (e *Engine) CreateIndexCols(table, name string, unique bool, cols []IndexCol) (*IndexDesc, error) {
	if name == "" {
		return nil, serr.New("index name is required", "table", table)
	}
	if len(cols) == 0 {
		return nil, serr.New("at least one indexed column is required", "table", table, "index", name)
	}
	// The backfill below holds every entry of the new index in memory
	// until commit; refuse before starting it rather than part-way.
	if err := e.CheckMemory(); err != nil {
		return nil, serr.Wrap(err, "op", "create index", "table", table, "index", name)
	}
	var created *IndexDesc
	err := e.alterDesc(table, func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		if old.Index(name) != nil {
			return nil, serr.New("index already exists", "table", table, "index", name)
		}
		idx := IndexDesc{Name: name, Unique: unique, ID: primaryIndexID + 1}
		for _, x := range old.Indexes {
			if x.ID >= idx.ID {
				idx.ID = x.ID + 1
			}
		}
		anyDesc := false
		for _, c := range cols {
			ord := old.ColIndex(c.Name)
			if ord < 0 {
				return nil, serr.New("indexed column not declared", "table", table, "index", name, "column", c.Name)
			}
			if slices.Contains(idx.Cols, ord) {
				return nil, serr.New("duplicate indexed column", "table", table, "index", name, "column", c.Name)
			}
			idx.Cols = append(idx.Cols, ord)
			anyDesc = anyDesc || c.Desc
		}
		if anyDesc {
			idx.Desc = make([]bool, len(cols))
			for i, c := range cols {
				idx.Desc[i] = c.Desc
			}
		}
		desc := old.clone()
		desc.Indexes = append(desc.Indexes, idx)

		// Backfill: collect entries in one stable pass over the rows,
		// then write. The backfill and the descriptor commit atomically —
		// either the index exists complete, or not at all.
		type entry struct {
			key string
			val []byte
		}
		var entries []entry
		prefix := tablePrefix(desc.ID)
		end := string(tuple.PrefixEnd(prefix))
		for k, v := range tx.Ascend(string(prefix)) {
			if k >= end {
				break
			}
			// Decoded with old, not the clone: the rows' layout is the
			// same, and old carries the ID->ordinal table the clone
			// drops, which turned every column of every row into a
			// linear scan of the columns (deep-look D-45).
			row, err := decodeRow(old, k, v)
			if err != nil {
				return nil, err
			}
			ek, ev, _, err := indexEntry(desc, &idx, row.Vals)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry{ek, ev})
		}
		for _, en := range entries {
			if idx.Unique && tx.Contains(en.key) {
				return nil, KindErr(0, "existing rows violate unique index", []error{ErrUniqueViolation}, "table", table, "index", name)
			}
			if err := tx.Set(en.key, en.val); err != nil {
				return nil, err
			}
		}
		created = desc.Index(name)
		return desc, nil
	})
	if err != nil {
		return nil, serr.Wrap(err, "op", "create index", "table", table, "index", name)
	}
	return created, nil
}

// DropIndex removes a secondary index and its entries atomically.
func (e *Engine) DropIndex(table, name string) error {
	err := e.alterDesc(table, func(tx *btypedb.Tx[string, []byte], old *TableDesc) (*TableDesc, error) {
		idx := old.Index(name)
		if idx == nil {
			return nil, serr.New("no such index", "table", table, "index", name)
		}
		// A unique index may be the uniqueness a foreign key depends on;
		// dropping it would leave "the referenced row" undefined. Refuse
		// unless another unique key still covers the referenced columns.
		if idx.Unique {
			refs, err := e.referencingFKs(tx, table, false)
			if err != nil {
				return nil, err
			}
			trimmed := old.clone() // clone() copies Indexes, so DeleteFunc is safe
			trimmed.Indexes = slices.DeleteFunc(trimmed.Indexes,
				func(x IndexDesc) bool { return x.Name == name })
			for _, r := range refs {
				if uniqueKeyCovers(old, r.FK.RefCols) && !uniqueKeyCovers(trimmed, r.FK.RefCols) {
					return nil, serr.New("cannot drop the unique index a foreign key depends on",
						"table", table, "index", name,
						"constraint", r.FK.Name, "referencing_table", r.Child.Name)
				}
			}
		}
		desc := old.clone()
		desc.Indexes = slices.DeleteFunc(desc.Indexes, func(x IndexDesc) bool { return x.Name == name })
		prefix := indexPrefix(desc.ID, idx.ID)
		if _, err := tx.DeleteRange(string(prefix), string(tuple.PrefixEnd(prefix))); err != nil {
			return nil, err
		}
		return desc, nil
	})
	if err != nil {
		return serr.Wrap(err, "op", "drop index", "table", table, "index", name)
	}
	return nil
}

// ScanIndex iterates rows in the named index's order (ties broken by
// primary key), optionally bounded by from <= indexed values < to.
// Bounds may be partial prefixes of the indexed columns; nil is
// unbounded. The scan runs on a consistent snapshot.
func (e *Engine) ScanIndex(table, index string, from, to []any) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		// A snapshot keeps the entry -> row lookup consistent and
		// lock-free while we iterate — and the descriptor resolves from
		// that same snapshot, so it can never describe an index whose
		// backfill the data predates.
		t, err := e.readSnapshot()
		if err != nil {
			yield(Row{}, serr.Wrap(err, "op", "begin index scan"))
			return
		}
		defer t.Rollback()
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
		scanIndexRows(t.tx, desc, idx, from, to, nil)(yield)
	}
}

// ScanIndexRev iterates rows in descending order of the named index —
// ScanIndex read backward. toIncl closes the upper bound: rows whose
// leading indexed columns equal to (the whole prefix group) are
// included (see ScanRangeRev). See ScanIndex on the snapshot caveat.
func (e *Engine) ScanIndexRev(table, index string, from, to []any, toIncl bool) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		t, err := e.readSnapshot()
		if err != nil {
			yield(Row{}, serr.Wrap(err, "op", "begin index scan"))
			return
		}
		defer t.Rollback()
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
		scanIndexRowsRev(t.tx, desc, idx, from, to, toIncl, nil)(yield)
	}
}

// scanIndexRowsRev iterates an index's rows in descending order,
// mirroring scanIndexRows (and scanRowsRev's bound handling, including
// toIncl's prefix-group upper bound).
func scanIndexRowsRev(v kvView, desc *TableDesc, idx *IndexDesc, from, to []any, toIncl bool, need []bool) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		prefix := indexPrefix(desc.ID, idx.ID)
		start := string(prefix)
		end := string(tuple.PrefixEnd(prefix))
		var err error
		if from != nil {
			if start, err = indexBound(desc, idx, prefix, from); err != nil {
				yield(Row{}, err)
				return
			}
		}
		if to != nil {
			if end, err = indexBound(desc, idx, prefix, to); err != nil {
				yield(Row{}, err)
				return
			}
			if toIncl {
				end = string(tuple.PrefixEnd([]byte(end)))
			}
		}
		// See scanRows on why the full claim is marked (serializable).
		markReadRange(v, start, end)
		for k, val := range v.Descend(end) {
			if k >= end {
				continue
			}
			if k < start {
				return
			}
			row, err := rowFromIndexEntry(v, desc, idx, k, val, need)
			if !yield(row, err) || err != nil {
				return
			}
		}
	}
}

// scanIndexRows iterates an index's rows from any kv view. The view
// serves both the entry scan and the entry -> row lookups, so it must
// be a snapshot or transaction (never the bare DB, whose iteration
// holds a read lock that the inner lookups would re-enter).
func scanIndexRows(v kvView, desc *TableDesc, idx *IndexDesc, from, to []any, need []bool) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		prefix := indexPrefix(desc.ID, idx.ID)
		start := string(prefix)
		end := string(tuple.PrefixEnd(prefix))
		var err error
		if from != nil {
			if start, err = indexBound(desc, idx, prefix, from); err != nil {
				yield(Row{}, err)
				return
			}
		}
		if to != nil {
			if end, err = indexBound(desc, idx, prefix, to); err != nil {
				yield(Row{}, err)
				return
			}
		}
		// See scanRows on why the full claim is marked (serializable).
		markReadRange(v, start, end)
		for k, val := range v.Ascend(start) {
			if k >= end {
				return
			}
			row, err := rowFromIndexEntry(v, desc, idx, k, val, need)
			if !yield(row, err) || err != nil {
				return
			}
		}
	}
}

// appendDirected appends vals to buf, each with its key column's
// direction.
func appendDirected(buf []byte, idx *IndexDesc, vals []any) ([]byte, error) {
	var err error
	for i, v := range vals {
		if idx.DescAt(i) {
			buf, err = tuple.AppendDesc(buf, v)
		} else {
			buf, err = tuple.Append(buf, v)
		}
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// indexEntry builds one row's entry for one index from a coerced row.
// enforced reports whether the entry participates in uniqueness (and
// so must be collision-checked before writing).
//
// Columns are encoded straight from the row into one pre-sized buffer.
// The entry used to gather the indexed values and the primary key into
// slices of their own and append to a prefix with no spare capacity —
// several allocations per index per written row (deep-look D-45).
func indexEntry(desc *TableDesc, idx *IndexDesc, row []any) (key string, val []byte, enforced bool, err error) {
	hasNull := false
	buf := appendIndexPrefix(make([]byte, 0, keyBufCap), desc.ID, idx.ID)
	for i, ord := range idx.Cols {
		v := row[ord]
		if v == nil {
			hasNull = true
		}
		if idx.DescAt(i) {
			buf, err = tuple.AppendDesc(buf, v)
		} else {
			buf, err = tuple.Append(buf, v)
		}
		if err != nil {
			return "", nil, false, serr.Wrap(err, "op", "encode index entry", "index", idx.Name)
		}
	}
	if idx.Unique && !hasNull {
		// The key is the indexed values alone; the row's primary key is
		// the entry's value.
		var pkVal []byte
		for _, ord := range desc.PKCols {
			if pkVal, err = tuple.Append(pkVal, row[ord]); err != nil {
				return "", nil, false, serr.Wrap(err, "op", "encode index entry pk", "index", idx.Name)
			}
		}
		return string(buf), pkVal, true, nil
	}
	for _, ord := range desc.PKCols {
		if buf, err = tuple.Append(buf, row[ord]); err != nil {
			return "", nil, false, serr.Wrap(err, "op", "encode index entry pk", "index", idx.Name)
		}
	}
	return string(buf), []byte{}, false, nil
}

// indexBound encodes a possibly-partial tuple of indexed values as a
// scan bound.
func indexBound(desc *TableDesc, idx *IndexDesc, prefix []byte, vals []any) (string, error) {
	if len(vals) > len(idx.Cols) {
		return "", serr.New("too many values in index scan bound", "table", desc.Name, "index", idx.Name)
	}
	coerced := make([]any, len(vals))
	for i, v := range vals {
		c := desc.Columns[idx.Cols[i]]
		cv, err := coerce(v, c.Type)
		if err != nil {
			return "", serr.Wrap(err, "table", desc.Name, "column", c.Name)
		}
		coerced[i] = cv
	}
	buf, err := appendDirected(prefix, idx, coerced)
	if err != nil {
		return "", serr.Wrap(err, "op", "encode index scan bound")
	}
	return string(buf), nil
}

// rowFromIndexEntry resolves an index entry to its full row. The entry
// form is recognized by arity: pk-suffixed keys carry the primary key
// themselves; unique-form keys carry it in the value. Indexed values
// decode each with its key column's direction; the pk suffix is always
// ascending.
func rowFromIndexEntry(v kvView, desc *TableDesc, idx *IndexDesc, key string, val []byte, need []bool) (Row, error) {
	data := []byte(key)
	var err error
	// Skip the (tableID, indexID) header and the indexed values.
	for i := 0; i < 2+len(idx.Cols); i++ {
		if _, data, err = tuple.DecodeOne(data, i >= 2 && idx.DescAt(i-2)); err != nil {
			return Row{}, serr.Wrap(err, "op", "decode index entry", "index", idx.Name)
		}
	}
	var pk []any
	if len(data) > 0 { // pk in the key
		if pk, err = tuple.Decode(data); err != nil {
			return Row{}, serr.Wrap(err, "op", "decode index entry", "index", idx.Name)
		}
	} else { // unique form: pk in the value
		if pk, err = tuple.Decode(val); err != nil {
			return Row{}, serr.Wrap(err, "op", "decode index entry value", "index", idx.Name)
		}
	}
	if len(pk) != len(desc.PKCols) {
		return Row{}, serr.New("index entry primary key has wrong arity", "table", desc.Name, "index", idx.Name)
	}
	rk, err := rowKey(desc, pk)
	if err != nil {
		return Row{}, err
	}
	// The scan's range claim covers only the index entries; the row
	// fetched through one is a separate read — an update to a
	// non-indexed column rewrites the row key without ever entering
	// the index range, and must still conflict (serializable mode).
	markRead(v, rk)
	rv, ok := v.Get(rk)
	if !ok {
		return Row{}, serr.New("index entry points at a missing row", "table", desc.Name, "index", idx.Name)
	}
	return decodeRowCols(desc, rk, rv, need)
}
