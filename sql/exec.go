package sql

import (
	"bytes"
	"cmp"
	"iter"
	"slices"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// scanPlan yields the rows a plan matches, in the chosen path's key
// order, within tx's snapshot. yield returning false stops the scan.
func scanPlan(tx *bytdb.Txn, table string, p *plan, yield func(bytdb.Row) bool) error {
	if p.get != nil {
		row, ok, err := tx.Get(table, p.get...)
		if err != nil {
			return err
		}
		if ok && matches(p.preds, row) {
			yield(row)
		}
		return nil
	}
	var seq iter.Seq2[bytdb.Row, error]
	if p.index == "" {
		seq = tx.ScanRange(table, p.from, nil)
	} else {
		seq = tx.ScanIndex(table, p.index, p.from, nil)
	}
	for row, err := range seq {
		if err != nil {
			return err
		}
		if stopped(p.stops, row) {
			return nil
		}
		if !matches(p.preds, row) {
			continue
		}
		if !yield(row) {
			return nil
		}
	}
	return nil
}

// stopped reports whether the scan has left the pushed-down region.
// Checks run in key-column order, so a range check on column k is only
// reached while the equality prefix before it still holds.
func stopped(stops []stop, row bytdb.Row) bool {
	for _, st := range stops {
		c, ok := compareVals(row.Vals[st.ord], st.val)
		if !ok {
			if st.kind == stopNE {
				return true // the equality region cannot contain NULL
			}
			continue // a range column's NULL group sorts before its values
		}
		switch st.kind {
		case stopNE:
			if c != 0 {
				return true
			}
		case stopGE:
			if c >= 0 {
				return true
			}
		case stopGT:
			if c > 0 {
				return true
			}
		}
	}
	return false
}

// matches evaluates every predicate against a row. A comparison
// against NULL, or between incomparable kinds, is false — so rows with
// NULL never match anything but IS NULL, per SQL.
func matches(preds []Pred, row bytdb.Row) bool {
	for _, pr := range preds {
		if !checkPred(row.Col(pr.Col), pr.Op, pr.Val) {
			return false
		}
	}
	return true
}

// compareVals orders two non-NULL values. ok is false for NULLs and
// for incomparable kinds; int64 and float64 compare numerically.
func compareVals(a, b any) (int, bool) {
	if a == nil || b == nil {
		return 0, false
	}
	switch av := a.(type) {
	case int64:
		switch bv := b.(type) {
		case int64:
			return cmp.Compare(av, bv), true
		case float64:
			return cmp.Compare(float64(av), bv), true
		}
	case float64:
		switch bv := b.(type) {
		case float64:
			return cmp.Compare(av, bv), true
		case int64:
			return cmp.Compare(av, float64(bv)), true
		}
	case string:
		if bv, ok := b.(string); ok {
			return cmp.Compare(av, bv), true
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return cmp.Compare(btoi(av), btoi(bv)), true
		}
	case []byte:
		if bv, ok := b.([]byte); ok {
			return bytes.Compare(av, bv), true
		}
	}
	return 0, false
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// --- statement executors ---

func (d *DB) execSelect(s *Select) (*Result, error) {
	res := &Result{}
	var rows [][]any // full rows; projected after sort/limit
	var ords []int   // projected column ordinals
	var keys []sortKey
	err := d.e.ReadTxn(func(tx *bytdb.Txn) error {
		desc := tx.Table(s.Table)
		if desc == nil {
			return serr.New("no such table", "table", s.Table)
		}
		pl, err := planScan(desc, s.Where)
		if err != nil {
			return err
		}
		if s.Star {
			for i, c := range desc.Columns {
				ords = append(ords, i)
				res.Cols = append(res.Cols, c.Name)
				res.Types = append(res.Types, c.Type)
			}
		} else {
			for _, it := range s.Items {
				ord := desc.ColIndex(it.Col)
				if ord < 0 {
					return serr.New("no such column", "table", s.Table, "column", it.Col)
				}
				ords = append(ords, ord)
				res.Cols = append(res.Cols, desc.Columns[ord].Name)
				res.Types = append(res.Types, desc.Columns[ord].Type)
			}
		}
		for _, o := range s.OrderBy {
			ord := desc.ColIndex(o.Col)
			if ord < 0 {
				return serr.New("no such column", "table", s.Table, "column", o.Col)
			}
			keys = append(keys, sortKey{ord, o.Desc})
		}
		// With no ORDER BY the scan order is the result order, so
		// collection can end at OFFSET+LIMIT rows.
		return scanPlan(tx, s.Table, pl, func(r bytdb.Row) bool {
			rows = append(rows, r.Vals)
			return len(keys) > 0 || s.Limit < 0 || int64(len(rows)) < s.Offset+s.Limit
		})
	})
	if err != nil {
		return nil, err
	}
	if len(keys) > 0 {
		slices.SortStableFunc(rows, func(a, b []any) int {
			for _, k := range keys {
				c := orderCmp(a[k.ord], b[k.ord])
				if k.desc {
					c = -c
				}
				if c != 0 {
					return c
				}
			}
			return 0
		})
	}
	if s.Offset > 0 {
		if s.Offset >= int64(len(rows)) {
			rows = nil
		} else {
			rows = rows[s.Offset:]
		}
	}
	if s.Limit >= 0 && int64(len(rows)) > s.Limit {
		rows = rows[:s.Limit]
	}
	res.Rows = make([][]any, len(rows))
	for i, r := range rows {
		out := make([]any, len(ords))
		for j, ord := range ords {
			out[j] = r[ord]
		}
		res.Rows[i] = out
	}
	return res, nil
}

type sortKey struct {
	ord  int
	desc bool
}

// orderCmp is compareVals for sorting: NULLs order last ascending
// (and so first descending), per Postgres defaults.
func orderCmp(a, b any) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	c, _ := compareVals(a, b)
	return c
}

func (d *DB) execInsert(s *Insert) (*Result, error) {
	err := d.e.WriteTxn(func(tx *bytdb.Txn) error {
		desc := tx.Table(s.Table)
		if desc == nil {
			return serr.New("no such table", "table", s.Table)
		}
		// With a column list, unnamed columns insert as NULL.
		var ords []int
		if s.Cols != nil {
			for _, name := range s.Cols {
				ord := desc.ColIndex(name)
				if ord < 0 {
					return serr.New("no such column", "table", s.Table, "column", name)
				}
				ords = append(ords, ord)
			}
		}
		for _, row := range s.Rows {
			vals := row
			if ords != nil {
				if len(row) != len(ords) {
					return serr.New("INSERT row has wrong number of values", "table", s.Table)
				}
				vals = make([]any, len(desc.Columns))
				for i, ord := range ords {
					vals[ord] = row[i]
				}
			}
			if err := tx.Insert(s.Table, vals...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Result{RowsAffected: len(s.Rows)}, nil
}

func (d *DB) execUpdate(s *Update) (*Result, error) {
	affected := 0
	err := d.e.WriteTxn(func(tx *bytdb.Txn) error {
		desc := tx.Table(s.Table)
		if desc == nil {
			return serr.New("no such table", "table", s.Table)
		}
		pl, err := planScan(desc, s.Where)
		if err != nil {
			return err
		}
		// Materialize matching keys before writing: updates move rows
		// and index entries under a live scan.
		pks, err := collectPKs(tx, s.Table, desc, pl)
		if err != nil {
			return err
		}
		for _, pk := range pks {
			ok, err := tx.Update(s.Table, pk, s.Set)
			if err != nil {
				return err
			}
			if ok {
				affected++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Result{RowsAffected: affected}, nil
}

func (d *DB) execDelete(s *Delete) (*Result, error) {
	affected := 0
	err := d.e.WriteTxn(func(tx *bytdb.Txn) error {
		desc := tx.Table(s.Table)
		if desc == nil {
			return serr.New("no such table", "table", s.Table)
		}
		pl, err := planScan(desc, s.Where)
		if err != nil {
			return err
		}
		pks, err := collectPKs(tx, s.Table, desc, pl)
		if err != nil {
			return err
		}
		for _, pk := range pks {
			ok, err := tx.Delete(s.Table, pk...)
			if err != nil {
				return err
			}
			if ok {
				affected++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Result{RowsAffected: affected}, nil
}

func collectPKs(tx *bytdb.Txn, table string, desc *bytdb.TableDesc, pl *plan) ([][]any, error) {
	var pks [][]any
	err := scanPlan(tx, table, pl, func(r bytdb.Row) bool {
		pk := make([]any, len(desc.PKCols))
		for i, ord := range desc.PKCols {
			pk[i] = r.Vals[ord]
		}
		pks = append(pks, pk)
		return true
	})
	return pks, err
}

func (d *DB) execDropIndex(s *DropIndex) (*Result, error) {
	table := s.Table
	if table == "" {
		var found []string
		for _, tn := range d.e.Tables() {
			if d.e.Table(tn).Index(s.Name) != nil {
				found = append(found, tn)
			}
		}
		switch len(found) {
		case 0:
			return nil, serr.New("no such index", "index", s.Name)
		case 1:
			table = found[0]
		default:
			return nil, serr.New("index name is ambiguous; use DROP INDEX name ON table", "index", s.Name)
		}
	}
	if err := d.e.DropIndex(table, s.Name); err != nil {
		return nil, err
	}
	return &Result{}, nil
}
