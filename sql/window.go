package sql

import (
	"math"
	"slices"
	"sort"
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
// ROWS/RANGE/GROUPS clause (RANGE offsets are distances on the sort
// key) with optional EXCLUDE CURRENT ROW/GROUP/TIES, or the SQL
// default — RANGE UNBOUNDED PRECEDING .. CURRENT ROW, which
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
		if w.Frame.Exclude != ExcludeNoOthers {
			b.WriteString(" " + w.Frame.Exclude.name())
		}
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
	// windows. RANGE and GROUPS bounds resolve through peer groups (and
	// RANGE offsets through the sort keys), so a framer bundles those
	// once per partition here.
	groups, groupOf, err := peerGroups(env, w, base, idxs)
	if err != nil {
		return err
	}
	fr, err := newFramer(env, w, fs, base, idxs, groups, groupOf)
	if err != nil {
		return err
	}
	if w.Win != WinNone {
		return assignValueFrame(env, w, fr, base, idxs, wi, winvals)
	}
	return assignAggFrame(env, sc, w, fr, base, idxs, wi, winvals)
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
// plus their offsets already evaluated, so computing a row's frame is
// position arithmetic (ROWS/GROUPS) or a sort-key search (RANGE).
type frameSpec struct {
	mode             FrameMode
	start, end       FrameBoundType
	startOff, endOff int64 // ROWS/GROUPS offsets: row / peer-group counts
	startNum, endNum any   // RANGE offsets: numeric distances on the sort key
	exclude          FrameExclusion
}

// hasRangeOffsets reports whether the frame has RANGE offset bounds,
// which measure distances on the window ORDER BY key and so need the
// key values (and their type checked) rather than just positions.
func (fs frameSpec) hasRangeOffsets() bool {
	return fs.mode == FrameRange &&
		(fs.start == BoundOffsetPreceding || fs.start == BoundOffsetFollowing ||
			fs.end == BoundOffsetPreceding || fs.end == BoundOffsetFollowing)
}

// resolveFrame turns a window's frame clause into a frameSpec,
// evaluating the offset expressions. No clause means the SQL default:
// RANGE UNBOUNDED PRECEDING .. CURRENT ROW. RANGE offsets are typed
// differently from ROWS/GROUPS ones — they are distances on the sort
// key, so they may be fractional, and the key itself must be numeric
// for the arithmetic to exist (the parser already guaranteed exactly
// one ORDER BY column).
func resolveFrame(env *exEnv, w *ExWindow) (frameSpec, error) {
	if w.Frame == nil {
		return frameSpec{mode: FrameRange, start: BoundUnboundedPreceding, end: BoundCurrentRow}, nil
	}
	fs := frameSpec{mode: w.Frame.Mode, start: w.Frame.Start.Type, end: w.Frame.End.Type, exclude: w.Frame.Exclude}
	var err error
	if fs.hasRangeOffsets() {
		if t := exprType(env.sc, w.OrderBy[0].Ex); t != bytdb.TInt && t != bytdb.TFloat {
			return fs, serr.New("RANGE with offset PRECEDING/FOLLOWING is not supported for column type " + string(t))
		}
		if fs.startNum, err = rangeOffset(env, w.Frame.Start.Offset, "starting"); err != nil {
			return fs, err
		}
		fs.endNum, err = rangeOffset(env, w.Frame.End.Offset, "ending")
		return fs, err
	}
	if fs.startOff, err = frameOffset(env, w.Frame.Start.Offset, "starting"); err != nil {
		return fs, err
	}
	fs.endOff, err = frameOffset(env, w.Frame.End.Offset, "ending")
	return fs, err
}

// frameOffset evaluates one ROWS/GROUPS frame offset. The parse-time
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

// rangeOffset evaluates one RANGE frame offset: a non-null numeric
// distance on the sort key, which unlike a row count may be
// fractional. Negative (and NaN — no ordered distance) sizes get
// Postgres' runtime wording.
func rangeOffset(env *exEnv, e Expr, which string) (any, error) {
	if e == nil {
		return nil, nil
	}
	v, err := evalOn(env, e, nil)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, serr.New("frame " + which + " offset must not be null")
	}
	f, ok := asFloat(v)
	if !ok {
		return nil, serr.New("frame " + which + " offset must be numeric")
	}
	if f < 0 || math.IsNaN(f) {
		return nil, serr.New("invalid preceding or following size in window function")
	}
	return v, nil
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

// framer computes per-row frames for one sorted partition. It bundles
// the resolved frameSpec with the partition context every bound type
// needs: peer groups for RANGE/GROUPS CURRENT ROW and GROUPS offsets,
// and — only when the frame has RANGE offset bounds — the sort-key
// value of every row, since those offsets are distances on the key.
type framer struct {
	fs      frameSpec
	size    int
	groups  []peerGroup
	groupOf []int
	// RANGE-offset context (nil/zero otherwise): keys[k] is the single
	// window ORDER BY key at partition position k, desc its direction,
	// and [nnLo, nnHi) the contiguous run of non-NULL keys (NULLs sort
	// last ascending, first descending, so one run at one end).
	keys       []any
	desc       bool
	nnLo, nnHi int
}

// newFramer builds a partition's framer, evaluating the sort keys up
// front when the frame has RANGE offset bounds.
func newFramer(env *exEnv, w *ExWindow, fs frameSpec, base [][]any, idxs []int, groups []peerGroup, groupOf []int) (*framer, error) {
	fr := &framer{fs: fs, size: len(idxs), groups: groups, groupOf: groupOf}
	if !fs.hasRangeOffsets() {
		return fr, nil
	}
	fr.desc = w.OrderBy[0].Desc
	fr.keys = make([]any, len(idxs))
	for k, ri := range idxs {
		v, err := evalOn(env, w.OrderBy[0].Ex, base[ri])
		if err != nil {
			return nil, err
		}
		fr.keys[k] = v
	}
	// Trim the NULL run off whichever end it sorted to.
	fr.nnLo, fr.nnHi = 0, len(idxs)
	for fr.nnHi > fr.nnLo && fr.keys[fr.nnHi-1] == nil {
		fr.nnHi--
	}
	for fr.nnLo < fr.nnHi && fr.keys[fr.nnLo] == nil {
		fr.nnLo++
	}
	return fr, nil
}

// bounds computes the frame of the row at partition position n as a
// half-open position range [lo, hi) over a partition of size rows.
// ROWS bounds are plain position arithmetic; GROUPS bounds (and every
// mode's CURRENT ROW) resolve through peer groups — CURRENT ROW means
// the current row's whole peer group, and a GROUPS offset steps whole
// groups; RANGE offset bounds are searches on the sort key:
//
//	positions: 0 1 | 2 3 4 | 5      (| = peer-group boundary)
//	ROWS   BETWEEN 1 PRECEDING AND 1 FOLLOWING  at n=3 -> [2,5)
//	GROUPS BETWEEN 1 PRECEDING AND CURRENT ROW  at n=3 -> [0,5)
//
//	keys:      1 2 | 4 4 | 7                    (RANGE, ORDER BY key)
//	RANGE  BETWEEN 2 PRECEDING AND 2 FOLLOWING  at key=4 -> keys in [2,6] -> [1,4)
//
// Bounds past a partition edge clamp to it, and a start past the end
// yields an empty frame (lo == hi) — legal, and NULL/zero-count
// downstream.
func (fr *framer) bounds(n int) (lo, hi int) {
	fs, size := fr.fs, fr.size
	g := fr.groupOf[n]
	// Clamp offsets to the position/group count first: anything larger
	// lands past an edge regardless, and clamping keeps the arithmetic
	// below far from int overflow even for absurd literal offsets.
	// (RANGE offsets never enter position arithmetic, so they are not
	// clamped — rangeEdge saturates its key arithmetic instead.)
	so := int(min(fs.startOff, int64(size)))
	eo := int(min(fs.endOff, int64(size)))
	switch fs.start {
	case BoundUnboundedPreceding:
		lo = 0
	case BoundOffsetPreceding:
		switch {
		case fs.mode == FrameRows:
			lo = n - so
		case fs.mode == FrameRange:
			lo = fr.rangeEdge(n, fs.startNum, true, false)
		default:
			if pg := g - so; pg < 0 {
				lo = 0 // group before the partition: frame starts at its head
			} else {
				lo = fr.groups[pg].lo
			}
		}
	case BoundCurrentRow:
		if fs.mode == FrameRows {
			lo = n
		} else {
			lo = fr.groups[g].lo
		}
	case BoundOffsetFollowing:
		switch {
		case fs.mode == FrameRows:
			lo = n + so
		case fs.mode == FrameRange:
			lo = fr.rangeEdge(n, fs.startNum, false, false)
		default:
			if pg := g + so; pg >= len(fr.groups) {
				lo = size // group after the partition: empty frame
			} else {
				lo = fr.groups[pg].lo
			}
		}
	}
	switch fs.end {
	case BoundOffsetPreceding:
		switch {
		case fs.mode == FrameRows:
			hi = n - eo + 1
		case fs.mode == FrameRange:
			hi = fr.rangeEdge(n, fs.endNum, true, true)
		default:
			if pg := g - eo; pg < 0 {
				hi = 0 // group before the partition: empty frame
			} else {
				hi = fr.groups[pg].hi
			}
		}
	case BoundCurrentRow:
		if fs.mode == FrameRows {
			hi = n + 1
		} else {
			hi = fr.groups[g].hi
		}
	case BoundOffsetFollowing:
		switch {
		case fs.mode == FrameRows:
			hi = n + eo + 1
		case fs.mode == FrameRange:
			hi = fr.rangeEdge(n, fs.endNum, false, true)
		default:
			if pg := g + eo; pg >= len(fr.groups) {
				hi = size // group after the partition: frame ends at its tail
			} else {
				hi = fr.groups[pg].hi
			}
		}
	case BoundUnboundedFollowing:
		hi = size
	}
	lo = max(0, min(lo, size))
	hi = max(lo, min(hi, size))
	return lo, hi
}

// rangeEdge resolves one RANGE offset bound for the row at partition
// position n: the boundary position where the sort key crosses
// key(n) ± off. Since the partition is sorted, that is a binary
// search over the non-NULL key run — end marks an end bound, whose
// edge is one past the last in-frame key (first key strictly beyond
// the threshold) rather than the first in-frame key.
//
// Confining the search to [nnLo, nnHi) is what implements Postgres'
// NULL semantics for a non-NULL current row: NULL keys sort to one
// end and act as infinitely preceding/following there, so an offset
// bound never reaches them — yet an UNBOUNDED or over-the-edge bound
// on the other side still takes them in, because these positions
// only ever clamp the NULL side of the partition off via nnLo/nnHi.
// A NULL current row is within any distance of NULL and of nothing
// else (Postgres' in_range), so its offset bounds are just its peer
// group's edges.
func (fr *framer) rangeEdge(n int, off any, preceding, end bool) int {
	v := fr.keys[n]
	if v == nil {
		g := fr.groupOf[n]
		if end {
			return fr.groups[g].hi
		}
		return fr.groups[g].lo
	}
	// PRECEDING moves against the sort direction, so under DESC it
	// adds; the threshold itself is then compared in sort order.
	t := rangeThreshold(v, off, preceding != fr.desc)
	return fr.nnLo + sort.Search(fr.nnHi-fr.nnLo, func(i int) bool {
		c, _ := compareVals(fr.keys[fr.nnLo+i], t) // both non-NULL numerics
		if fr.desc {
			c = -c
		}
		if end {
			return c > 0
		}
		return c >= 0
	})
}

// seg is one contiguous run of frame positions; frame exclusion can
// split a row's frame into at most three of these.
type seg struct{ lo, hi int }

// segs computes the frame of the row at partition position n as an
// ordered list of disjoint, non-empty position segments. Without an
// EXCLUDE clause that is just bounds(n); with one, the excluded run —
// the current row, its whole peer group, or the peers minus the row
// itself (TIES) — is clipped out of [lo, hi):
//
//	bounds  [lo...............hi)    peers [pLo ..n.. pHi)
//	EXCLUDE CURRENT ROW  ->  [lo, n)   ∪ [n+1, hi)
//	EXCLUDE GROUP        ->  [lo, pLo) ∪ [pHi, hi)
//	EXCLUDE TIES         ->  [lo, pLo) ∪ [n, n+1) ∪ [pHi, hi)
//
// Every piece is intersected with [lo, hi): exclusion only removes
// rows the bounds selected, so TIES does not re-admit a current row
// the frame never contained. buf is appended to and returned, letting
// callers reuse one backing array across rows.
func (fr *framer) segs(n int, buf []seg) []seg {
	lo, hi := fr.bounds(n)
	xLo, xHi := n, n+1 // the excluded run; CURRENT ROW's case
	switch fr.fs.exclude {
	case ExcludeNoOthers:
		if lo < hi {
			buf = append(buf, seg{lo, hi})
		}
		return buf
	case ExcludeGroup, ExcludeTies:
		g := fr.groups[fr.groupOf[n]]
		xLo, xHi = g.lo, g.hi
	}
	if l := (seg{lo, min(hi, xLo)}); l.lo < l.hi {
		buf = append(buf, l)
	}
	if fr.fs.exclude == ExcludeTies {
		if m := (seg{max(lo, n), min(hi, n+1)}); m.lo < m.hi {
			buf = append(buf, m)
		}
	}
	if r := (seg{max(lo, xHi), hi}); r.lo < r.hi {
		buf = append(buf, r)
	}
	return buf
}

// segPos maps the k-th frame row (0-based) to its partition position
// by walking the segments in order.
func segPos(segs []seg, k int) int {
	for _, s := range segs {
		if k < s.hi-s.lo {
			return s.lo + k
		}
		k -= s.hi - s.lo
	}
	return -1 // unreachable: callers bound k by the summed length
}

// rangeThreshold computes the sort-key boundary v ± off for a RANGE
// offset bound (sub chooses the sign). Pure-int arithmetic stays
// exact, with a saturation guard: off is validated non-negative, so
// wraparound is detectable by direction, and an overflowed threshold
// lies past every representable key — ±Inf compares the same way.
// Any float operand switches to float math, whose NaN propagation
// matches the comparator's ordering: a NaN key sorts after +Inf
// (cmpFloat), so finite thresholds exclude it, while a NaN current
// row yields NaN thresholds that equal only its fellow NaNs — the
// same "NaN is within any distance of NaN, and of nothing else" that
// Postgres' float in_range implements.
func rangeThreshold(v, off any, sub bool) any {
	vi, vok := v.(int64)
	oi, ook := off.(int64)
	if vok && ook {
		if sub {
			if r := vi - oi; r <= vi {
				return r
			}
			return math.Inf(-1)
		}
		if r := vi + oi; r >= vi {
			return r
		}
		return math.Inf(1)
	}
	vf, _ := asFloat(v)
	of, _ := asFloat(off)
	if sub {
		return vf - of
	}
	return vf + of
}

// assignValueFrame fills FIRST_VALUE/LAST_VALUE/NTH_VALUE for one
// sorted partition: each row surfaces one row of its own frame. Under
// the default frame LAST_VALUE lands on the current row's last peer
// (the famous PG surprise — not the partition end); explicit bounds
// (or an EXCLUDE clause, which can hollow the frame into segments)
// can make any frame, including an empty one, which yields NULL.
func assignValueFrame(env *exEnv, w *ExWindow, fr *framer, base [][]any, idxs []int, wi int, winvals [][]any) error {
	var segbuf []seg
	for n, ri := range idxs {
		segbuf = fr.segs(n, segbuf[:0])
		total := 0
		for _, s := range segbuf {
			total += s.hi - s.lo
		}
		var t int // frame position to surface
		switch w.Win {
		case WinFirstValue:
			if total == 0 {
				continue // empty frame -> NULL
			}
			t = segbuf[0].lo
		case WinLastValue:
			if total == 0 {
				continue
			}
			t = segbuf[len(segbuf)-1].hi - 1
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
			if nth > int64(total) {
				continue // frame shorter than n (or empty) -> NULL
			}
			t = segPos(segbuf, int(nth)-1)
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
// position (RANGE offset ends included: sorted keys make the
// thresholds, and so the searched edges, monotone) — so one
// accumulator extends forward and the common default frame stays
// O(rows). A moving start invalidates that (min/max can't retract),
// so those frames recompute per distinct [lo, hi), memoizing the last
// range since consecutive rows often share a frame (always, for
// RANGE/GROUPS peers); a sliding ROWS frame is O(rows x frame width).
// An EXCLUDE clause hollows a hole that moves with the current row,
// which defeats the anchored fast path too, so exclusion always takes
// the recompute-with-memoization road — under EXCLUDE GROUP whole
// peer groups still share segment lists, so the memo keeps RANGE/
// GROUPS frames at one aggregation per group.
func assignAggFrame(env *exEnv, sc *scope, w *ExWindow, fr *framer, base [][]any, idxs []int, wi int, winvals [][]any) error {
	intSum := w.Arg != nil && exprType(sc, w.Arg) == bytdb.TInt
	newAcc := func() *accum { return &accum{fn: w.Agg, ord: -1, argEx: w.Arg, intSum: intSum} }
	add := func(acc *accum, ri int) error {
		re := *env
		re.row = base[ri]
		return acc.add(&re, base[ri])
	}
	if fr.fs.exclude != ExcludeNoOthers {
		var lastSegs, segbuf []seg
		var lastV any
		have := false // guards the first row: empty segs == empty memo otherwise
		for n, ri := range idxs {
			segbuf = fr.segs(n, segbuf[:0])
			if !have || !slices.Equal(segbuf, lastSegs) {
				acc := newAcc()
				for _, s := range segbuf {
					for k := s.lo; k < s.hi; k++ {
						if err := add(acc, idxs[k]); err != nil {
							return err
						}
					}
				}
				lastV, have = acc.value(), true
				lastSegs = append(lastSegs[:0], segbuf...)
			}
			winvals[ri][wi] = lastV
		}
		return nil
	}
	if fr.fs.start == BoundUnboundedPreceding {
		acc := newAcc()
		done := 0 // rows accumulated so far; hi never decreases
		for n, ri := range idxs {
			_, hi := fr.bounds(n)
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
		lo, hi := fr.bounds(n)
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
