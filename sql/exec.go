package sql

import (
	"bytes"
	"cmp"
	"fmt"
	"iter"
	"math"
	"slices"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// scanPlan yields the rows a plan matches, in the chosen path's key
// order, within tx's snapshot. env supplies the evaluation
// environment for any Cond leaves in the residual filter (nil when
// the filter is known to hold none). yield returning false stops the
// scan.
func scanPlan(tx *bytdb.Txn, table string, p *plan, env *exEnv, yield func(bytdb.Row) bool) error {
	if p.get != nil {
		row, ok, err := tx.Get(table, p.get...)
		if err != nil {
			return err
		}
		if ok {
			hit, err := p.matches(env, row)
			if err != nil {
				return err
			}
			if hit {
				yield(row)
			}
		}
		return nil
	}
	// A reverse scan swaps the roles of the plan's edges: the stops mark
	// where a forward walk would end, so they become the reverse walk's
	// entry bound (revEnd), and from — the forward entry — becomes where
	// the engine stops descending. Both edges are then key bounds, so no
	// stop checks run per row.
	var seq iter.Seq2[bytdb.Row, error]
	switch {
	case p.reverse && p.index == "":
		to, incl := p.revEnd()
		seq = tx.ScanRangeRev(table, p.from, to, incl)
	case p.reverse:
		to, incl := p.revEnd()
		seq = tx.ScanIndexRev(table, p.index, p.from, to, incl)
	case p.index == "":
		seq = tx.ScanRange(table, p.from, nil)
	default:
		seq = tx.ScanIndex(table, p.index, p.from, nil)
	}
	for row, err := range seq {
		if err != nil {
			return err
		}
		if !p.reverse && stopped(p.stops, row) {
			return nil
		}
		hit, err := p.matches(env, row)
		if err != nil {
			return err
		}
		if !hit {
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
			switch st.kind {
			case stopNE:
				return true // the equality region cannot contain NULL
			case stopLE, stopLT:
				return true // descending column: NULLs sort after every value
			}
			continue // an ascending range column's NULL group sorts before its values
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
		case stopLE:
			if c <= 0 {
				return true
			}
		case stopLT:
			if c < 0 {
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

// evalBool evaluates a boolean expression; leaf values each Pred or
// Cond leaf. A nil expression is true (no WHERE / no HAVING).
func evalBool(e BoolExpr, leaf func(BoolExpr) (tri, error)) (tri, error) {
	switch n := e.(type) {
	case nil:
		return triTrue, nil
	case *Pred, *Cond:
		return leaf(e)
	case *Not:
		t, err := evalBool(n.Expr, leaf)
		return t.not(), err
	case *And:
		out := triTrue
		for _, sub := range n.Exprs {
			t, err := evalBool(sub, leaf)
			if err != nil {
				return triUnknown, err
			}
			switch t {
			case triFalse:
				return triFalse, nil
			case triUnknown:
				out = triUnknown
			}
		}
		return out, nil
	case *Or:
		out := triFalse
		for _, sub := range n.Exprs {
			t, err := evalBool(sub, leaf)
			if err != nil {
				return triUnknown, err
			}
			switch t {
			case triTrue:
				return triTrue, nil
			case triUnknown:
				out = triUnknown
			}
		}
		return out, nil
	}
	return triUnknown, serr.New("unhandled condition kind")
}

// checkPred applies one predicate to the item's value. IS [NOT] NULL
// is always definite; a comparison involving NULL or incomparable
// kinds is unknown — except that a string operand facing a number or
// boolean re-reads as that type first, Postgres-style ('16384' = oid
// 16384), which covers positions the static coercion pass cannot see
// (subqueries, expression internals).
func checkPred(v any, op PredOp, lit any) (tri, error) {
	switch op {
	case OpIsNull:
		if v == nil {
			return triTrue, nil
		}
		return triFalse, nil
	case OpIsNotNull:
		if v != nil {
			return triTrue, nil
		}
		return triFalse, nil
	case OpRegex, OpNotRegex, OpRegexI, OpNotRegexI:
		if v == nil || lit == nil {
			return triUnknown, nil
		}
		s, okS := v.(string)
		pat, okP := lit.(string)
		if !okS || !okP {
			return triUnknown, serr.New("regex match requires text operands")
		}
		re, err := compileRegex(pat, op == OpRegexI || op == OpNotRegexI)
		if err != nil {
			return triUnknown, err
		}
		hit := re.MatchString(s)
		if op == OpNotRegex || op == OpNotRegexI {
			hit = !hit
		}
		if hit {
			return triTrue, nil
		}
		return triFalse, nil
	}
	if v != nil && lit != nil {
		if _, ok := compareVals(v, lit); !ok {
			if s, isS := v.(string); isS {
				if cv, err := coerceLit(s, kindOf(lit)); err == nil {
					v = cv
				}
			} else if s, isS := lit.(string); isS {
				if cl, err := coerceLit(s, kindOf(v)); err == nil {
					lit = cl
				}
			}
		}
	}
	c, ok := compareVals(v, lit)
	if !ok {
		return triUnknown, nil
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
		return triTrue, nil
	}
	return triFalse, nil
}

// kindOf is the column type a runtime value reads as.
func kindOf(v any) bytdb.ColType {
	switch v.(type) {
	case int64:
		return bytdb.TInt
	case float64:
		return bytdb.TFloat
	case bool:
		return bytdb.TBool
	case []byte:
		return bytdb.TBytes
	}
	return bytdb.TString
}

// matches reports whether a row definitely satisfies the plan's
// residual filter.
func (p *plan) matches(env *exEnv, row bytdb.Row) (bool, error) {
	t, err := evalPreds(p.filter, p.binds, env, row.Vals)
	return t == triTrue, err
}

// compareVals orders two non-NULL values. ok is false for NULLs and
// for incomparable kinds; int64 and float64 compare numerically, with
// NaN handled per Postgres (see cmpFloat).
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
			return cmpFloat(float64(av), bv), true
		}
	case float64:
		switch bv := b.(type) {
		case float64:
			return cmpFloat(av, bv), true
		case int64:
			return cmpFloat(av, float64(bv)), true
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

// cmpFloat compares floats with Postgres's NaN convention: NaN is
// greater than every non-NaN value and equal to itself, so it sorts
// last ascending and groups deterministically. Go's cmp.Compare does
// the opposite (NaN smallest), which would silently diverge in ORDER
// BY, MIN/MAX, and range comparisons. The store rejects NaN in keys,
// but expressions and parameters can still produce one.
func cmpFloat(a, b float64) int {
	an, bn := math.IsNaN(a), math.IsNaN(b)
	switch {
	case an && bn:
		return 0
	case an:
		return 1
	case bn:
		return -1
	}
	return cmp.Compare(a, b)
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
	var rows [][]any     // full combined rows; projected after sort/limit
	var proj []projEntry // projected column ordinals and literals
	var keys []sortKey
	err := d.read(func(tx *bytdb.Txn) error {
		fp, err := prepareFrom(d.lookup(tx.Table), s.From, s.Where)
		if err != nil {
			return err
		}
		sc := fp.sc
		var exprs []Expr // evaluated per row, appended after the combined columns
		if proj, exprs, err = projectSelect(sc, s, res); err != nil {
			return err
		}
		if keys, exprs, err = buildSortKeys(sc, s, proj, exprs); err != nil {
			return err
		}
		// When the sole table's scan can yield rows already in ORDER BY
		// order, plan it that way and drop the sort keys: collection then
		// stops at OFFSET+LIMIT and sortOffsetProject skips the sort.
		if p, elim, err := orderedScan(fp, s, keys); err != nil {
			return err
		} else if elim {
			fp.steps[0].plan = p
			keys = nil
		}
		env := &exEnv{d: d, tx: tx, sc: sc}
		var evalErr error
		// With no ORDER BY the join order is the result order, so
		// collection can end at OFFSET+LIMIT rows.
		err = runJoin(tx, fp, env, func(vals []any) bool {
			for _, ex := range exprs {
				rowEnv := *env
				rowEnv.row = vals[:sc.width]
				v, e := evalEx(&rowEnv, ex)
				if e != nil {
					evalErr = e
					return false
				}
				vals = append(vals, v)
			}
			rows = append(rows, vals)
			return len(keys) > 0 || s.Limit < 0 || int64(len(rows)) < satAdd(s.Offset, s.Limit)
		})
		if err != nil {
			return err
		}
		return evalErr
	})
	if err != nil {
		return nil, err
	}
	sortOffsetProject(res, rows, keys, proj, s.Offset, s.Limit)
	return res, nil
}

// sortOffsetProject finishes a non-grouped result: a stable multi-key
// sort (skipped when there are no keys), then OFFSET/LIMIT, then
// projection of each combined row down to the output columns. It fills
// res.Rows. Shared by the plain and window SELECT paths.
func sortOffsetProject(res *Result, rows [][]any, keys []sortKey, proj []projEntry, offset, limit int64) {
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
	if offset > 0 {
		if offset >= int64(len(rows)) {
			rows = nil
		} else {
			rows = rows[offset:]
		}
	}
	if limit >= 0 && int64(len(rows)) > limit {
		rows = rows[:limit]
	}
	res.Rows = make([][]any, len(rows))
	for i, r := range rows {
		out := make([]any, len(proj))
		for j, pe := range proj {
			if pe.ord < 0 {
				out[j] = pe.lit
			} else {
				out[j] = r[pe.ord]
			}
		}
		res.Rows[i] = out
	}
}

// satAdd adds two non-negative int64s, saturating at MaxInt64. The
// collection cutoff above is OFFSET+LIMIT; huge (but individually
// valid) values would wrap that sum negative and silently truncate
// the result after the first row.
func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

type sortKey struct {
	ord  int
	desc bool
}

// buildSortKeys resolves a SELECT's ORDER BY into combined-row sort
// keys: select-list ordinals (ORDER BY n), output aliases, hidden sort
// expressions (appended to exprs, evaluated per row like select items),
// or FROM columns. It returns the keys and the possibly-extended exprs.
func buildSortKeys(sc *scope, s *Select, proj []projEntry, exprs []Expr) ([]sortKey, []Expr, error) {
	var keys []sortKey
	for _, o := range s.OrderBy {
		if o.IsLit { // ORDER BY n: a select-list position
			pe, err := ordinalEntry(o.Lit, proj)
			if err != nil {
				return nil, nil, err
			}
			if pe.ord >= 0 {
				keys = append(keys, sortKey{pe.ord, o.Desc})
			}
			continue // a literal output is a constant key: no effect
		}
		if o.Ex != nil { // a hidden sort expression, appended like an item
			keys = append(keys, sortKey{sc.width + len(exprs), o.Desc})
			exprs = append(exprs, o.Ex)
			continue
		}
		// A bare name matching an output alias sorts by that output.
		if o.Col.Table == "" {
			if pe, ok := aliasEntry(sc, s, proj, o.Col.Name); ok {
				if pe.ord >= 0 {
					keys = append(keys, sortKey{pe.ord, o.Desc})
				}
				continue
			}
		}
		ord, err := sc.resolve(o.Col)
		if err != nil {
			return nil, nil, err
		}
		keys = append(keys, sortKey{ord, o.Desc})
	}
	return keys, exprs, nil
}

// orderedScan decides, for a single-table non-aggregate, non-window
// SELECT, whether the table's scan can yield rows already in ORDER BY
// order. It returns the order-aware plan for the sole table and whether
// the sort (and, under a LIMIT, the tail rows) can be skipped. Not
// eligible -> (nil, false): the caller keeps the ordinary plan and
// sorts. Every sort key must be a base column of the table; an ORDER BY
// expression (ord past the table's width) forces the sort.
func orderedScan(fp *fromPlan, s *Select, keys []sortKey) (*plan, bool, error) {
	if len(keys) == 0 || len(fp.steps) != 1 {
		return nil, false, nil
	}
	step := &fp.steps[0]
	if step.st.rows != nil || len(step.tmpls) != 0 {
		return nil, false, nil
	}
	width := len(step.st.desc.Columns)
	for _, k := range keys {
		if k.ord < 0 || k.ord >= width {
			return nil, false, nil
		}
	}
	return chooseOrderedPlan(step.st.desc, step.st.name, combineStatic(step.static), keys, s.Limit >= 0)
}

// combineStatic folds a step's static conjuncts into one condition, as
// rec does before planning a scan.
func combineStatic(static []BoolExpr) BoolExpr {
	switch len(static) {
	case 0:
		return nil
	case 1:
		return static[0]
	default:
		return &And{Exprs: static}
	}
}

// projEntry is one projected output: a combined-row ordinal, or a
// literal (ord < 0).
type projEntry struct {
	ord int
	lit any
}

// projectSelect resolves a non-aggregate select list against the FROM
// scope: the projection entries, with the column names and types
// appended to res. Expression items come back in exprs; they are
// evaluated per row and appended to the combined row, and their
// projection ordinals point past sc.width accordingly.
func projectSelect(sc *scope, s *Select, res *Result) (proj []projEntry, exprs []Expr, err error) {
	project := func(st scopeTable) {
		for i, c := range st.desc.Columns {
			proj = append(proj, projEntry{ord: st.off + i})
			res.Cols = append(res.Cols, c.Name)
			res.Types = append(res.Types, c.Type)
		}
	}
	name := func(it SelectItem, def string) string {
		if it.As != "" {
			return it.As
		}
		return def
	}
	if s.Star {
		if len(sc.tables) == 0 {
			return nil, nil, serr.New("SELECT * requires a FROM clause")
		}
		for _, st := range sc.tables {
			project(st)
		}
		return proj, nil, nil
	}
	for _, it := range s.Items {
		switch {
		case it.Ex != nil:
			proj = append(proj, projEntry{ord: sc.width + len(exprs)})
			res.Cols = append(res.Cols, name(it, exprName(it.Ex)))
			res.Types = append(res.Types, exprType(sc, it.Ex))
			exprs = append(exprs, it.Ex)
			continue
		case it.IsLit:
			proj = append(proj, projEntry{ord: -1, lit: it.Lit})
			res.Cols = append(res.Cols, name(it, it.LitName))
			res.Types = append(res.Types, litType(it.Lit))
			continue
		case it.Star: // t.*
			st, err := sc.table(it.Col.Table)
			if err != nil {
				return nil, nil, err
			}
			project(st)
			continue
		}
		ord, err := sc.resolve(it.Col)
		if err != nil {
			return nil, nil, err
		}
		proj = append(proj, projEntry{ord: ord})
		c := sc.column(ord)
		res.Cols = append(res.Cols, name(it, c.Name))
		res.Types = append(res.Types, c.Type)
	}
	return proj, exprs, nil
}

// aliasEntry finds the projection entry behind an output alias, for
// ORDER BY name resolution (an alias wins over a FROM column).
func aliasEntry(sc *scope, s *Select, proj []projEntry, name string) (projEntry, bool) {
	if s.Star {
		return projEntry{}, false
	}
	pi := 0
	for _, it := range s.Items {
		if it.Star { // t.* expands to the table's width
			st, err := sc.table(it.Col.Table)
			if err != nil {
				return projEntry{}, false
			}
			pi += len(st.desc.Columns)
			continue
		}
		if it.As == name && pi < len(proj) {
			return proj[pi], true
		}
		pi++
	}
	return projEntry{}, false
}

// litType is the column type a literal select item reports.
func litType(v any) bytdb.ColType {
	switch v.(type) {
	case int64:
		return bytdb.TInt
	case float64:
		return bytdb.TFloat
	case bool:
		return bytdb.TBool
	case []byte:
		return bytdb.TBytes
	}
	return bytdb.TString // strings and NULL
}

// ordinalEntry resolves ORDER BY n to the n-th projection entry.
func ordinalEntry(lit any, proj []projEntry) (projEntry, error) {
	n, ok := lit.(int64)
	if !ok {
		return projEntry{}, serr.New("non-integer constant in ORDER BY")
	}
	if n < 1 || int(n) > len(proj) {
		return projEntry{}, serr.New("ORDER BY position is not in the select list",
			"position", fmt.Sprint(n))
	}
	return proj[n-1], nil
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
	err := d.write(func(tx *bytdb.Txn) error {
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
		checks, err := tableChecks(desc)
		if err != nil {
			return err
		}
		var env *exEnv
		if len(checks) > 0 {
			env = d.tableEnv(tx, s.Table, desc)
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
			// Arity mismatches fall through to the engine's error.
			if env != nil && len(vals) == len(desc.Columns) {
				if err := checkRow(env, s.Table, checks, vals); err != nil {
					return err
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
	err := d.write(func(tx *bytdb.Txn) error {
		desc := tx.Table(s.Table)
		if desc == nil {
			return serr.New("no such table", "table", s.Table)
		}
		pl, err := planScan(desc, s.Table, s.Where)
		if err != nil {
			return err
		}
		checks, err := tableChecks(desc)
		if err != nil {
			return err
		}
		setOrds := map[string]int{}
		for col := range s.Set {
			ord := desc.ColIndex(col)
			if ord < 0 {
				return serr.New("no such column", "table", s.Table, "column", col)
			}
			setOrds[col] = ord
		}
		// CHECK expressions must judge the row tx.Update will actually
		// store, so the SET values coerce to their column types before
		// the check row is built — a quoted '5' into an int column has
		// to check as the integer 5, not the string "5". run() already
		// literal-coerced s.Set against the engine's current catalog,
		// but this executor resolves the transaction's catalog; coercing
		// here (a no-op on already-coerced values) removes that hidden
		// coupling instead of trusting it.
		setVals := s.Set
		if len(checks) > 0 {
			setVals = make(map[string]any, len(s.Set))
			for col, v := range s.Set {
				cv, err := coerceLit(v, desc.Columns[setOrds[col]].Type)
				if err != nil {
					return serr.Wrap(err, "table", s.Table, "column", col)
				}
				setVals[col] = cv
			}
		}
		// Materialize matching rows before writing: updates move rows
		// and index entries under a live scan.
		env := d.tableEnv(tx, s.Table, desc)
		var rows [][]any
		err = scanPlan(tx, s.Table, pl, env, func(r bytdb.Row) bool {
			rows = append(rows, r.Vals)
			return true
		})
		if err != nil {
			return err
		}
		for _, rv := range rows {
			if len(checks) > 0 {
				nv := slices.Clone(rv)
				for col, v := range setVals {
					nv[setOrds[col]] = v
				}
				if err := checkRow(env, s.Table, checks, nv); err != nil {
					return err
				}
			}
			pk := make([]any, len(desc.PKCols))
			for i, ord := range desc.PKCols {
				pk[i] = rv[ord]
			}
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
	err := d.write(func(tx *bytdb.Txn) error {
		desc := tx.Table(s.Table)
		if desc == nil {
			return serr.New("no such table", "table", s.Table)
		}
		pl, err := planScan(desc, s.Table, s.Where)
		if err != nil {
			return err
		}
		pks, err := collectPKs(tx, s.Table, desc, pl, d.tableEnv(tx, s.Table, desc))
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

// tableEnv is the evaluation environment for a single-table scan
// (UPDATE/DELETE), whose plan binds are table-local ordinals.
func (d *DB) tableEnv(tx *bytdb.Txn, table string, desc *bytdb.TableDesc) *exEnv {
	sc := &scope{
		tables: []scopeTable{{name: table, desc: desc}},
		width:  len(desc.Columns),
	}
	return &exEnv{d: d, tx: tx, sc: sc}
}

func collectPKs(tx *bytdb.Txn, table string, desc *bytdb.TableDesc, pl *plan, env *exEnv) ([][]any, error) {
	var pks [][]any
	err := scanPlan(tx, table, pl, env, func(r bytdb.Row) bool {
		pk := make([]any, len(desc.PKCols))
		for i, ord := range desc.PKCols {
			pk[i] = r.Vals[ord]
		}
		pks = append(pks, pk)
		return true
	})
	return pks, err
}

// runSelectCore executes one UNION arm (or a whole plain SELECT).
func (d *DB) runSelectCore(s *Select) (*Result, error) {
	if s.isAggregate() {
		if hasWindow(s) {
			return nil, serr.New("window functions with GROUP BY or aggregates are not supported")
		}
		return d.execSelectAgg(s)
	}
	if hasWindow(s) {
		return d.execSelectWindow(s)
	}
	return d.execSelect(s)
}

// execUnion executes a UNION chain left to right: arms concatenate,
// and each distinct arm dedups everything accumulated so far. ORDER
// BY applies to the combined output and takes select-list positions
// or output column names.
func (d *DB) execUnion(s *Select) (*Result, error) {
	head := *s
	head.Union, head.OrderBy = nil, nil
	head.Limit, head.Offset = -1, 0
	res, err := d.runSelectCore(&head)
	if err != nil {
		return nil, err
	}
	rows := res.Rows
	for _, arm := range s.Union {
		armRes, err := d.runSelectCore(arm.Sel)
		if err != nil {
			return nil, err
		}
		if len(armRes.Cols) != len(res.Cols) {
			return nil, serr.New("each UNION query must have the same number of columns")
		}
		rows = append(rows, armRes.Rows...)
		if !arm.All {
			if rows, err = dedupRows(rows); err != nil {
				return nil, err
			}
		}
	}
	var keys []sortKey
	for _, o := range s.OrderBy {
		switch {
		case o.IsLit:
			n, ok := o.Lit.(int64)
			if !ok {
				return nil, serr.New("non-integer constant in ORDER BY")
			}
			if n < 1 || int(n) > len(res.Cols) {
				return nil, serr.New("ORDER BY position is not in the select list",
					"position", fmt.Sprint(n))
			}
			keys = append(keys, sortKey{int(n - 1), o.Desc})
		case o.Ex == nil && o.Agg == AggNone && o.Col.Table == "":
			found := -1
			for i, c := range res.Cols {
				if c == o.Col.Name {
					found = i
					break
				}
			}
			if found < 0 {
				return nil, serr.New("ORDER BY on a UNION must name an output column",
					"column", o.Col.Name)
			}
			keys = append(keys, sortKey{found, o.Desc})
		default:
			return nil, serr.New("ORDER BY on a UNION takes output columns or positions")
		}
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
	res.Rows = rows
	res.RowsAffected = 0
	return res, nil
}

// dedupRows keeps each distinct row's first occurrence, compared by
// the order-preserving tuple encoding (NULLs equal NULLs, as UNION
// requires).
func dedupRows(rows [][]any) ([][]any, error) {
	seen := make(map[string]bool, len(rows))
	out := rows[:0]
	for _, r := range rows {
		kb, err := tuple.Encode(r...)
		if err != nil {
			return nil, serr.Wrap(err, "op", "encode UNION row")
		}
		if seen[string(kb)] {
			continue
		}
		seen[string(kb)] = true
		out = append(out, r)
	}
	return out, nil
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
