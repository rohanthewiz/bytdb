package sql

import (
	"slices"
	"strings"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// window.go: window functions — fn(...) OVER (PARTITION BY ... ORDER BY
// ... [frame]). Unlike aggregates, windows emit every input row, so the
// query materializes all post-filter rows, partitions them by the
// order-preserving tuple encoding (the same equivalence GROUP BY uses),
// sorts each partition by the window ORDER BY, and assigns a per-row
// value. Ranking functions (ROW_NUMBER/RANK/DENSE_RANK), value
// functions (LAG/LEAD/FIRST_VALUE/LAST_VALUE/NTH_VALUE), and aggregate
// windows are supported. Frame-sensitive functions (the aggregates and
// FIRST/LAST/NTH_VALUE) evaluate over each row's frame: an explicit
// ROWS/GROUPS clause (RANGE only with UNBOUNDED/CURRENT ROW bounds),
// or the SQL default — RANGE UNBOUNDED PRECEDING .. CURRENT ROW, which
// with ORDER BY ends at the current row's last peer, so LAST_VALUE is
// the last peer (not the partition end) — same surprise as PG.

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
	switch w.Win {
	case WinRowNumber, WinRank, WinDenseRank:
		return bytdb.TInt // ranking functions count rows
	case WinLag, WinLead, WinFirstValue, WinLastValue, WinNthValue:
		return exprType(sc, w.Arg) // value functions surface another row's Arg
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
		if w.Offset != nil {
			b.WriteString(", " + exprText(w.Offset))
		}
		if w.Default != nil {
			b.WriteString(", " + exprText(w.Default))
		}
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
		sep = " "
	}
	if w.Frame != nil {
		// Always the canonical BETWEEN form; the single-bound shorthand
		// already normalized to it at parse.
		b.WriteString(sep + w.Frame.Mode.name() + " BETWEEN " +
			boundText(w.Frame.Start) + " AND " + boundText(w.Frame.End))
	}
	b.WriteByte(')')
	return b.String()
}

