package sql

import (
	"context"
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
			return nil, serr.New("no such table", "table", it.Table)
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
		sc.tables = append(sc.tables, scopeTable{name: name, desc: desc, off: sc.width, rows: rows})
		sc.width += len(desc.Columns)
	}
	return sc, nil
}

// prefix is the scope an ON condition sees: the first n tables.
func (sc *scope) prefix(n int) *scope {
	last := sc.tables[n-1]
	return &scope{tables: sc.tables[:n], width: last.off + len(last.desc.Columns)}
}

// resolve maps a column reference to its combined-row ordinal. An
// unqualified name must be unambiguous across the scope's tables.
func (sc *scope) resolve(c ColRef) (int, error) {
	if c.Table != "" {
		for _, st := range sc.tables {
			if st.name == c.Table {
				ord := st.desc.ColIndex(c.Name)
				if ord < 0 {
					return -1, serr.New("no such column", "table", c.Table, "column", c.Name)
				}
				return st.off + ord, nil
			}
		}
		return -1, serr.New("no such table in FROM", "table", c.Table)
	}
	found := -1
	for _, st := range sc.tables {
		if ord := st.desc.ColIndex(c.Name); ord >= 0 {
			if found >= 0 {
				return -1, serr.New("column reference is ambiguous; qualify it", "column", c.Name)
			}
			found = st.off + ord
		}
	}
	if found < 0 {
		return -1, serr.New("no such column", "column", c.Name)
	}
	return found, nil
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
			env := *base
			env.row = vals
			return evalTruth(&env, n.Ex)
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
	plan   *plan // precomputed access path (order-aware single-table scan); nil: plan per outer row

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
// always encode to the same hash key. Identical types do; the numeric
// class (ints, floats, and the int64-backed date/time types) does via
// the float64 normalization in hashJoinKey. Everything else — notably
// text against a non-text — compares through coercion the key cannot
// reproduce.
func hashCompatible(a, b bytdb.ColType) bool {
	if a == b {
		return true
	}
	num := func(t bytdb.ColType) bool {
		switch t {
		case bytdb.TInt, bytdb.TFloat, bytdb.TTimestamp, bytdb.TDate:
			return true
		}
		return false
	}
	return num(a) && num(b)
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
		if n, isInt := v.(int64); isInt {
			v = float64(n)
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

	// Fill the templates from the outer row. A NULL source value can
	// never satisfy its conjunct, so the scan is skipped outright (a
	// LEFT JOIN still NULL-extends below).
	exprs := make([]BoolExpr, 0, len(step.static)+len(step.tmpls))
	exprs = append(exprs, step.static...)
	skip := false
	for _, tp := range step.tmpls {
		v := partial[tp.srcOrd]
		if v == nil {
			skip = true
			break
		}
		exprs = append(exprs, &Pred{Item: tp.item, Op: tp.op, Val: v})
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
				var expr BoolExpr
				switch {
				case len(exprs) == 1:
					expr = exprs[0]
				case len(exprs) > 1:
					expr = &And{Exprs: exprs}
				}
				var err error
				if pl, err = planScan(step.st.desc, step.st.name, expr); err != nil {
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

// stepNext extends the outer row with one candidate inner row,
// applies the step's ON, and recurses to the next step. Returns false
// to stop the current step's row source.
func (j *joinRun) stepNext(k int, partial, rowVals []any, matched *bool) bool {
	step := &j.fp.steps[k]
	vals := make([]any, len(partial)+len(rowVals))
	copy(vals, partial)
	copy(vals[len(partial):], rowVals)
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
			pl, err := planScan(step.st.desc, step.st.name, combineStatic(step.static))
			if err != nil {
				j.err = err
				return
			}
			// Static conjuncts are plain Preds (conjuncts() only yields
			// those), so the residual filter needs no environment.
			err = scanPlan(j.ctx, j.tx, step.it.Table, pl, nil, func(r bytdb.Row) bool {
				add(r.Vals)
				return true
			})
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
