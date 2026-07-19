package sql

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"iter"
	"math"
	"slices"
	"time"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// cancelEvery is how many scanned rows pass between cancellation
// polls. Polling costs a mutex under the hood, so per-row would tax
// the in-memory scan rate for nothing; at this stride even a
// million-row-per-second scan notices a cancel within a millisecond.
const cancelEvery = 256

// scanPlan yields the rows a plan matches, in the chosen path's key
// order, within tx's snapshot. env supplies the evaluation
// environment for any Cond leaves in the residual filter (nil when
// the filter is known to hold none). yield returning false stops the
// scan.
//
// ctx (nil for uncancellable execution) is the statement's
// cancellation scope: it is polled at scan start — which alone bounds
// correlated-subquery and inner-join-loop storms, since those re-enter
// here per outer row — and every cancelEvery rows within one scan.
func scanPlan(ctx context.Context, tx *bytdb.Txn, table string, p *plan, env *exEnv, yield func(bytdb.Row) bool) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return serr.Wrap(err, "state", "statement canceled")
		}
	}
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
	n := 0
	for row, err := range seq {
		if err != nil {
			return err
		}
		if n++; ctx != nil && n%cancelEvery == 0 {
			if err := ctx.Err(); err != nil {
				return serr.Wrap(err, "state", "statement canceled")
			}
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
	case OpRegex, OpNotRegex, OpRegexI, OpNotRegexI,
		OpLike, OpNotLike, OpILike, OpNotILike:
		if v == nil || lit == nil {
			return triUnknown, nil
		}
		s, okS := v.(string)
		pat, okP := lit.(string)
		if !okS || !okP {
			return triUnknown, serr.New("pattern match requires text operands")
		}
		// LIKE patterns become regexes first; both families then share
		// one compile cache and match path. Case-insensitivity is a
		// compile flag, not a pattern rewrite.
		if op == OpLike || op == OpNotLike || op == OpILike || op == OpNotILike {
			var err error
			if pat, err = likeRegex(pat); err != nil {
				return triUnknown, err
			}
		}
		insensitive := op == OpRegexI || op == OpNotRegexI || op == OpILike || op == OpNotILike
		re, err := compileRegex(pat, insensitive)
		if err != nil {
			return triUnknown, err
		}
		hit := re.MatchString(s)
		switch op {
		case OpNotRegex, OpNotRegexI, OpNotLike, OpNotILike:
			hit = !hit
		}
		if hit {
			return triTrue, nil
		}
		return triFalse, nil
	case OpContains, OpContainedBy, OpKeyExists, OpKeyExistsAny, OpKeyExistsAll:
		if v == nil || lit == nil {
			return triUnknown, nil
		}
		// Both sides are text at runtime: the left a jsonb document's
		// canonical form, the right a jsonb document (@> <@), a key (?),
		// or a '{...}' key list (?| ?&). jsonbPredicate parses and
		// reports malformed operands in Postgres's words.
		doc, okD := v.(string)
		arg, okA := lit.(string)
		if !okD || !okA {
			return triUnknown, serr.New("jsonb operator requires text operands",
				"op", opText(op))
		}
		hit, err := jsonbPredicate(op, doc, arg)
		if err != nil {
			return triUnknown, err
		}
		if hit {
			return triTrue, nil
		}
		return triFalse, nil
	}
	// A bound time.Time compares as timestamp micros — the dynamic
	// mirror of coerceLit's conversion, for placeholders that end up in
	// expression leaves the static pass never types.
	if tv, ok := lit.(time.Time); ok {
		lit = tv.UnixMicro()
	}
	if tv, ok := v.(time.Time); ok {
		v = tv.UnixMicro()
	}
	if v != nil && lit != nil {
		if _, ok := compareVals(v, lit); !ok {
			if _, isS := v.(string); isS {
				// A text column (or text expression) against a typed
				// non-string value. Postgres refuses this outright
				// ("operator does not exist: text < integer") and so do
				// we: silently parsing the *stored* text per row would
				// make `text_col < 5` match or skip rows by whether each
				// value happens to parse — data-dependent semantics, not
				// a type rule. Note the asymmetry with the branch below:
				// a string *literal* is untyped and adapts to the column
				// (Postgres quoted-literal semantics), but a string
				// *column value* is typed text and never converts.
				return triUnknown, serr.New(fmt.Sprintf(
					"operator does not exist: text %s %s", opText(op), kindOf(lit)))
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

// retProj is a resolved RETURNING clause: the projection over the
// target table's row shape, ready to map each affected row to an
// output row. Built inside the statement's transaction so the column
// set matches the descriptor the writes run against.
type retProj struct {
	env   *exEnv
	proj  []projEntry
	exprs []Expr
}

// prepareReturning resolves a DML statement's RETURNING clause against
// its target table, filling res.Cols and res.Types. A statement
// without the clause resolves to nil — the executor's signal to skip
// row collection entirely.
func (d *DB) prepareReturning(tx *bytdb.Txn, table string, desc *bytdb.TableDesc, r *Returning, res *Result) (*retProj, error) {
	if !r.hasReturning() {
		return nil, nil
	}
	env := d.tableEnv(tx, table, desc)
	proj, exprs, err := projectSelect(env.sc, r.retSelect(), res)
	if err != nil {
		return nil, err
	}
	return &retProj{env: env, proj: proj, exprs: exprs}, nil
}

// row projects one affected row (full values in declared column order)
// to a RETURNING output row, evaluating expression items against it —
// the same combined-row convention as execSelect: expression results
// append past the table's width, where the projection ordinals expect
// them.
func (rp *retProj) row(vals []any) ([]any, error) {
	combined := vals
	if len(rp.exprs) > 0 {
		combined = slices.Clone(vals)
		for _, ex := range rp.exprs {
			rowEnv := *rp.env
			rowEnv.row = vals
			v, err := evalEx(&rowEnv, ex)
			if err != nil {
				return nil, err
			}
			combined = append(combined, v)
		}
	}
	out := make([]any, len(rp.proj))
	for j, pe := range rp.proj {
		if pe.ord < 0 {
			out[j] = pe.lit
		} else {
			out[j] = combined[pe.ord]
		}
	}
	return out, nil
}

// columnDefaults parses each column's stored DEFAULT into its
// insert-ready value, coerced to the column type; nil entries mean
// no default (insert NULL). ExprDefault markers evaluate here, which
// is once per INSERT statement — every row of a multi-row insert
// (and every now() column) shares the one instant, the statement-
// granular cousin of Postgres freezing now() per transaction. The
// stored marker text is unquoted, so it can never collide with a
// constant string default (renderLit quotes those).
func columnDefaults(desc *bytdb.TableDesc) ([]any, error) {
	out := make([]any, len(desc.Columns))
	now := time.Now().UTC()
	for i := range desc.Columns {
		c := &desc.Columns[i]
		if c.Default == "" {
			continue
		}
		var v any
		var err error
		switch c.Default {
		case string(DefaultNow):
			v = now
		case string(DefaultCurrentDate):
			// Truncate before coercing: on a timestamp column the date
			// must land as midnight UTC, not the current instant.
			v = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		default:
			if v, err = parseStoredLiteral(c.Default); err != nil {
				return nil, serr.Wrap(err, "op", "parse column default", "column", c.Name)
			}
		}
		if out[i], err = coerceLit(v, c.Type); err != nil {
			return nil, serr.Wrap(err, "op", "parse column default", "column", c.Name)
		}
	}
	return out, nil
}

// resolveDefaultMarkers replaces DEFAULT-keyword values with the
// column's default (NULL without one). Full-arity rows alias the
// statement's parsed AST, so a row carrying a marker is cloned before
// the write — prepared statements re-execute against the original.
func resolveDefaultMarkers(vals, defaults []any) []any {
	cloned := false
	for i, v := range vals {
		if _, ok := v.(defaultMarker); !ok {
			continue
		}
		if !cloned {
			vals, cloned = slices.Clone(vals), true
		}
		if i < len(defaults) { // arity mismatches error at the engine
			vals[i] = defaults[i]
		} else {
			vals[i] = nil
		}
	}
	return vals
}

// resolveExprValues evaluates each expression value in an INSERT row
// (nextval('s'), 1+2, a scalar subquery) down to a plain value the
// engine can store. VALUES has no input row, so env carries an empty
// scope: a column reference fails with "no such column", as it should.
// Like resolveDefaultMarkers, the slice may alias the parsed AST, so
// it is cloned before the first in-place write — prepared statements
// re-execute (and re-evaluate volatile calls like nextval) against
// the original tree.
func resolveExprValues(env *exEnv, vals []any) ([]any, error) {
	cloned := false
	for i, v := range vals {
		ex, ok := v.(Expr)
		if !ok {
			continue
		}
		out, err := evalEx(env, ex)
		if err != nil {
			return nil, err
		}
		if !cloned {
			vals, cloned = slices.Clone(vals), true
		}
		vals[i] = out
	}
	return vals, nil
}

func (d *DB) execInsert(s *Insert) (*Result, error) {
	res := &Result{}
	affected := 0
	err := d.write(func(tx *bytdb.Txn) error {
		desc := tx.Table(s.Table)
		if desc == nil {
			return serr.New("no such table", "table", s.Table)
		}
		ret, err := d.prepareReturning(tx, s.Table, desc, &s.Returning, res)
		if err != nil {
			return err
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
		defaults, err := columnDefaults(desc)
		if err != nil {
			return err
		}
		up, err := d.prepareUpsert(tx, s.Table, desc, s.Conflict, checks)
		if err != nil {
			return err
		}
		var env *exEnv
		if len(checks) > 0 {
			env = d.tableEnv(tx, s.Table, desc)
		}
		// The environment expression values evaluate in: transaction and
		// DB for nextval/subqueries, an empty scope so column references
		// fail. Built once; VALUES expressions never read a row.
		venv := &exEnv{d: d, tx: tx, sc: &scope{}}
		for _, row := range s.Rows {
			vals := row
			// s.Cols (not ords) is the column-list signal: DEFAULT VALUES
			// parses as an empty list, where ords stays nil too.
			if s.Cols != nil {
				if len(row) != len(ords) {
					return serr.New("INSERT row has wrong number of values", "table", s.Table)
				}
				// Unnamed columns take their defaults (nil entries are the
				// old insert-as-NULL behavior).
				vals = slices.Clone(defaults)
				for i, ord := range ords {
					vals[ord] = row[i]
				}
			}
			vals = resolveDefaultMarkers(vals, defaults)
			if vals, err = resolveExprValues(venv, vals); err != nil {
				return err
			}
			// Arity mismatches skip the conflict probe too and fall
			// through to the engine's error.
			if up != nil && len(vals) == len(desc.Columns) {
				existing, hit, err := up.conflict(tx, vals)
				if err != nil {
					return err
				}
				if hit {
					if !s.Conflict.Update {
						continue // DO NOTHING: not counted, not returned
					}
					stored, updated, err := up.resolve(tx, existing, vals)
					if err != nil {
						return err
					}
					if !updated { // DO UPDATE ... WHERE filtered the pair out
						continue
					}
					if len(desc.ForeignKeys) > 0 {
						if err := d.fkVerifyRow(tx, desc, stored.Vals); err != nil {
							return err
						}
					}
					affected++
					if ret != nil {
						out, err := ret.row(stored.Vals)
						if err != nil {
							return err
						}
						res.Rows = append(res.Rows, out)
					}
					continue
				}
			}
			if env != nil && len(vals) == len(desc.Columns) {
				if err := checkRow(env, s.Table, checks, vals); err != nil {
					return err
				}
			}
			// The engine hands back the row as stored — identity columns
			// filled, values coerced — which is the only correct input for
			// RETURNING: the SQL layer never sees a drawn identity value
			// otherwise.
			stored, err := tx.InsertReturning(s.Table, vals...)
			if err != nil {
				return err
			}
			// Foreign keys check the row as stored, after the write: the
			// transaction discards it on error, and checking post-insert is
			// what lets a self-referencing row (a node that is its own
			// parent) go in.
			if len(desc.ForeignKeys) > 0 {
				if err := d.fkVerifyRow(tx, desc, stored.Vals); err != nil {
					return err
				}
			}
			affected++
			// A NULL in an identity column meant a draw: record it for
			// lastval()/currval(). An explicit value is not a draw.
			for i := range desc.Columns {
				if c := &desc.Columns[i]; c.Identity && i < len(vals) && vals[i] == nil {
					if v, ok := stored.Vals[i].(int64); ok {
						d.seq.record(identitySeqName(desc.Name, c.Name), v)
					}
				}
			}
			if up != nil {
				if err := up.markInserted(stored.Vals); err != nil {
					return err
				}
			}
			if ret != nil {
				out, err := ret.row(stored.Vals)
				if err != nil {
					return err
				}
				res.Rows = append(res.Rows, out)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.RowsAffected = affected
	return res, nil
}

func (d *DB) execUpdate(s *Update) (*Result, error) {
	res := &Result{}
	affected := 0
	err := d.write(func(tx *bytdb.Txn) error {
		desc := tx.Table(s.Table)
		if desc == nil {
			return serr.New("no such table", "table", s.Table)
		}
		ret, err := d.prepareReturning(tx, s.Table, desc, &s.Returning, res)
		if err != nil {
			return err
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
		for col := range s.SetEx {
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
		err = scanPlan(d.ctx, tx, s.Table, pl, env, func(r bytdb.Row) bool {
			rows = append(rows, r.Vals)
			return true
		})
		if err != nil {
			return err
		}
		// Inbound foreign keys: if any other table references this one,
		// old row images whose referenced columns change must be
		// re-checked once the statement's writes are in (NO ACTION is an
		// end-of-statement check, so intra-statement key shuffles work).
		refs, err := tx.ReferencingFKs(s.Table)
		if err != nil {
			return err
		}
		var refRecheck [][]any
		for _, rv := range rows {
			// SET expressions evaluate against the pre-update row (SET age
			// = age + 1 reads the old age; Postgres semantics), then coerce
			// string results to the column type exactly like a quoted
			// literal would. The merged map starts from the uncoerced
			// literal Set — tx.Update applies its own coercion to those,
			// preserving the literal-only path's behavior byte for byte.
			set := s.Set
			if len(s.SetEx) > 0 {
				set = make(map[string]any, len(s.Set)+len(s.SetEx))
				for col, v := range s.Set {
					set[col] = v
				}
				env.row = rv
				for col, ex := range s.SetEx {
					v, err := evalEx(env, ex)
					if err != nil {
						return err
					}
					cv, err := coerceLit(v, desc.Columns[setOrds[col]].Type)
					if err != nil {
						return serr.Wrap(err, "table", s.Table, "column", col)
					}
					set[col] = cv
				}
			}
			if len(checks) > 0 {
				nv := slices.Clone(rv)
				for col, v := range setVals {
					nv[setOrds[col]] = v
				}
				// Expression results are already evaluated and coerced in
				// the merged map; the check row must judge those values,
				// not the expressions' raw text.
				for col := range s.SetEx {
					nv[setOrds[col]] = set[col]
				}
				if err := checkRow(env, s.Table, checks, nv); err != nil {
					return err
				}
			}
			pk := make([]any, len(desc.PKCols))
			for i, ord := range desc.PKCols {
				pk[i] = rv[ord]
			}
			// RETURNING reports the engine's post-update row rather than a
			// SQL-layer reconstruction: the engine applies its own coercion
			// (e.g. an int assigned to a float column stores as float), and
			// only its output is guaranteed to match what a later read sees.
			stored, ok, err := tx.UpdateReturning(s.Table, pk, set)
			if err != nil {
				return err
			}
			if ok {
				if len(desc.ForeignKeys) > 0 {
					// Child side: the row's own references must still exist.
					if err := d.fkVerifyRow(tx, desc, stored.Vals); err != nil {
						return err
					}
				}
				if len(refs) > 0 && fkRefColsChanged(desc, refs, rv, stored.Vals) {
					refRecheck = append(refRecheck, rv)
				}
				affected++
				if ret != nil {
					out, err := ret.row(stored.Vals)
					if err != nil {
						return err
					}
					res.Rows = append(res.Rows, out)
				}
			}
		}
		for _, old := range refRecheck {
			if err := d.fkVerifyReferenced(tx, desc, refs, old); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.RowsAffected = affected
	return res, nil
}

func (d *DB) execDelete(s *Delete) (*Result, error) {
	res := &Result{}
	affected := 0
	err := d.write(func(tx *bytdb.Txn) error {
		desc := tx.Table(s.Table)
		if desc == nil {
			return serr.New("no such table", "table", s.Table)
		}
		ret, err := d.prepareReturning(tx, s.Table, desc, &s.Returning, res)
		if err != nil {
			return err
		}
		pl, err := planScan(desc, s.Table, s.Where)
		if err != nil {
			return err
		}
		refs, err := tx.ReferencingFKs(s.Table)
		if err != nil {
			return err
		}
		// RETURNING reports each row as it was before its delete, so the
		// scan keeps the full rows alongside the keys (they are decoded
		// already; keeping them costs only the reference). Inbound
		// foreign keys need the old images too: cascades and the
		// referenced check both run after all deletes (fkAfterDelete),
		// so deleting a parent together with its children (or a
		// self-referencing row) in one statement is legal.
		keepRows := ret != nil || len(refs) > 0
		var rows [][]any
		pks, err := collectPKs(d.ctx, tx, s.Table, desc, pl, d.tableEnv(tx, s.Table, desc), func(vals []any) {
			if keepRows {
				rows = append(rows, vals)
			}
		})
		if err != nil {
			return err
		}
		for i, pk := range pks {
			ok, err := tx.Delete(s.Table, pk...)
			if err != nil {
				return err
			}
			if ok {
				affected++
				if ret != nil {
					out, err := ret.row(rows[i])
					if err != nil {
						return err
					}
					res.Rows = append(res.Rows, out)
				}
			}
		}
		if len(refs) > 0 {
			if err := d.fkAfterDelete(tx, desc, refs, rows); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.RowsAffected = affected
	return res, nil
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

// collectPKs materializes the primary keys of every row the plan
// matches, calling keep with each full row along the way (DELETE ...
// RETURNING retains them; a plain DELETE's keep is a no-op).
func collectPKs(ctx context.Context, tx *bytdb.Txn, table string, desc *bytdb.TableDesc, pl *plan, env *exEnv, keep func([]any)) ([][]any, error) {
	var pks [][]any
	err := scanPlan(ctx, tx, table, pl, env, func(r bytdb.Row) bool {
		pk := make([]any, len(desc.PKCols))
		for i, ord := range desc.PKCols {
			pk[i] = r.Vals[ord]
		}
		pks = append(pks, pk)
		keep(r.Vals)
		return true
	})
	return pks, err
}

// runSelectCore executes one UNION arm (or a whole plain SELECT).
func (d *DB) runSelectCore(s *Select) (*Result, error) {
	if s.Distinct {
		return d.execSelectDistinct(s)
	}
	if s.isAggregate() {
		if hasWindow(s) {
			return d.execSelectAggWindow(s)
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
	keys, err := outputSortKeys(s.OrderBy, res.Cols,
		"ORDER BY on a UNION must name an output column",
		"ORDER BY on a UNION takes output columns or positions")
	if err != nil {
		return nil, err
	}
	res.Rows = sortLimitRows(rows, keys, s.Offset, s.Limit)
	res.RowsAffected = 0
	return res, nil
}

// execSelectDistinct runs a SELECT DISTINCT of any shape (plain,
// aggregate, windowed): the core executes with ORDER BY / OFFSET /
// LIMIT stripped, the projected rows dedup, and the trimmings apply to
// the deduped set — the same post-materialization treatment a UNION
// gets, because both operate on output rows, not table rows.
//
//	core rows ──dedup──▶ distinct rows ──sort──▶ OFFSET/LIMIT
//
// ORDER BY is therefore restricted to output columns and positions
// (Postgres's rule, for the same reason: a sort key the projection
// dropped would decide which duplicate survives, invisibly).
func (d *DB) execSelectDistinct(s *Select) (*Result, error) {
	core := *s
	core.Distinct = false
	core.OrderBy, core.Limit, core.Offset = nil, -1, 0
	res, err := d.runSelectCore(&core)
	if err != nil {
		return nil, err
	}
	rows, err := dedupRows(res.Rows)
	if err != nil {
		return nil, err
	}
	notInList := "for SELECT DISTINCT, ORDER BY expressions must appear in select list"
	keys, err := outputSortKeys(s.OrderBy, res.Cols, notInList, notInList)
	if err != nil {
		return nil, err
	}
	res.Rows = sortLimitRows(rows, keys, s.Offset, s.Limit)
	return res, nil
}

// outputSortKeys resolves ORDER BY items against a materialized
// result's output columns: select-list positions (ORDER BY n) or bare
// output column names. Anything richer — expressions, aggregates,
// qualified names — has nothing to resolve against once rows are
// projected, and errors with the caller's wording (errName for an
// unmatched bare name, errShape for the rest).
func outputSortKeys(order []OrderItem, cols []string, errName, errShape string) ([]sortKey, error) {
	var keys []sortKey
	for _, o := range order {
		switch {
		case o.IsLit:
			n, ok := o.Lit.(int64)
			if !ok {
				return nil, serr.New("non-integer constant in ORDER BY")
			}
			if n < 1 || int(n) > len(cols) {
				return nil, serr.New("ORDER BY position is not in the select list",
					"position", fmt.Sprint(n))
			}
			keys = append(keys, sortKey{int(n - 1), o.Desc})
		case o.Ex == nil && o.Agg == AggNone && o.Col.Table == "":
			found := -1
			for i, c := range cols {
				if c == o.Col.Name {
					found = i
					break
				}
			}
			if found < 0 {
				return nil, serr.New(errName, "column", o.Col.Name)
			}
			keys = append(keys, sortKey{found, o.Desc})
		default:
			return nil, serr.New(errShape)
		}
	}
	return keys, nil
}

// sortLimitRows finishes a materialized (already projected) result:
// stable sort by the output-column keys, then OFFSET, then LIMIT.
func sortLimitRows(rows [][]any, keys []sortKey, offset, limit int64) [][]any {
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
	return rows
}

// dedupRows keeps each distinct row's first occurrence, compared by
// the order-preserving tuple encoding (NULLs equal NULLs, as UNION
// and SELECT DISTINCT both require).
func dedupRows(rows [][]any) ([][]any, error) {
	seen := make(map[string]bool, len(rows))
	out := rows[:0]
	for _, r := range rows {
		kb, err := tuple.Encode(r...)
		if err != nil {
			return nil, serr.Wrap(err, "op", "encode row for dedup")
		}
		if seen[string(kb)] {
			continue
		}
		seen[string(kb)] = true
		out = append(out, r)
	}
	return out, nil
}

// execTruncate deletes every row of each named table — one atomic
// write transaction for the whole statement, so a failure on the
// third table leaves the first two intact.
func (d *DB) execTruncate(s *Truncate) (*Result, error) {
	for _, t := range s.Tables {
		// run()'s single-target guard can't cover a table list, so the
		// system-catalog check runs here, per table.
		if err := sysWriteGuard(t); err != nil {
			return nil, err
		}
	}
	err := d.write(func(tx *bytdb.Txn) error {
		inList := map[string]bool{}
		for _, t := range s.Tables {
			inList[t] = true
		}
		for _, t := range s.Tables {
			// A referenced table can only truncate together with every
			// table referencing it — otherwise child rows would dangle.
			// Postgres's rule and wording (it demands CASCADE or listing
			// the children; bytdb has no cascades, so: list them).
			refs, err := tx.ReferencingFKs(t)
			if err != nil {
				return err
			}
			for _, r := range refs {
				if !inList[r.Child.Name] {
					return serr.New(`cannot truncate a table referenced in a foreign key constraint`,
						"table", t, "referencing_table", r.Child.Name, "constraint", r.FK.Name,
						"hint", `TRUNCATE `+t+`, `+r.Child.Name)
				}
			}
		}
		for _, t := range s.Tables {
			if err := tx.Truncate(t, s.RestartIdentity); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Result{}, nil
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