// boundText renders one frame endpoint for EXPLAIN.
func boundText(fb FrameBound) string {
	switch fb.Type {
	case BoundUnboundedPreceding:
		return "UNBOUNDED PRECEDING"
	case BoundOffsetPreceding:
		return exprText(fb.Offset) + " PRECEDING"
	case BoundCurrentRow:
		return "CURRENT ROW"
	case BoundOffsetFollowing:
		return exprText(fb.Offset) + " FOLLOWING"
	}
	return "UNBOUNDED FOLLOWING"
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
// values: resolve the frame, partition, sort each partition by the
// window ORDER BY, then assign.
func computeWindow(env *exEnv, sc *scope, base [][]any, w *ExWindow, wi int, winvals [][]any) error {
	// Frame offsets are row-independent (enforced at parse), so the
	// frame resolves once per window — and a bad offset (negative,
	// NULL, non-integer) errors even over zero rows.
	fs, err := resolveFrame(env, w)
	if err != nil {
		return err
	}
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
		if err := assignWindow(env, sc, w, fs, wi, base, idxs, winvals); err != nil {
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
// already-sorted partition. The ranking family and LAG/LEAD ignore
// the frame (as in Postgres — a frame clause on them parses but has
// no effect); everything else evaluates over each row's frame.
func assignWindow(env *exEnv, sc *scope, w *ExWindow, fs frameSpec, wi int, base [][]any, idxs []int, winvals [][]any) error {
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
	case WinLag, WinLead:
		return assignLagLead(env, w, base, idxs, wi, winvals)
	}

	// Frame-sensitive: FIRST_VALUE/LAST_VALUE/NTH_VALUE and aggregate
	// windows. RANGE and GROUPS bounds resolve through peer groups, so
	// those are built once per partition here.
	groups, groupOf, err := peerGroups(env, w, base, idxs)
	if err != nil {
		return err
	}
	if w.Win != WinNone {
		return assignValueFrame(env, w, fs, base, idxs, wi, winvals, groups, groupOf)
	}
	return assignAggFrame(env, sc, w, fs, base, idxs, wi, winvals, groups, groupOf)
}

// assignLagLead fills LAG/LEAD values for one sorted partition. Both
// address the whole partition regardless of frame, as in Postgres:
// LAG(v, o) reads o rows back, LEAD(v, o) o rows ahead. The offset is
// evaluated per row (it may be an expression), may be negative (which
// flips direction, as in PG), and a NULL offset yields NULL. Rows past
// either partition edge take the Default expression, or NULL.
func assignLagLead(env *exEnv, w *ExWindow, base [][]any, idxs []int, wi int, winvals [][]any) error {
	for n, ri := range idxs {
		off := int64(1) // Postgres default offset
		if w.Offset != nil {
			ov, err := evalOn(env, w.Offset, base[ri])
			if err != nil {
				return err
			}
			if ov == nil {
				winvals[ri][wi] = nil
				continue
			}
			o, ok := ov.(int64)
			if !ok {
				return serr.New("window function offset must be an integer", "function", w.fnName())
			}
			off = o
		}
		if w.Win == WinLead {
			off = -off
		}
		// Guard the subtraction: |off| bounded by the partition size
		// keeps int64(n)-off well inside int64, so a huge or MinInt64
		// offset can't overflow — it is simply out of the partition.
		t := int64(-1)
		if off >= -int64(len(idxs)) && off <= int64(len(idxs)) {
			t = int64(n) - off
		}
		if t >= 0 && t < int64(len(idxs)) {
			v, err := evalOn(env, w.Arg, base[idxs[t]])
			if err != nil {
				return err
			}
			winvals[ri][wi] = v
		} else if w.Default != nil {
			v, err := evalOn(env, w.Default, base[ri])
			if err != nil {
				return err
			}
			winvals[ri][wi] = v
		}
	}
	return nil
}

// frameSpec is a window frame ready for per-row use: the bound types
// plus their offsets already evaluated to integers, so computing a
// row's frame is pure position arithmetic.
type frameSpec struct {
	mode             FrameMode
	start, end       FrameBoundType
	startOff, endOff int64
}

// resolveFrame turns a window's frame clause into a frameSpec,
// evaluating the offset expressions. No clause means the SQL default:
// RANGE UNBOUNDED PRECEDING .. CURRENT ROW.
func resolveFrame(env *exEnv, w *ExWindow) (frameSpec, error) {
	if w.Frame == nil {
		return frameSpec{mode: FrameRange, start: BoundUnboundedPreceding, end: BoundCurrentRow}, nil
	}
	fs := frameSpec{mode: w.Frame.Mode, start: w.Frame.Start.Type, end: w.Frame.End.Type}
	var err error
	if fs.startOff, err = frameOffset(env, w.Frame.Start.Offset, "starting"); err != nil {
		return fs, err
	}
	fs.endOff, err = frameOffset(env, w.Frame.End.Offset, "ending")
	return fs, err
}

// frameOffset evaluates one frame offset. The parse-time
// row-independence check makes a nil row safe here; Postgres'
// runtime rules apply — the offset must be a non-null, non-negative
// integer.
func frameOffset(env *exEnv, e Expr, which string) (int64, error) {
	if e == nil {
		return 0, nil
	}
	v, err := evalOn(env, e, nil)
	if err != nil {
		return 0, err
	}
	if v == nil {
		return 0, serr.New("frame " + which + " offset must not be null")
	}
	n, ok := v.(int64)
	if !ok {
		return 0, serr.New("frame " + which + " offset must be an integer")
	}
	if n < 0 {
		return 0, serr.New("frame " + which + " offset must not be negative")
	}
	return n, nil
}

// peerGroup is one run of ORDER BY peers, as a half-open range of
// partition positions.
type peerGroup struct{ lo, hi int }

// peerGroups splits a sorted partition into ORDER BY peer groups:
// groups[g] is group g's position range and groupOf[k] the group of
// partition position k. With no ORDER BY every row is a peer — one
// group spanning the partition — which is exactly what makes the
// default frame cover the whole partition in that case.
func peerGroups(env *exEnv, w *ExWindow, base [][]any, idxs []int) (groups []peerGroup, groupOf []int, err error) {
	groupOf = make([]int, len(idxs))
	if len(w.OrderBy) == 0 {
		return []peerGroup{{0, len(idxs)}}, groupOf, nil
	}
	for i := 0; i < len(idxs); {
		key0, err := orderVals(env, w.OrderBy, base[idxs[i]])
		if err != nil {
			return nil, nil, err
		}
		j := i + 1
		for j < len(idxs) {
			kj, err := orderVals(env, w.OrderBy, base[idxs[j]])
			if err != nil {
				return nil, nil, err
			}
			if !equalVals(key0, kj) {
				break
			}
			j++
		}
		for k := i; k < j; k++ {
			groupOf[k] = len(groups)
		}
		groups = append(groups, peerGroup{lo: i, hi: j})
		i = j
	}
	return groups, groupOf, nil
}

// frameBounds computes the frame of the row at partition position n
// as a half-open position range [lo, hi) over a partition of size
// rows. ROWS bounds are plain position arithmetic; RANGE and GROUPS
// bounds resolve through peer groups — CURRENT ROW means the current
// row's whole peer group, and a GROUPS offset steps whole groups:
//
//	positions: 0 1 | 2 3 4 | 5      (| = peer-group boundary)
//	ROWS   BETWEEN 1 PRECEDING AND 1 FOLLOWING  at n=3 -> [2,5)
//	GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW  at n=3 -> [0,5)
//
// Bounds past a partition edge clamp to it, and a start past the end
// yields an empty frame (lo == hi) — legal, and NULL/zero-count
// downstream.
func frameBounds(fs frameSpec, n, size int, groups []peerGroup, groupOf []int) (lo, hi int) {
	g := groupOf[n]
	// Clamp offsets to the position/group count first: anything larger
	// lands past an edge regardless, and clamping keeps the arithmetic
	// below far from int overflow even for absurd literal offsets.
	so := int(min(fs.startOff, int64(size)))
	eo := int(min(fs.endOff, int64(size)))
	switch fs.start {
	case BoundUnboundedPreceding:
		lo = 0
	case BoundOffsetPreceding:
		if fs.mode == FrameRows {
			lo = n - so
		} else if pg := g - so; pg < 0 {
			lo = 0 // group before the partition: frame starts at its head
		} else {
			lo = groups[pg].lo
		}
	case BoundCurrentRow:
		if fs.mode == FrameRows {
			lo = n
		} else {
			lo = groups[g].lo
		}
	case BoundOffsetFollowing:
		if fs.mode == FrameRows {
			lo = n + so
		} else if pg := g + so; pg >= len(groups) {
			lo = size // group after the partition: empty frame
		} else {
			lo = groups[pg].lo
		}
	}
	switch fs.end {
	case BoundOffsetPreceding:
		if fs.mode == FrameRows {
			hi = n - eo + 1
		} else if pg := g - eo; pg < 0 {
			hi = 0 // group before the partition: empty frame
		} else {
			hi = groups[pg].hi
		}
	case BoundCurrentRow:
		if fs.mode == FrameRows {
			hi = n + 1
		} else {
			hi = groups[g].hi
		}
	case BoundOffsetFollowing:
		if fs.mode == FrameRows {
			hi = n + eo + 1
		} else if pg := g + eo; pg >= len(groups) {
			hi = size // group after the partition: frame ends at its tail
		} else {
			hi = groups[pg].hi
		}
	case BoundUnboundedFollowing:
		hi = size
	}
	lo = max(0, min(lo, size))
	hi = max(lo, min(hi, size))
	return lo, hi
}

// assignValueFrame fills FIRST_VALUE/LAST_VALUE/NTH_VALUE for one
// sorted partition: each row surfaces one row of its own frame. Under
// the default frame LAST_VALUE lands on the current row's last peer
// (the famous PG surprise — not the partition end); explicit bounds
// can make any frame, including an empty one, which yields NULL.
func assignValueFrame(env *exEnv, w *ExWindow, fs frameSpec, base [][]any, idxs []int, wi int, winvals [][]any, groups []peerGroup, groupOf []int) error {
	for n, ri := range idxs {
		lo, hi := frameBounds(fs, n, len(idxs), groups, groupOf)
		var t int // frame position to surface
		switch w.Win {
		case WinFirstValue:
			if lo >= hi {
				continue // empty frame -> NULL
			}
			t = lo
		case WinLastValue:
			if lo >= hi {
				continue
			}
			t = hi - 1
		default: // NTH_VALUE: n evaluates per row, so peers can differ
			nv, err := evalOn(env, w.Offset, base[ri])
			if err != nil {
				return err
			}
			if nv == nil {
				continue // NULL n -> NULL, as in Postgres
			}
			nth, ok := nv.(int64)
			if !ok {
				return serr.New("nth_value argument must be an integer")
			}
			if nth < 1 {
				return serr.New("argument of nth_value must be greater than zero")
			}
			if nth > int64(hi-lo) {
				continue // frame shorter than n (or empty) -> NULL
			}
			t = lo + int(nth) - 1
		}
		v, err := evalOn(env, w.Arg, base[idxs[t]])
		if err != nil {
			return err
		}
		winvals[ri][wi] = v
	}
	return nil
}

// assignAggFrame fills an aggregate window for one sorted partition.
// A frame anchored at the partition start (UNBOUNDED PRECEDING) only
// ever grows — every end-bound type is nondecreasing in the row
// position — so one accumulator extends forward and the common
// default frame stays O(rows). A moving start invalidates that
// (min/max can't retract), so those frames recompute per distinct
// [lo, hi), memoizing the last range since consecutive rows often
// share a frame (always, for RANGE/GROUPS peers); a sliding ROWS
// frame is O(rows x frame width).
func assignAggFrame(env *exEnv, sc *scope, w *ExWindow, fs frameSpec, base [][]any, idxs []int, wi int, winvals [][]any, groups []peerGroup, groupOf []int) error {
	intSum := w.Arg != nil && exprType(sc, w.Arg) == bytdb.TInt
	newAcc := func() *accum { return &accum{fn: w.Agg, ord: -1, argEx: w.Arg, intSum: intSum} }
	add := func(acc *accum, ri int) error {
		re := *env
		re.row = base[ri]
		return acc.add(&re, base[ri])
	}
	size := len(idxs)
	if fs.start == BoundUnboundedPreceding {
		acc := newAcc()
		done := 0 // rows accumulated so far; hi never decreases
		for n, ri := range idxs {
			_, hi := frameBounds(fs, n, size, groups, groupOf)
			for ; done < hi; done++ {
				if err := add(acc, idxs[done]); err != nil {
					return err
				}
			}
			winvals[ri][wi] = acc.value()
		}
		return nil
	}
	lastLo, lastHi := -1, -1
	var lastV any
	for n, ri := range idxs {
		lo, hi := frameBounds(fs, n, size, groups, groupOf)
		if lo != lastLo || hi != lastHi {
			acc := newAcc()
			for k := lo; k < hi; k++ {
				if err := add(acc, idxs[k]); err != nil {
					return err
				}
			}
			lastV, lastLo, lastHi = acc.value(), lo, hi
		}
		winvals[ri][wi] = lastV
	}
	return nil
}

// evalOn evaluates a single expression against one base row.
func evalOn(env *exEnv, e Expr, row []any) (any, error) {
	vs, err := evalRow(env, []Expr{e}, row)
	if err != nil {
		return nil, err
	}
	return vs[0], nil
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
