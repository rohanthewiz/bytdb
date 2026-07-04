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
		if ok && p.matches(row) {
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
		if !p.matches(row) {
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

// tri is a three-valued SQL truth value: a comparison against NULL
// (or between incomparable kinds) is unknown, NOT preserves unknown,
// and AND/OR treat it per Kleene logic. A row or group matches a
// condition only when it evaluates to definitely true.
type tri int8

const (
	triFalse tri = iota
	triTrue
	triUnknown
)

func (t tri) not() tri {
	switch t {
	case triTrue:
		return triFalse
	case triFalse:
		return triTrue
	}
	return triUnknown
}

// evalBool evaluates a boolean expression; leaf values each Pred.
// A nil expression is true (no WHERE / no HAVING).
func evalBool(e BoolExpr, leaf func(*Pred) tri) tri {
	switch n := e.(type) {
	case nil:
		return triTrue
	case *Pred:
		return leaf(n)
	case *Not:
		return evalBool(n.Expr, leaf).not()
	case *And:
		out := triTrue
		for _, sub := range n.Exprs {
			switch evalBool(sub, leaf) {
			case triFalse:
				return triFalse
			case triUnknown:
				out = triUnknown
			}
		}
		return out
	case *Or:
		out := triFalse
		for _, sub := range n.Exprs {
			switch evalBool(sub, leaf) {
			case triTrue:
				return triTrue
			case triUnknown:
				out = triUnknown
			}
		}
		return out
	}
	return triUnknown
}

// checkPred applies one predicate to the item's value. IS [NOT] NULL
// is always definite; a comparison involving NULL or incomparable
// kinds is unknown.
func checkPred(v any, op PredOp, lit any) tri {
	switch op {
	case OpIsNull:
		if v == nil {
			return triTrue
		}
		return triFalse
	case OpIsNotNull:
		if v != nil {
			return triTrue
		}
		return triFalse
	}
	c, ok := compareVals(v, lit)
	if !ok {
		return triUnknown
	}
	hit := false
	switch op {
	case OpEQ:
		hit = c == 0
	case OpNE:
		hit = c != 0
	case OpLT:
		hit = c < 0
	case OpLE:
		hit = c <= 0
	case OpGT:
		hit = c > 0
	case OpGE:
		hit = c >= 0
	}
	if hit {
		return triTrue
	}
	return triFalse
}

// matches reports whether a row definitely satisfies the plan's
// residual filter.
func (p *plan) matches(row bytdb.Row) bool {
	return evalPreds(p.filter, p.binds, row.Vals) == triTrue
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
	var rows [][]any // full combined rows; projected after sort/limit
	var ords []int   // projected column ordinals
	var keys []sortKey
	err := d.e.ReadTxn(func(tx *bytdb.Txn) error {
		fp, err := prepareFrom(tx, s.From, s.Where)
		if err != nil {
			return err
		}
		sc := fp.sc
		if ords, err = projectSelect(sc, s, res); err != nil {
			return err
		}
		for _, o := range s.OrderBy {
			ord, err := sc.resolve(o.Col)
			if err != nil {
				return err
			}
			keys = append(keys, sortKey{ord, o.Desc})
		}
		// With no ORDER BY the join order is the result order, so
		// collection can end at OFFSET+LIMIT rows.
		return runJoin(tx, fp, func(vals []any) bool {
			rows = append(rows, vals)
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

// projectSelect resolves a non-aggregate select list against the FROM
// scope: the combined-row ordinals to output, with the column names
// and types appended to res.
func projectSelect(sc *scope, s *Select, res *Result) (ords []int, err error) {
	project := func(st scopeTable) {
		for i, c := range st.desc.Columns {
			ords = append(ords, st.off+i)
			res.Cols = append(res.Cols, c.Name)
			res.Types = append(res.Types, c.Type)
		}
	}
	if s.Star {
		for _, st := range sc.tables {
			project(st)
		}
		return ords, nil
	}
	for _, it := range s.Items {
		if it.Star { // t.*
			st, err := sc.table(it.Col.Table)
			if err != nil {
				return nil, err
			}
			project(st)
			continue
		}
		ord, err := sc.resolve(it.Col)
		if err != nil {
			return nil, err
		}
		ords = append(ords, ord)
		c := sc.column(ord)
		res.Cols = append(res.Cols, c.Name)
		res.Types = append(res.Types, c.Type)
	}
	return ords, nil
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
		pl, err := planScan(desc, s.Table, s.Where)
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
		pl, err := planScan(desc, s.Table, s.Where)
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
