package sql

// subselect.go: subqueries through the real SELECT executor (deep-look
// D-27).
//
// The four subquery runners in expr.go (scalar, IN/ANY column, ARRAY,
// EXISTS) are a second, reduced executor: one FROM clause, a WHERE and a
// single item, with a per-outer-row plan cache for correlated
// predicates (corrsub.go). They never sorted, ignored LIMIT and OFFSET,
// and refused GROUP BY, HAVING, UNION and aggregates inside expressions
// — which is how (SELECT amt FROM o ORDER BY amt DESC LIMIT 1) returned
// the first row scanned, and why D-09 had to refuse those shapes.
//
// Every shape the runners do not implement now runs through
// runSelectBody, the executor every top-level SELECT uses, so sorting,
// LIMIT/OFFSET, grouping, HAVING, UNION, DISTINCT and aggregate
// expressions all come from one implementation:
//
//	subquery ─► subNeedsExecutor? ─no──► reduced runner (fast path, plan cache)
//	                 │ yes
//	                 ▼
//	         decorrelated body (cached per statement) ─► runSelectBody on a DB copy
//	         bound to the statement's transaction, with the outer row as the
//	         correlation scope (DB.outer → every root exEnv's outer)
//
// The simple shapes keep the runners because a correlated subquery
// re-runs per outer row, and the runners' plan cache rebinds scan bounds
// instead of re-planning; they answer those shapes correctly. An
// uncorrelated subquery on either path still runs once per statement
// (subMemo).

import (
	"github.com/rohanthewiz/serr"
)

// subNeedsExecutor reports whether a subquery has a shape the reduced
// runners do not implement. kind distinguishes the scalar runner, the
// only one that folds a plain aggregate item itself.
func subNeedsExecutor(sel *Select, kind subKind) bool {
	if len(sel.Union) > 0 || len(sel.GroupBy) > 0 || sel.Having != nil || len(sel.OrderBy) > 0 ||
		sel.Limit >= 0 || sel.Offset > 0 || sel.LimitParam != 0 || sel.OffsetParam != 0 {
		return true
	}
	for _, it := range sel.Items {
		if it.Agg != AggNone && kind != subScalar {
			return true
		}
		if it.Ex != nil && (findAgg(it.Ex) != AggNone || exprHasWindow(it.Ex)) {
			return true
		}
	}
	return false
}

// exprHasWindow reports whether an expression contains a window call.
func exprHasWindow(e Expr) bool {
	found := false
	walkExpr(e, func(sub Expr) bool {
		if _, ok := sub.(*ExWindow); ok {
			found = true
		}
		return !found
	})
	return found
}

// runSubSelect executes a subquery through the full SELECT executor,
// inside the statement's transaction, with env — the outer row — as the
// correlation scope for references the subquery's own FROM does not
// resolve.
func runSubSelect(env *exEnv, sel *Select) (*Result, error) {
	body, err := decorrelatedBody(env, sel)
	if err != nil {
		return nil, err
	}
	dd := *env.d
	dd.tx = env.tx
	dd.outer = env
	return dd.runSelectBody(body)
}

// decorrelatedBody returns sel with every reference to an enclosing
// query rewritten into a form the executor evaluates per row: a lowered
// predicate (WHERE, ON) naming an outer column becomes a Cond leaf, and
// a plain select-list or ORDER BY column becomes an expression item.
// The planner and projector resolve those shapes against the
// subquery's own scope; expressions fall back to the outer row at
// evaluation (exEnv.lookupVal). The rewrite depends only on the
// subquery and the catalog, so it is cached per statement.
func decorrelatedBody(env *exEnv, sel *Select) (*Select, error) {
	m := env.subMemo()
	if m != nil {
		if b, ok := m.bodies[sel]; ok {
			return b, nil
		}
	}
	body := decorrelateSelect(env, sel)
	if m != nil {
		if m.bodies == nil {
			m.bodies = map[*Select]*Select{}
		}
		m.bodies[sel] = body
	}
	return body, nil
}

func decorrelateSelect(env *exEnv, sel *Select) *Select {
	sc, err := buildScope(env.d.shapeLookup(env.tx.TableShared), sel.From)
	if err != nil {
		return sel // an unknown table: the executor reports it
	}
	c := *sel
	c.Where = decorrelate(sel.Where, sc)
	if len(sel.From) > 0 {
		c.From = append([]FromItem(nil), sel.From...)
		for k := range c.From {
			c.From[k].On = decorrelate(c.From[k].On, sc.prefix(k+1))
		}
	}
	outerCol := func(it SelectItem) bool {
		if it.Ex != nil || it.IsLit || it.Star || it.Agg != AggNone {
			return false
		}
		_, err := sc.resolve(it.Col)
		return err != nil
	}
	for i, it := range sel.Items {
		if outerCol(it) {
			if c.Items == nil || &c.Items[0] == &sel.Items[0] {
				c.Items = append([]SelectItem(nil), sel.Items...)
			}
			c.Items[i].Ex = &ExCol{Col: it.Col}
		}
	}
	for i, o := range sel.OrderBy {
		if outerCol(o.SelectItem) {
			if &c.OrderBy[0] == &sel.OrderBy[0] {
				c.OrderBy = append([]OrderItem(nil), sel.OrderBy...)
			}
			c.OrderBy[i].Ex = &ExCol{Col: o.Col}
		}
	}
	if len(sel.Union) > 0 {
		c.Union = append([]UnionArm(nil), sel.Union...)
		for i := range c.Union {
			c.Union[i].Sel = decorrelateSelect(env, c.Union[i].Sel)
		}
	}
	return &c
}

