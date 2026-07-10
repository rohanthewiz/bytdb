package sql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// seedFrames builds a table with duplicate ORDER BY keys so ROWS,
// RANGE, and GROUPS frames all give distinguishable answers: peer
// groups by k are {1,2} (k=1), {3} (k=2), {4} (k=3), with powers of
// two for v so every distinct frame has a unique sum.
func seedFrames(t *testing.T, d *DB) {
	t.Helper()
	exec(t, d, `create table f (id int primary key, k int, v int)`)
	exec(t, d, `insert into f values (1,1,1),(2,1,2),(3,2,4),(4,3,8)`)
}

// TestFrameRows covers ROWS mode: the sliding window, the single-bound
// shorthand, an anchored-end frame, and the tie-breaking difference
// from the RANGE default (ROWS CURRENT ROW stops at the row itself,
// not its last peer).
func TestFrameRows(t *testing.T) {
	d := openDB(t)
	seedFrames(t, d)

	res := exec(t, d, `select id,
		sum(v) over (order by k, id rows between 1 preceding and 1 following) as slide,
		sum(v) over (order by k, id rows 2 preceding) as short,
		sum(v) over (order by k, id rows between current row and unbounded following) as tail,
		sum(v) over (order by k rows unbounded preceding) as run,
		sum(v) over (order by k) as peers
		from f order by id`)
	want := [][]any{
		// run vs peers: rows 1,2 tie on k, so the RANGE default sums
		// through the peer group (3) while ROWS stops at each row.
		{int64(1), int64(3), int64(1), int64(15), int64(1), int64(3)},
		{int64(2), int64(7), int64(3), int64(14), int64(3), int64(3)},
		{int64(3), int64(14), int64(7), int64(12), int64(7), int64(7)},
		{int64(4), int64(12), int64(14), int64(8), int64(15), int64(15)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("rows frames: %v", res.Rows)
	}
}

// TestFrameGroups covers GROUPS mode: offsets count whole peer groups,
// and CURRENT ROW spans the current group.
func TestFrameGroups(t *testing.T) {
	d := openDB(t)
	seedFrames(t, d)

	res := exec(t, d, `select id,
		sum(v) over (order by k groups between 1 preceding and current row) as back,
		sum(v) over (order by k groups between current row and 1 following) as fwd,
		count(*) over (order by k groups between 2 following and 3 following) as far
		from f order by id`)
	want := [][]any{
		// Peer groups: {1,2} {3} {4}. For rows 1,2 "back" clamps to the
		// first group; "far" (groups 2..3 ahead) is empty past the end.
		{int64(1), int64(3), int64(7), int64(1)},
		{int64(2), int64(3), int64(7), int64(1)},
		{int64(3), int64(7), int64(12), int64(0)},
		{int64(4), int64(12), int64(8), int64(0)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("groups frames: %v", res.Rows)
	}
}

// TestFrameValueFunctions: explicit frames change what FIRST/LAST/NTH
// _VALUE see — including the canonical fix for the LAST_VALUE default-
// frame surprise (extend the frame to the whole partition) and empty
// frames yielding NULL.
func TestFrameValueFunctions(t *testing.T) {
	d := openDB(t)
	seedFrames(t, d)

	res := exec(t, d, `select id,
		last_value(v) over (order by k range between unbounded preceding and unbounded following) as lvfull,
		last_value(v) over (order by k) as lvpeer,
		nth_value(v, 2) over (order by k rows between unbounded preceding and unbounded following) as nv2,
		first_value(v) over (order by k, id rows between 2 following and 3 following) as fvahead
		from f order by id`)
	want := [][]any{
		// lvfull is the partition end everywhere (the PG-surprise fix);
		// lvpeer keeps the default peer-bounded behavior for contrast.
		// fvahead's frame is empty once fewer than 2 rows remain ahead.
		{int64(1), int64(8), int64(2), int64(2), int64(4)},
		{int64(2), int64(8), int64(2), int64(2), int64(8)},
		{int64(3), int64(8), int64(4), int64(2), nil},
		{int64(4), int64(8), int64(8), int64(2), nil},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("value frames: %v", res.Rows)
	}
}

// TestFrameEmptyAndIgnored: empty frames aggregate to COUNT 0 / SUM
// NULL, ranking functions and LAG ignore a frame clause entirely, and
// EXCLUDE NO OTHERS is the accepted no-op spelling.
func TestFrameEmptyAndIgnored(t *testing.T) {
	d := openDB(t)
	seedFrames(t, d)

	res := exec(t, d, `select id,
		sum(v) over (order by k, id rows between 3 following and 4 following) as s,
		count(*) over (order by k, id rows between 3 following and 4 following) as c,
		row_number() over (order by k, id rows between 1 preceding and current row) as rn,
		lag(v) over (order by k, id rows between current row and current row) as lg,
		sum(v) over (order by k, id rows between 1 preceding and current row exclude no others) as x
		from f order by id`)
	want := [][]any{
		{int64(1), int64(8), int64(1), int64(1), nil, int64(1)},
		{int64(2), nil, int64(0), int64(2), int64(1), int64(3)},
		{int64(3), nil, int64(0), int64(3), int64(2), int64(6)},
		{int64(4), nil, int64(0), int64(4), int64(4), int64(12)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("empty/ignored frames: %v", res.Rows)
	}
}

// TestFrameParamsAndExplain: $n placeholders bind inside frame offsets
// (and describe as int for wire drivers), and EXPLAIN renders the
// canonical BETWEEN form.
func TestFrameParamsAndExplain(t *testing.T) {
	d := openDB(t)
	seedFrames(t, d)

	res, err := d.Exec(`select sum(v) over (order by k, id rows between $1 preceding and $2 following) from f order by id`,
		1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]any{{int64(3)}, {int64(7)}, {int64(14)}, {int64(12)}}; !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("bound frame offsets: %v", res.Rows)
	}

	info := describe(t, d, `select sum(v) over (order by k rows between $1 preceding and $2 following) from f`)
	if !reflect.DeepEqual(info.Params, []bytdb.ColType{bytdb.TInt, bytdb.TInt}) {
		t.Fatalf("inferred params: %v", info.Params)
	}

	res = exec(t, d, `explain select sum(v) over (order by k rows 2 preceding),
		count(*) over (rows between current row and unbounded following),
		avg(v) over (partition by k order by id groups between 1 preceding and 1 following) from f`)
	var plan strings.Builder
	for _, r := range res.Rows {
		plan.WriteString(r[0].(string) + "\n")
	}
	for _, want := range []string{
		"sum(v) OVER (ORDER BY k ROWS BETWEEN 2 PRECEDING AND CURRENT ROW)",
		"count(*) OVER (ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING)",
		"avg(v) OVER (PARTITION BY k ORDER BY id GROUPS BETWEEN 1 PRECEDING AND 1 FOLLOWING)",
	} {
		if !strings.Contains(plan.String(), want) {
			t.Errorf("plan missing %q:\n%s", want, plan.String())
		}
	}
}

// TestFrameRangeOffsets covers RANGE offset bounds over an int sort
// key: offsets are distances on the key (ties land in one frame, gaps
// shrink it — the contrast with ROWS), DESC flips the direction,
// fractional offsets work over int keys, and offsets big enough to
// overflow the key arithmetic saturate rather than wrap.
func TestFrameRangeOffsets(t *testing.T) {
	d := openDB(t)
	seedFrames(t, d)

	res := exec(t, d, `select id,
		sum(v) over (order by k range 2 preceding) as short,
		sum(v) over (order by k range between 1 preceding and 1 following) as slide,
		sum(v) over (order by k range between 1 following and 2 following) as ahead,
		sum(v) over (order by k desc range 1 preceding) as dsc,
		sum(v) over (order by k range between current row and 9223372036854775807 following) as sat,
		sum(v) over (order by k range 0.5 preceding) as frac
		from f order by id`)
	want := [][]any{
		// k values 1,1,2,3 with v 1,2,4,8. Both k=1 rows share every
		// frame (RANGE bounds are peer-wide by construction); "ahead"
		// is empty for k=3 (no key in [4,5]); "sat"'s k+MaxInt64
		// overflows and saturates past every key; "frac"'s half-unit
		// reach only ever spans the current row's peers.
		{int64(1), int64(3), int64(7), int64(12), int64(7), int64(15), int64(3)},
		{int64(2), int64(3), int64(7), int64(12), int64(7), int64(15), int64(3)},
		{int64(3), int64(7), int64(15), int64(8), int64(12), int64(12), int64(4)},
		{int64(4), int64(15), int64(12), nil, int64(8), int64(8), int64(8)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("range offset frames: %v", res.Rows)
	}
}

// seedRangeFloat builds a table with a float sort key at uneven
// spacing, so fractional RANGE offsets give distinguishable frames.
func seedRangeFloat(t *testing.T, d *DB) {
	t.Helper()
	exec(t, d, `create table g (id int primary key, x float, v int)`)
	exec(t, d, `insert into g values (1,1.0,1),(2,1.5,2),(3,2.0,4),(4,3.5,8)`)
}

// TestFrameRangeFloat covers RANGE offsets over a float sort key —
// fractional and integer offsets, and the value functions reading
// their frames.
func TestFrameRangeFloat(t *testing.T) {
	d := openDB(t)
	seedRangeFloat(t, d)

	res := exec(t, d, `select id,
		sum(v) over (order by x range 0.5 preceding) as half,
		sum(v) over (order by x range between 0.5 preceding and 1.5 following) as span,
		sum(v) over (order by x range 1 preceding) as intoff,
		first_value(v) over (order by x range between 0.5 preceding and current row) as fv,
		last_value(v) over (order by x range between current row and 1.5 following) as lv
		from g order by id`)
	want := [][]any{
		// x values 1.0, 1.5, 2.0, 3.5 with v 1,2,4,8.
		{int64(1), int64(1), int64(7), int64(1), int64(1), int64(4)},
		{int64(2), int64(3), int64(7), int64(3), int64(1), int64(4)},
		{int64(3), int64(6), int64(14), int64(7), int64(2), int64(8)},
		{int64(4), int64(8), int64(8), int64(8), int64(8), int64(8)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("float range frames: %v", res.Rows)
	}
}

// TestFrameRangeNulls pins Postgres' NULL semantics for RANGE offset
// frames: a NULL sort key is within any distance of NULL only, so a
// NULL row's offset frame is its peer group; non-NULL rows never
// reach the NULLs through an offset bound, but an UNBOUNDED bound
// from the other side still takes them in.
func TestFrameRangeNulls(t *testing.T) {
	d := openDB(t)
	seedFrames(t, d)
	exec(t, d, `insert into f values (5, null, 16)`)

	res := exec(t, d, `select id,
		sum(v) over (order by k range 1 preceding) as back,
		sum(v) over (order by k range between unbounded preceding and 1 following) as head,
		sum(v) over (order by k desc range between current row and 1 following) as dfw
		from f order by id`)
	want := [][]any{
		// "back": non-NULL rows sum as if the NULL row weren't there;
		// the NULL row sums only itself. "head" starts UNBOUNDED, so
		// the NULL row (sorted last ascending) sees the whole table.
		// "dfw" sorts NULL first (descending), and its offset end still
		// stops at the NULL peer group.
		{int64(1), int64(3), int64(7), int64(3)},
		{int64(2), int64(3), int64(7), int64(3)},
		{int64(3), int64(7), int64(15), int64(7)},
		{int64(4), int64(12), int64(15), int64(12)},
		{int64(5), int64(16), int64(31), int64(16)},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("null range frames: %v", res.Rows)
	}
}

// TestFrameRangeParams: $n placeholders bind as RANGE offsets and
// describe with the sort key's type (float over a float key — a wire
// driver encoding by described type must not truncate 0.5 to int),
// int over an int key.
func TestFrameRangeParams(t *testing.T) {
	d := openDB(t)
	seedFrames(t, d)
	seedRangeFloat(t, d)

	res, err := d.Exec(`select sum(v) over (order by x range between $1 preceding and $2 following) from g order by id`,
		0.5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]any{{int64(3)}, {int64(7)}, {int64(6)}, {int64(8)}}; !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("bound range offsets: %v", res.Rows)
	}

	info := describe(t, d, `select sum(v) over (order by x range between $1 preceding and $2 following) from g`)
	if !reflect.DeepEqual(info.Params, []bytdb.ColType{bytdb.TFloat, bytdb.TFloat}) {
		t.Fatalf("inferred float params: %v", info.Params)
	}
	info = describe(t, d, `select sum(v) over (order by k range $1 preceding) from f`)
	if !reflect.DeepEqual(info.Params, []bytdb.ColType{bytdb.TInt}) {
		t.Fatalf("inferred int param: %v", info.Params)
	}
}

// TestFrameErrors covers the parse- and run-time rejections: illegal
// bound pairs (Postgres' wording), unsupported modes and exclusions,
// row-dependent offsets, and bad offset values.
func TestFrameErrors(t *testing.T) {
	d := openDB(t)
	seedFrames(t, d)

	for _, c := range []struct{ q, want string }{
		{`select sum(v) over (order by k rows between unbounded following and current row) from f`,
			"frame start cannot be UNBOUNDED FOLLOWING"},
		{`select sum(v) over (order by k rows between current row and unbounded preceding) from f`,
			"frame end cannot be UNBOUNDED PRECEDING"},
		{`select sum(v) over (order by k rows between current row and 1 preceding) from f`,
			"starting from current row cannot have preceding rows"},
		{`select sum(v) over (order by k rows between 1 following and 1 preceding) from f`,
			"starting from following row cannot have preceding rows"},
		{`select sum(v) over (order by k rows between 1 following and current row) from f`,
			"starting from following row cannot reference current row"},
		{`select sum(v) over (order by k rows 1 following) from f`,
			"starting from following row cannot reference current row"},
		{`select sum(v) over (order by k, id range 1 preceding) from f`,
			"RANGE with offset PRECEDING/FOLLOWING requires exactly one ORDER BY column"},
		{`select sum(v) over (range 1 preceding) from f`,
			"RANGE with offset PRECEDING/FOLLOWING requires exactly one ORDER BY column"},
		// RANGE offsets are distances on the sort key, so the key must
		// support the arithmetic (numeric here; no date/time types yet)
		// and the offset must be a sane size.
		{`select sum(v) over (order by k::text range 1 preceding) from f`,
			"RANGE with offset PRECEDING/FOLLOWING is not supported for column type string"},
		{`select sum(v) over (order by k > 1 range 1 preceding) from f`,
			"RANGE with offset PRECEDING/FOLLOWING is not supported for column type bool"},
		{`select sum(v) over (order by k range -1 preceding) from f`,
			"invalid preceding or following size in window function"},
		{`select sum(v) over (order by k range between current row and null following) from f`,
			"frame ending offset must not be null"},
		{`select sum(v) over (order by k range 'x' preceding) from f`,
			"frame starting offset must be numeric"},
		{`select sum(v) over (groups between 1 preceding and current row) from f`,
			"GROUPS mode requires an ORDER BY clause"},
		{`select sum(v) over (order by k rows between 1 preceding and current row exclude ties) from f`,
			"frame exclusion"},
		{`select sum(v) over (order by k rows v preceding) from f`,
			"argument of ROWS must not contain variables"},
		{`select sum(v) over (order by k groups count(v) preceding) from f`,
			"argument of GROUPS must be a simple expression"},
		{`select sum(v) over (order by k rows -1 preceding) from f`,
			"frame starting offset must not be negative"},
		{`select sum(v) over (order by k rows between current row and null following) from f`,
			"frame ending offset must not be null"},
		{`select sum(v) over (order by k rows 'x' preceding) from f`,
			"frame starting offset must be an integer"},
		// Half-written bounds are plain syntax errors (the want/got
		// detail rides in the error's attributes, not its message).
		{`select sum(v) over (order by k rows unbounded) from f`, "syntax error"},
		{`select sum(v) over (order by k rows current) from f`, "syntax error"},
	} {
		if _, err := d.Exec(c.q); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want containing %q", c.q, err, c.want)
		}
	}
}
