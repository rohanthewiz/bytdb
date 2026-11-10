package sql

import (
	"context"
	"math"
	"slices"
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// scope is the row shape a FROM clause produces: each table's columns
// concatenated in FROM order. Column references resolve to ordinals
// in the combined row.
type scope struct {
	tables []scopeTable
	width  int
	// correlated marks the scope of a subquery the full executor runs
	// per outer row (subselect.go): a column it does not resolve may be
	// an outer reference, which evaluation finds through exEnv.outer.
	// Only the aggregate rewriter consults it — it otherwise insists that
	// every column be a group key or an aggregate argument.
	correlated bool
	// merged maps the columns of JOIN ... USING to the left table's
	// ordinal: an unqualified reference to one is not ambiguous, as in
	// Postgres, where the USING column is a single merged column. For
	// the INNER and LEFT joins bytdb has, the merged value is the left
	// table's (equal to the right's, or the left row's own when the
	// right side is NULL-extended).
	merged map[string]int
}

type scopeTable struct {
	name string // what qualifiers match: the alias, or the table name
	desc *bytdb.TableDesc
	off  int     // this table's first ordinal in the combined row
	rows [][]any // materialized rows of a virtual system table; nil for real tables
}

func buildScope(lookup tableLookup, items []FromItem) (*scope, error) {
	sc := &scope{}
	seen := map[string]bool{}
	for _, it := range items {
		if it.FuncArgs != nil {
			return nil, serr.New("set-returning functions in FROM are not supported",
				"function", it.Table)
		}
		desc, rows := lookup(it.Table)
		if desc == nil {
			return nil, noSuchTable(it.Table)
		}
		name := it.Alias
		if name == "" {
			// A schema-qualified system table is referenced by its bare
			// name (pg_catalog.pg_class c aside, ON n.oid = ... uses
			// pg_class.oid), as in Postgres.
			name = it.Table
			if _, bare, ok := strings.Cut(name, "."); ok {
				name = bare
			}
		}
		if seen[name] {
			return nil, serr.New("table name appears twice in FROM; alias one of them", "name", name)
		}
		seen[name] = true
		for _, c := range it.Using {
			for _, st := range sc.tables {
				if ord := st.desc.ColIndex(c); ord >= 0 {
					if sc.merged == nil {
						sc.merged = map[string]int{}
					}
					if _, already := sc.merged[c]; !already {
						sc.merged[c] = st.off + ord
					}
					break
				}
			}
		}
		sc.tables = append(sc.tables, scopeTable{name: name, desc: desc, off: sc.width, rows: rows})
		sc.width += len(desc.Columns)
	}
	return sc, nil
}

// prefix is the scope an ON condition sees: the first n tables.
func (sc *scope) prefix(n int) *scope {
	last := sc.tables[n-1]
	return &scope{tables: sc.tables[:n], width: last.off + len(last.desc.Columns), merged: sc.merged}
}

// resolve maps a column reference to its combined-row ordinal. An
// unqualified name must be unambiguous across the scope's tables, and
// a name must be unique within its table: stored tables cannot repeat
// a column name, but a derived table or CTE can (SELECT a.id, b.id
// FROM a, b), and Postgres then calls any reference to it ambiguous —
// resolving to the first silently picked a column the query never
// named (deep-look D-21). TableDesc.ColIndexUnique answers both
// questions in one pass, through a name index on wide descriptors.
func (sc *scope) resolve(c ColRef) (int, error) {
	if c.Table != "" {
		for _, st := range sc.tables {
			if st.name == c.Table {
				ord, unique := st.desc.ColIndexUnique(c.Name)
				if ord < 0 {
					return -1, noSuchColumn(c.Table, c.Name)
				}
				if !unique {
					return -1, ambiguousColumn(c.Name)
				}
				return st.off + ord, nil
			}
		}
		return -1, serr.New("no such table in FROM", "table", c.Table)
	}
	if ord, ok := sc.merged[c.Name]; ok && ord < sc.width {
		return ord, nil
	}
	found := -1
	for _, st := range sc.tables {
		if ord, unique := st.desc.ColIndexUnique(c.Name); ord >= 0 {
			if found >= 0 || !unique {
				return -1, ambiguousColumn(c.Name)
			}
			found = st.off + ord
		}
	}
	if found < 0 {
		return -1, noSuchColumn("", c.Name)
	}
	return found, nil
}

func ambiguousColumn(name string) error {
	return serr.New(`column reference "`+name+`" is ambiguous; qualify it`, "column", name)
}

// table finds a scope table by alias or name (for t.* expansion).
func (sc *scope) table(name string) (scopeTable, error) {
	for _, st := range sc.tables {
		if st.name == name {
			return st, nil
		}
	}
	return scopeTable{}, serr.New("no such table in FROM", "table", name)
}

// column is the descriptor column behind a combined ordinal.
func (sc *scope) column(ord int) bytdb.Column {
	st := sc.tables[sc.tableOf(ord)]
	return st.desc.Columns[ord-st.off]
}

// tableOf is the index of the scope table containing ordinal ord.
func (sc *scope) tableOf(ord int) int {
	for i := len(sc.tables) - 1; i >= 0; i-- {
		if ord >= sc.tables[i].off {
			return i
		}
	}
	return 0
}

// binding is one predicate's resolved operand ordinals in the
// combined row; r is -1 when the right side is a literal.
type binding struct{ l, r int }

type binds map[*Pred]binding

// bind resolves every column reference in a condition against sc,
// recording each leaf's ordinals.
func (b binds) bind(e BoolExpr, sc *scope) error {
	return walkPreds(e, func(pr *Pred) error {
		l, err := sc.resolve(pr.Item.Col)
		if err != nil {
			return err
		}
		r := -1
		if pr.RItem != nil {
			if r, err = sc.resolve(pr.RItem.Col); err != nil {
				return err
			}
		}
		b[pr] = binding{l, r}
		return nil
	})
}

// evalPreds evaluates a bound condition over one combined row. base
// supplies the evaluation environment for Cond leaves (nil when the
// tree is known to hold none).
func evalPreds(e BoolExpr, b binds, base *exEnv, vals []any) (tri, error) {
	return evalBool(e, func(leaf BoolExpr) (tri, error) {
		switch n := leaf.(type) {
		case *Pred:
			bd := b[n]
			rhs := n.Val
			if bd.r >= 0 {
				rhs = vals[bd.r]
			}
			return checkPred(vals[bd.l], n.Op, rhs)
		case *Cond:
			if base == nil {
				return triUnknown, serr.New("internal: expression condition without an environment")
			}
			// Point the caller's env at this row for the call instead of
			// copying it: a copy escaped to the heap, one ~96-byte
			// allocation per row per Cond (deep-look D-44). Evaluation
			// never keeps the env past the call (a subquery links it as
			// its outer scope only while it runs), and the previous row
			// is restored for whoever set it.
			prev := base.row
			base.row = vals
			t, err := evalTruth(base, n.Ex)
			base.row = prev
			return t, err
		}
		return triUnknown, serr.New("internal: unknown condition leaf")
	})
}

// fromPlan is a prepared FROM clause: the scope, one nested-loop step
// per table, and the bound WHERE, applied to full combined rows.
type fromPlan struct {
	sc    *scope
	steps []joinStep
	where BoolExpr
	b     binds // combined-space bindings for the WHERE and every ON
}

// joinStep scans one table once per combination of the tables before
// it. static conjuncts mention only this table and push into every
// scan; each template re-binds a conjunct between this table and an
// earlier one, turning it into a literal predicate per outer row (the
// index nested-loop: an equality template on a key prefix makes the
// inner scan a point get or bounded range).
type joinStep struct {
	it     FromItem
	st     scopeTable
	static []BoolExpr
	tmpls  []predTmpl
	// colSel is the SELECT whose column references bound what this
	// step's scans decode (colneed.go); nil outside a SELECT. cols caches
	// the mask, computed on first use (colsDone).
	colSel   *Select
	cols     []bool
	colsDone bool

	// probes are IN-list conjuncts on this table's columns (col IN
	// (constants), col = ANY(array)). They are Conds, so they cannot join
	// static — the step's scan filter runs without an evaluation
	// environment — but the planner may turn one into key probes
	// (inlist.go, deep-look D-38); the WHERE or ON that holds them still
	// checks every row.
	probes []BoolExpr
	plan   *plan // precomputed access path (order-aware single-table scan); nil: plan per outer row

	// tmplPreds/tmplPlan cache the nested-loop inner scan across outer
	// rows. Planning from scratch per outer row cost ~6 map allocations
	// plus a full predicate re-analysis, all to reach the same access
	// path every time: which columns are bound and the Go type of each
	// bound value (fixed per source column) fully determine planScan's
	// choice. So the template Pred nodes are allocated once and their
	// Vals mutated per outer row, and the plan built on the first row is
	// reused with only its pushed bounds refreshed (plan.rebind).
	//
	// "Fixed per source column" holds for stored columns but not for a
	// virtual-table source (a CTE or derived table whose column is an
	// untyped expression), whose values can change Go type from row to
	// row. tmplTypes records the kind each bound value had when the plan
	// was built (pushKind: Go type, and uuid-sized or not); a row
	// bringing a different one drops the cached plan and plans afresh,
	// as corrsub's bind does — rebinding a bound the plan encoded for
	// another type failed with "value does not fit column type"
	// (deep-look D-21).
	tmplPreds []*Pred
	tmplPlan  *plan
	tmplTypes []pushKind

	// hashEq, when non-empty, switches this step from the nested loop
	// to a hash join: the inner table (filtered by the static
	// conjuncts) is scanned once into a hash table keyed by these
	// equality columns, and each outer row probes instead of scanning.
	// Chosen only when no index could serve the equalities — the case
	// where the nested loop degrades to a full inner scan per outer
	// row. The templates it replaces are still re-verified through the
	// full ON/WHERE evaluation, so the bucket only has to be a
	// superset of the true matches.
	hashEq []hashEqKey
}

// hashEqKey is one hash-join key column: the inner table's own
// ordinal (build side) and the outer combined-row ordinal (probe
// side) of an equality conjunct.
type hashEqKey struct{ innerOrd, srcOrd int }

// predTmpl is "this table's column op the value at srcOrd of the
// outer row". pr is the conjunct it was made from (EXPLAIN needs the
// identity; execution does not).
type predTmpl struct {
	item   SelectItem
	op     PredOp
	srcOrd int
	pr     *Pred
}

// prepareFrom builds the scope, binds every ON (against the tables
// joined so far) and the WHERE (against everything), and assigns each
// step its pushable conjuncts. ON conjuncts always narrow their
// table's scan; WHERE conjuncts narrow a table's scan only when the
// table is not NULL-extended (the right side of a LEFT JOIN), where
// they must wait for the post-join filter.
func prepareFrom(lk tableLookup, items []FromItem, where BoolExpr) (*fromPlan, error) {
	sc, err := buildScope(lk, items)
	if err != nil {
		return nil, err
	}
	if items, err = foldUsing(sc, items); err != nil {
		return nil, err
	}
	fp := &fromPlan{sc: sc, where: where, b: binds{}}
	for k, it := range items {
		if it.On != nil {
			if err := fp.b.bind(it.On, sc.prefix(k+1)); err != nil {
				return nil, err
			}
		}
	}
	if err := fp.b.bind(where, sc); err != nil {
		return nil, err
	}

	whereConj := conjuncts(where)
	for k, it := range items {
		st := sc.tables[k]
		step := joinStep{it: it, st: st}
		avail := conjuncts(it.On)
		if it.Join != JoinLeft {
			avail = append(avail, whereConj...)
		}
		end := st.off + len(st.desc.Columns)
		// IN-list conjuncts whose column is this table's. The column is
		// resolved against the scope as far as this step, so an
		// unqualified name that belongs to another table never probes
		// this one.
		addProbe := func(part BoolExpr) {
			if col, ok := isInShape(part); ok {
				if ord, err := sc.prefix(k + 1).resolve(col); err == nil && ord >= st.off && ord < end {
					step.probes = append(step.probes, part)
				}
			}
		}
		eachPart(it.On, addProbe)
		if it.Join != JoinLeft {
			eachPart(where, addProbe)
		}
		for _, pr := range avail {
			bd := fp.b[pr]
			lIn := bd.l >= st.off && bd.l < end
			rIn := bd.r >= st.off && bd.r < end
			switch {
			case lIn && (bd.r < 0 || rIn):
				step.static = append(step.static, pr)
			case lIn && bd.r >= 0 && bd.r < st.off:
				step.tmpls = append(step.tmpls, predTmpl{item: pr.Item, op: pr.Op, srcOrd: bd.r, pr: pr})
			case rIn && bd.l < st.off:
				step.tmpls = append(step.tmpls, predTmpl{item: *pr.RItem, op: flip(pr.Op), srcOrd: bd.l, pr: pr})
			}
		}
		if k > 0 {
			step.hashEq = chooseHashJoin(sc, &step)
		}
		fp.steps = append(fp.steps, step)
	}
	return fp, nil
}

// chooseHashJoin decides whether a join step should build and probe a
// hash table instead of re-scanning per outer row. The conditions:
// the step has equality templates whose two sides hash compatibly,
// and no index or primary key could serve any of them (when one
// could, the nested loop's per-row point get or bounded scan is the
// better plan — it reads only the matching region and builds
// nothing). Virtual tables (CTEs, derived tables, views, catalog
// tables) have no indexes at all, so any equality qualifies them.
func chooseHashJoin(sc *scope, step *joinStep) []hashEqKey {
	var keys []hashEqKey
	for _, tp := range step.tmpls {
		if tp.op != OpEQ {
			continue
		}
		innerOrd := step.st.desc.ColIndex(tp.item.Col.Name)
		if innerOrd < 0 {
			return nil // resolution failed; leave it to the nested loop's error
		}
		if !hashCompatible(step.st.desc.Columns[innerOrd].Type, sc.column(tp.srcOrd).Type) {
			// Sides that only compare through dynamic coercion cannot
			// share a hash key; the nested loop keeps exact semantics.
			return nil
		}
		if step.st.rows == nil && indexServes(step.st.desc, innerOrd) {
			return nil
		}
		keys = append(keys, hashEqKey{innerOrd: innerOrd, srcOrd: tp.srcOrd})
	}
	return keys
}

// indexServes reports whether an equality on the column could push
// into the primary key or an index (it is some access path's first
// key column).
func indexServes(d *bytdb.TableDesc, ord int) bool {
	if len(d.PKCols) > 0 && d.PKCols[0] == ord {
		return true
	}
	for i := range d.Indexes {
		if len(d.Indexes[i].Cols) > 0 && d.Indexes[i].Cols[0] == ord {
			return true
		}
	}
	return false
}

// hashCompatible reports whether equal values of the two column types
// always encode to the same hash key. Identical types do; ints and
// floats do via the float64 normalization in hashJoinKey, and so does an
// int against a date or timestamp (compared as the day or microsecond
// count, which the typed bind pass accepts). A date against a timestamp
// does not: their int64s count different units, so they must never
// share a bucket (deep-look D-26) — the bind pass casts one side, which
// takes such a join off the hash path anyway. Everything else — notably
// text against a non-text — compares through coercion the key cannot
// reproduce.
func hashCompatible(a, b bytdb.ColType) bool {
	if a == b {
		return true
	}
	num := func(t bytdb.ColType) bool { return t == bytdb.TInt || t == bytdb.TFloat }
	if num(a) && num(b) {
		return true
	}
	return (a == bytdb.TInt && temporalType(b)) || (b == bytdb.TInt && temporalType(a))
}

// hashJoinKey encodes one side's key values; ok is false when any
// value is NULL (a NULL never equals anything, so it never joins) or
// unencodable. Integers normalize to float64 so cross-numeric
// equality (1 = 1.0) lands in the same bucket; the loss above 2^53
// only ever merges buckets, and every bucket row is re-verified by
// the full ON/WHERE evaluation.
func hashJoinKey(vals []any, ord func(hashEqKey) int, keys []hashEqKey) (string, bool) {
	ks := make([]any, len(keys))
	for i, k := range keys {
		v := vals[ord(k)]
		if v == nil {
			return "", false
		}
		switch n := v.(type) {
		case int64:
			v = float64(n)
		case float64:
			if math.IsNaN(n) {
				v = nanKey // NaN = NaN in SQL ordering semantics (cmpFloat)
			}
		}
		ks[i] = v
	}
	kb, err := tuple.Encode(ks...)
	if err != nil {
		return "", false
	}
	return string(kb), true
}

// runJoin nested-loops the FROM clause within tx's snapshot, yielding
// each combined row on which every ON and the WHERE are definitely
// true. env is the evaluation environment for expression conditions
// (its row is set per combined row). yield returning false stops the
// whole join.
func runJoin(tx *bytdb.Txn, fp *fromPlan, env *exEnv, yield func([]any) bool) error {
	j := &joinRun{tx: tx, fp: fp, env: env, yield: yield}
	if env != nil && env.d != nil {
		j.ctx = env.d.ctx // the statement's cancellation scope, polled by each step's scan
	}
	j.rec(0, nil)
	return j.err
}

type joinRun struct {
	tx    *bytdb.Txn
	fp    *fromPlan
	env   *exEnv
	ctx   context.Context
	yield func([]any) bool
	stop  bool
	err   error

	// hashes are the hash-join steps' build tables, keyed by
	// hashJoinKey, built lazily the first time each step runs.
	hashes []map[string][][]any
}

func (j *joinRun) rec(k int, partial []any) {
	if k == len(j.fp.steps) {
		t, err := evalPreds(j.fp.where, j.fp.b, j.env, partial)
		if err != nil {
			j.err = err
			return
		}
		if t == triTrue && !j.yield(partial) {
			j.stop = true
		}
		return
	}
	step := &j.fp.steps[k]
	if len(step.hashEq) > 0 {
		j.hashStep(k, partial)
		return
	}

	// A NULL source value can never satisfy its conjunct, so the scan
	// is skipped outright (a LEFT JOIN still NULL-extends below).
	skip := false
	for _, tp := range step.tmpls {
		if partial[tp.srcOrd] == nil {
			skip = true
			break
		}
	}

	matched := false
	if !skip {
		next := func(rowVals []any) bool {
			return j.stepNext(k, partial, rowVals, &matched)
		}
		if step.st.rows != nil {
			// A virtual system table: no pushdown, just its
			// materialized rows — ON and WHERE still filter fully.
			for _, rv := range step.st.rows {
				if !next(rv) {
					break
				}
			}
		} else {
			pl := step.plan
			if pl == nil {
				var err error
				if pl, err = step.rowPlan(partial); err != nil {
					j.err = err
					return
				}
			}
			// The pushed conjuncts are all plain Preds, so no
			// environment is needed for this scan's residual filter.
			err := scanPlan(j.ctx, j.tx, step.it.Table, pl, nil, func(r bytdb.Row) bool {
				return next(r.Vals)
			})
			if err != nil && j.err == nil {
				j.err = err
			}
		}
		if j.stop || j.err != nil {
			return
		}
	}
	if step.it.Join == JoinLeft && !matched {
		vals := make([]any, len(partial)+len(step.st.desc.Columns))
		copy(vals, partial)
		j.rec(k+1, vals)
	}
}

// rowPlan returns the step's inner-scan plan bound to the current
// outer row. The first call allocates the template Preds, assembles the
// pushdown expression, and plans; later calls mutate the template Vals
// in place and refresh the plan's pushed bounds. The residual filter
// and binds reference the same Pred nodes, so they see the new values
// with no rebuild.
func (step *joinStep) rowPlan(partial []any) (*plan, error) {
	if step.tmplPlan != nil {
		for i, tp := range step.tmpls {
			if pushKindOf(partial[tp.srcOrd]) != step.tmplTypes[i] {
				step.tmplPlan, step.tmplPreds = nil, nil
				break
			}
		}
	}
	if step.tmplPlan == nil {
		exprs := make([]BoolExpr, 0, len(step.static)+len(step.tmpls))
		exprs = append(exprs, step.static...)
		step.tmplPreds = make([]*Pred, len(step.tmpls))
		step.tmplTypes = make([]pushKind, len(step.tmpls))
		for i, tp := range step.tmpls {
			pr := &Pred{Item: tp.item, Op: tp.op, Val: partial[tp.srcOrd]}
			step.tmplPreds[i] = pr
			step.tmplTypes[i] = pushKindOf(pr.Val)
			exprs = append(exprs, pr)
		}
		var expr BoolExpr
		switch {
		case len(exprs) == 1:
			expr = exprs[0]
		case len(exprs) > 1:
			expr = &And{Exprs: exprs}
		}
		pl, err := planScan(step.st.desc, step.st.name, expr, step.probes...)
		if err != nil {
			return nil, err
		}
		if pl.get == nil { // a point get decodes one row; the mask is not worth computing
			pl.cols = step.needCols()
		}
		step.tmplPlan = pl
		return pl, nil
	}
	for i, tp := range step.tmpls {
		step.tmplPreds[i].Val = partial[tp.srcOrd]
	}
	step.tmplPlan.rebind()
	return step.tmplPlan, nil
}

// stepNext extends the outer row with one candidate inner row,
// applies the step's ON, and recurses to the next step. Returns false
// to stop the current step's row source.
func (j *joinRun) stepNext(k int, partial, rowVals []any, matched *bool) bool {
	step := &j.fp.steps[k]
	var vals []any
	if len(partial) == 0 && step.st.rows == nil {
		// The first table of a stored-table scan: the combined row IS the
		// scanned row, which the engine decoded freshly for this call and
		// nothing else holds, so it passes through instead of being
		// copied — about 10% of a one-table scan (deep-look D-44). A
		// virtual table's rows are shared materializations and are
		// still copied.
		vals = rowVals
	} else {
		vals = make([]any, len(partial)+len(rowVals))
		copy(vals, partial)
		copy(vals[len(partial):], rowVals)
	}
	if step.it.On != nil {
		t, err := evalPreds(step.it.On, j.fp.b, j.env, vals)
		if err != nil {
			j.err = err
			return false
		}
		if t != triTrue {
			return true
		}
	}
	*matched = true
	j.rec(k+1, vals)
	return !j.stop && j.err == nil
}

// hashStep runs one hash-join step: build the inner hash table once
// (statics pushed into the build scan), then probe with the outer
// row's key. Rows in a bucket still pass through the full ON (and
// later the WHERE), so bucket collisions and dropped non-equality
// templates cost work, never correctness.
func (j *joinRun) hashStep(k int, partial []any) {
	step := &j.fp.steps[k]
	if j.hashes == nil {
		j.hashes = make([]map[string][][]any, len(j.fp.steps))
	}
	ht := j.hashes[k]
	if ht == nil {
		ht = map[string][][]any{}
		add := func(rowVals []any) {
			if key, ok := hashJoinKey(rowVals, func(h hashEqKey) int { return h.innerOrd }, step.hashEq); ok {
				ht[key] = append(ht[key], rowVals)
			}
		}
		if step.st.rows != nil {
			for _, rv := range step.st.rows {
				add(rv)
			}
		} else {
			pl, err := planScan(step.st.desc, step.st.name, combineStatic(step.static), step.probes...)
			if err != nil {
				j.err = err
				return
			}
			pl.cols = step.needCols()
			// The build table holds the inner side's matching rows in
			// memory for the whole join, so it is charged like any other
			// row set (see rowbudget.go). A virtual table's rows (the branch
			// above) were already charged when they were materialized.
			var d *DB
			if j.env != nil {
				d = j.env.d
			}
			budget := d.budget()
			var capErr error
			n := 0
			// Static conjuncts are plain Preds (conjuncts() only yields
			// those), so the residual filter needs no environment.
			err = scanPlan(j.ctx, j.tx, step.it.Table, pl, nil, func(r bytdb.Row) bool {
				add(r.Vals)
				n++
				capErr = budget.check(n)
				return capErr == nil
			})
			if err == nil {
				err = capErr
			}
			if err != nil {
				j.err = err
				return
			}
		}
		j.hashes[k] = ht
	}

	matched := false
	if key, ok := hashJoinKey(partial, func(h hashEqKey) int { return h.srcOrd }, step.hashEq); ok {
		for _, rowVals := range ht[key] {
			if !j.stepNext(k, partial, rowVals, &matched) {
				break
			}
		}
	}
	if j.stop || j.err != nil {
		return
	}
	if step.it.Join == JoinLeft && !matched {
		vals := make([]any, len(partial)+len(step.st.desc.Columns))
		copy(vals, partial)
		j.rec(k+1, vals)
	}
}

// foldUsing turns each JOIN ... USING (cols) into ON conditions, now
// that the scope knows every table's columns: for each column, the one
// earlier table that has it equals the joined table's (deep-look D-52).
// Items are copied before any is changed — they belong to a parsed tree
// that may be shared.
//
// Unlike Postgres, a USING column is not merged into one in SELECT *:
// both tables' copies are listed (docs/gotchas.md).
func foldUsing(sc *scope, items []FromItem) ([]FromItem, error) {
	copied := false
	for k, it := range items {
		if len(it.Using) == 0 {
			continue
		}
		if !copied {
			items, copied = slices.Clone(items), true
		}
		right := sc.tables[k]
		var conds []BoolExpr
		for _, c := range it.Using {
			if right.desc.ColIndex(c) < 0 {
				return nil, serr.New(`column "` + c + `" specified in USING clause does not exist in right table`)
			}
			left := ""
			for _, st := range sc.tables[:k] {
				if st.desc.ColIndex(c) >= 0 {
					if left != "" {
						return nil, serr.New(`common column name "` + c + `" appears more than once in left table`)
					}
					left = st.name
				}
			}
			if left == "" {
				return nil, serr.New(`column "` + c + `" specified in USING clause does not exist in left table`)
			}
			r := SelectItem{Col: ColRef{Table: right.name, Name: c}}
			conds = append(conds, &Pred{Item: SelectItem{Col: ColRef{Table: left, Name: c}}, Op: OpEQ, RItem: &r})
		}
		if it.On != nil {
			conds = append(conds, it.On)
		}
		items[k].On = &And{Exprs: conds}
		if len(conds) == 1 {
			items[k].On = conds[0]
		}
		items[k].Using = nil
	}
	return items, nil
}