// oneColumn checks a subquery result's width for a context that takes a
// single column.
func oneColumn(res *Result, what string) error {
	if len(res.Cols) != 1 {
		return serr.New(what + " must select exactly one column")
	}
	return nil
}

// subColumnValues is a one-column result's values, in result order.
func subColumnValues(res *Result) []any {
	vals := make([]any, len(res.Rows))
	for i, r := range res.Rows {
		vals[i] = r[0]
	}
	return vals
}

// scalarFromExecutor runs a scalar subquery through the executor: no
// row is NULL, one row is its value, more is an error.
func scalarFromExecutor(env *exEnv, sel *Select) (any, error) {
	res, err := runSubSelect(env, sel)
	if err != nil {
		return nil, err
	}
	if err := oneColumn(res, "a scalar subquery"); err != nil {
		return nil, err
	}
	switch len(res.Rows) {
	case 0:
		return nil, nil
	case 1:
		return res.Rows[0][0], nil
	}
	return nil, serr.New("more than one row returned by a scalar subquery")
}

// subSet is an uncorrelated IN-subquery's values as a hash set, built
// once per statement (deep-look D-28): x IN (SELECT ...) used to walk
// the memoized value list for every outer row — 449 ms for 100k outer
// rows against 500 values, beside a 30 ms scan.
type subSet struct {
	keys    map[string]struct{}
	hasNull bool
	// class is the one value kind every non-NULL element has; usable is
	// false when the elements mix kinds (or one cannot be keyed), in
	// which case comparisons need checkPred's coercion and the set is
	// not used.
	class  valueClass
	usable bool
}

// valueClass groups the runtime kinds that compare by equality of their
// canonical hash key: numbers (int64 and float64 together, as hashKey
// folds them), strings, booleans and byte strings.
type valueClass int

const (
	classNone valueClass = iota
	classNumber
	classString
	classBool
	classBytes
)

func classOf(v any) valueClass {
	switch v.(type) {
	case int64, float64:
		return classNumber
	case string:
		return classString
	case bool:
		return classBool
	case []byte:
		return classBytes
	}
	return classNone
}

// inSubquerySet answers x IN (sel) from a memoized hash set when sel is
// uncorrelated and its values are hash-comparable with x. handled is
// false when the set cannot decide, and the caller falls back to the
// comparison loop. Three-valued like the loop: a match is true; no match
// is NULL when the set holds a NULL (x = NULL is unknown) or x is NULL,
// and false otherwise; IN over no rows is false even for a NULL x.
func inSubquerySet(env *exEnv, sel *Select, x any) (result any, handled bool, err error) {
	m := env.subMemo()
	if m == nil || !m.uncorrelated(env, sel) {
		return nil, false, nil
	}
	set, ok := m.sets[sel]
	if !ok {
		vals, err := collectSubColumn(env, sel)
		if err != nil {
			return nil, true, err
		}
		set = buildSubSet(vals)
		if m.sets == nil {
			m.sets = map[*Select]*subSet{}
		}
		m.sets[sel] = set
	}
	if !set.usable {
		return nil, false, nil
	}
	if len(set.keys) == 0 && !set.hasNull {
		return false, true, nil
	}
	if x == nil {
		return nil, true, nil
	}
	if classOf(x) != set.class && len(set.keys) > 0 {
		return nil, false, nil // needs coercion: let the loop decide
	}
	k, err := hashKey(x)
	if err != nil {
		return nil, false, nil
	}
	if _, hit := set.keys[string(k)]; hit {
		return true, true, nil
	}
	if set.hasNull {
		return nil, true, nil
	}
	return false, true, nil
}

func buildSubSet(vals []any) *subSet {
	set := &subSet{keys: make(map[string]struct{}, len(vals)), usable: true}
	for _, v := range vals {
		if v == nil {
			set.hasNull = true
			continue
		}
		c := classOf(v)
		if c == classNone || (set.class != classNone && c != set.class) {
			set.usable = false
			return set
		}
		set.class = c
		k, err := hashKey(v)
		if err != nil {
			set.usable = false
			return set
		}
		set.keys[string(k)] = struct{}{}
	}
	return set
}
