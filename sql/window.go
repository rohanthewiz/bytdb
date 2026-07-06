package sql

import (
	"slices"
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// window.go: window functions — fn(...) OVER (PARTITION BY ... ORDER BY
// ...). Unlike aggregates, windows emit every input row, so the query
// materializes all post-filter rows, partitions them by the
// order-preserving tuple encoding (the same equivalence GROUP BY uses),
// sorts each partition by the window ORDER BY, and assigns a per-row
// value. Ranking functions (ROW_NUMBER/RANK/DENSE_RANK) and unframed
// aggregate windows are supported; explicit ROWS/RANGE frames are not.

// hasWindow reports whether the query uses any window function in its
// select list or ORDER BY.
func hasWindow(s *Select) bool {
	found := false
	visit := func(e Expr) bool {
		if _, ok := e.(*ExWindow); ok {
			found = true
		}
		return !found
	}
	for _, it := range s.Items {
		walkExpr(it.Ex, visit)
	}
	for _, o := range s.OrderBy {
		walkExpr(o.Ex, visit)
	}
	return found
}

// resultType is the type of a window function's output column.
func (w *ExWindow) resultType(sc *scope) bytdb.ColType {
	if w.Win != WinNone {
		return bytdb.TInt // row_number / rank / dense_rank
	}
	switch w.Agg {
	case AggCount:
		return bytdb.TInt
	case AggAvg:
		return bytdb.TFloat
	}
	if w.Arg != nil {
		return exprType(sc, w.Arg)
	}
	return bytdb.TInt
}

// windowText renders a window call for EXPLAIN.
func windowText(w *ExWindow) string {
	var b strings.Builder
	b.WriteString(w.fnName())
	b.WriteByte('(')
	switch {
	case w.Star:
		b.WriteByte('*')
	case w.Arg != nil:
		b.WriteString(exprText(w.Arg))
	}
	b.WriteString(") OVER (")
	sep := ""
	if len(w.Partition) > 0 {
		txts := make([]string, len(w.Partition))
		for i, e := range w.Partition {
			txts[i] = exprText(e)
		}
		b.WriteString("PARTITION BY " + strings.Join(txts, ", "))
		sep = " "
	}
	if len(w.OrderBy) > 0 {
		txts := make([]string, len(w.OrderBy))
		for i, o := range w.OrderBy {
			txts[i] = exprText(o.Ex)
			if o.Desc {
				txts[i] += " DESC"
			}
		}
		b.WriteString(sep + "ORDER BY " + strings.Join(txts, ", "))
	}
	b.WriteByte(')')
	return b.String()
}

// rewriteWindows returns a copy of e with each ExWindow replaced by an
// exWinRef placeholder, appending the windows found to *out in order.
// It mirrors bindExpr's container coverage; a window nested somewhere
// not handled here survives to the evaluator, which reports it clearly.
func rewriteWindows(e Expr, sc *scope, out *[]*ExWindow) Expr {
	rw := func(x Expr) Expr { return rewriteWindows(x, sc, out) }
	switch n := e.(type) {
	case *ExWindow:
		*out = append(*out, n)
		return &exWinRef{idx: len(*out) - 1, typ: n.resultType(sc), name: n.fnName()}
	case *ExAnd:
		c := &ExAnd{Exprs: make([]Expr, len(n.Exprs))}
		for i, s := range n.Exprs {
			c.Exprs[i] = rw(s)
		}
		return c
	case *ExOr:
		c := &ExOr{Exprs: make([]Expr, len(n.Exprs))}
		for i, s := range n.Exprs {
			c.Exprs[i] = rw(s)
		}
		return c
	case *ExNot:
		return &ExNot{E: rw(n.E)}
	case *ExCmp:
		return &ExCmp{Op: n.Op, L: rw(n.L), R: rw(n.R)}
	case *ExAny:
		return &ExAny{Op: n.Op, L: rw(n.L), R: rw(n.R), All: n.All}
	case *ExIsNull:
		return &ExIsNull{E: rw(n.E), Not: n.Not}
	case *ExIn:
		c := &ExIn{E: rw(n.E), Not: n.Not, List: make([]Expr, len(n.List))}
		for i, s := range n.List {
			c.List[i] = rw(s)
		}
		return c
	case *ExCase:
		c := &ExCase{Operand: rw(n.Operand), Else: rw(n.Else), Whens: make([]ExWhen, len(n.Whens))}
		for i, w := range n.Whens {
			c.Whens[i] = ExWhen{When: rw(w.When), Then: rw(w.Then)}
		}
		return c
	case *ExFunc:
		c := &ExFunc{Name: n.Name, Args: make([]Expr, len(n.Args))}
		for i, a := range n.Args {
			c.Args[i] = rw(a)
		}
		return c
	case *ExCast:
		return &ExCast{E: rw(n.E), Type: n.Type}
	case *ExIndex:
		return &ExIndex{E: rw(n.E), Idx: rw(n.Idx)}
	case *ExArray:
		c := &ExArray{Elems: make([]Expr, len(n.Elems))}
		for i, el := range n.Elems {
			c.Elems[i] = rw(el)
		}
		return c
	case *ExArith:
		return &ExArith{Op: n.Op, L: rw(n.L), R: rw(n.R)}
	}
	return e
}

func (d *DB) execSelectWindow(s *Select) (*Result, error) {
	res := &Result{}
	var full [][]any     // combined rows (base columns + evaluated exprs)
	var proj []projEntry // projection into the combined rows
	var keys []sortKey
	err := d.read(func(tx *bytdb.Txn) error {
		fp, err := prepareFrom(d.lookup(tx.Table), s.From, s.Where)
		if err != nil {
			return err
		}
		sc := fp.sc
		env := &exEnv{d: d, tx: tx, sc: sc}

		// Materialize the base (post-filter, post-join) rows; only the
		// join columns are kept — window values and select expressions
		// are computed after all rows are known.
		var base [][]any
		if err = runJoin(tx, fp, env, func(vals []any) bool {
			base = append(base, append([]any(nil), vals[:sc.width]...))
			return true
		}); err != nil {
			return err
		}

		// Rewrite window calls in a shallow copy of the select list and
		// ORDER BY into exWinRef placeholders, collecting the windows.
		var wins []*ExWindow
		s2 := *s
		s2.Items = make([]SelectItem, len(s.Items))
		for i, it := range s.Items {
			if it.Ex != nil {
				it.Ex = rewriteWindows(it.Ex, sc, &wins)
			}
			s2.Items[i] = it
		}
		s2.OrderBy = make([]OrderItem, len(s.OrderBy))
		for i, o := range s.OrderBy {
			if o.Ex != nil {
				o.Ex = rewriteWindows(o.Ex, sc, &wins)
			}
			s2.OrderBy[i] = o
		}

		// Compute every window's value for every row.
		winvals := make([][]any, len(base))
		for i := range winvals {
			winvals[i] = make([]any, len(wins))
		}
		for wi, w := range wins {
			if err = computeWindow(env, sc, base, w, wi, winvals); err != nil {
				return err
			}
		}

		// Project the rewritten select list and ORDER BY, then build the
		// combined rows: base columns plus each expression evaluated with
		// the row's window values visible through env.win.
		var exprs []Expr
		if proj, exprs, err = projectSelect(sc, &s2, res); err != nil {
			return err
		}
		if keys, exprs, err = buildSortKeys(sc, &s2, proj, exprs); err != nil {
			return err
		}
		full = make([][]any, len(base))
		for i, row := range base {
			rowEnv := *env
			rowEnv.row = row
			rowEnv.win = winvals[i]
			combined := row
			for _, ex := range exprs {
				v, e := evalEx(&rowEnv, ex)
				if e != nil {
					return e
				}
				combined = append(combined, v)
			}
			full[i] = combined
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortOffsetProject(res, full, keys, proj, s.Offset, s.Limit)
	return res, nil
}

// computeWindow fills column wi of winvals with one window's per-row
// values: partition, sort each partition by the window ORDER BY, then
// assign.
func computeWindow(env *exEnv, sc *scope, base [][]any, w *ExWindow, wi int, winvals [][]any) error {
	parts := map[string][]int{}
	var order []string // first-seen partition order
	for i, row := range base {
		key, err := partitionKey(env, w.Partition, row)
		if err != nil {
			return err
		}
		if _, ok := parts[key]; !ok {
			order = append(order, key)
		}
		parts[key] = append(parts[key], i)
	}
	for _, key := range order {
		idxs := parts[key]
		if len(w.OrderBy) > 0 {
			if err := sortPartition(env, w.OrderBy, base, idxs); err != nil {
				return err
			}
		}
		if err := assignWindow(env, sc, w, wi, base, idxs, winvals); err != nil {
			return err
		}
	}
	return nil
}

// partitionKey encodes a row's PARTITION BY values into a map key with
// the order-preserving tuple encoding (NULLs partition together). No
// PARTITION BY means a single partition.
func partitionKey(env *exEnv, part []Expr, row []any) (string, error) {
	if len(part) == 0 {
		return "", nil
	}
	vals, err := evalRow(env, part, row)
	if err != nil {
		return "", err
	}
	kb, err := tuple.Encode(vals...)
	if err != nil {
		return "", serr.Wrap(err, "op", "encode window partition key")
	}
	return string(kb), nil
}

// sortPartition stably orders a partition's row indices by the window
// ORDER BY.
func sortPartition(env *exEnv, ob []OrderItem, base [][]any, idxs []int) error {
	ov := make(map[int][]any, len(idxs))
	for _, ri := range idxs {
		v, err := orderVals(env, ob, base[ri])
		if err != nil {
			return err
		}
		ov[ri] = v
	}
	slices.SortStableFunc(idxs, func(a, b int) int {
		for k, o := range ob {
			c := orderCmp(ov[a][k], ov[b][k])
			if o.Desc {
				c = -c
			}
			if c != 0 {
				return c
			}
		}
		return 0
	})
	return nil
}

// assignWindow writes one window's value for each row of a single,
// already-sorted partition.
func assignWindow(env *exEnv, sc *scope, w *ExWindow, wi int, base [][]any, idxs []int, winvals [][]any) error {
	switch w.Win {
	case WinRowNumber:
		for n, ri := range idxs {
			winvals[ri][wi] = int64(n + 1)
		}
		return nil
	case WinRank, WinDenseRank:
		var rank, dense int64
		var prev []any
		for n, ri := range idxs {
			cur, err := orderVals(env, w.OrderBy, base[ri])
			if err != nil {
				return err
			}
			if prev == nil || !equalVals(prev, cur) {
				dense++
				rank = int64(n + 1)
			}
			if w.Win == WinRank {
				winvals[ri][wi] = rank
			} else {
				winvals[ri][wi] = dense
			}
			prev = cur
		}
		return nil
	}

	// Aggregate window.
	intSum := w.Arg != nil && exprType(sc, w.Arg) == bytdb.TInt
	newAcc := func() *accum { return &accum{fn: w.Agg, ord: -1, argEx: w.Arg, intSum: intSum} }
	add := func(acc *accum, ri int) error {
		re := *env
		re.row = base[ri]
		return acc.add(&re, base[ri])
	}
	if len(w.OrderBy) == 0 {
		// Whole partition: one value shared by every row.
		acc := newAcc()
		for _, ri := range idxs {
			if err := add(acc, ri); err != nil {
				return err
			}
		}
		v := acc.value()
		for _, ri := range idxs {
			winvals[ri][wi] = v
		}
		return nil
	}
	// Running aggregate, RANGE default: peers sharing an ORDER BY key
	// all see the value accumulated through the end of their peer group.
	acc := newAcc()
	for i := 0; i < len(idxs); {
		key0, err := orderVals(env, w.OrderBy, base[idxs[i]])
		if err != nil {
			return err
		}
		j := i
		for j < len(idxs) {
			kj, err := orderVals(env, w.OrderBy, base[idxs[j]])
			if err != nil {
				return err
			}
			if !equalVals(key0, kj) {
				break
			}
			if err := add(acc, idxs[j]); err != nil {
				return err
			}
			j++
		}
		v := acc.value()
		for k := i; k < j; k++ {
			winvals[idxs[k]][wi] = v
		}
		i = j
	}
	return nil
}

// orderVals evaluates a window ORDER BY's expressions on one row.
func orderVals(env *exEnv, ob []OrderItem, row []any) ([]any, error) {
	exprs := make([]Expr, len(ob))
	for i, o := range ob {
		exprs[i] = o.Ex
	}
	return evalRow(env, exprs, row)
}

// evalRow evaluates a list of expressions against a single base row.
func evalRow(env *exEnv, exprs []Expr, row []any) ([]any, error) {
	out := make([]any, len(exprs))
	re := *env
	re.row = row
	for i, e := range exprs {
		v, err := evalEx(&re, e)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// equalVals reports whether two value slices are equal under the
// comparison ordering, with NULLs equal to each other (peer grouping).
func equalVals(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] == nil && b[i] == nil {
			continue
		}
		if a[i] == nil || b[i] == nil {
			return false
		}
		if c, ok := compareVals(a[i], b[i]); !ok || c != 0 {
			return false
		}
	}
	return true
}
