package sql

// Tricky-query suite: small queries that sit exactly on semantic
// boundaries where SQL engines historically diverge from Postgres —
// name-resolution precedence, three-valued logic shortcuts, integer
// arithmetic edges, NULL handling in DISTINCT/UNION/GROUP BY, window
// frame defaults, and LEFT JOIN ON-vs-WHERE filtering. Each expected
// value is Postgres's answer, so a failure here means a real semantic
// divergence rather than a crash (the fuzz targets own crash-hunting;
// this file owns wrong-answer and wrong-rejection hunting — the class
// of bug where a valid query errors or a query returns plausible but
// incorrect rows).
//
// TestOrderByFormulationEquivalence at the bottom generalizes the
// DISTINCT qualified-ORDER-BY fix: any two spellings of the same ORDER
// BY (bare name, qualified name, select-list position) must agree —
// either all succeed with the same rows or all fail.

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// seedEmp creates the shared fixture: duplicate salaries (100 and 120
// each appear twice) so ties exercise rank/frame/order edges, and two
// NULL depts so NULL-grouping and NULL-join behavior is reachable.
func seedEmp(t *testing.T, d *DB) {
	t.Helper()
	exec(t, d, `create table emp (id int primary key, name text, dept text, sal int, bonus float)`)
	exec(t, d, `insert into emp values
		(1, 'ada',    'eng', 100, 10.5),
		(2, 'grace',  'eng', 120, null),
		(3, 'alan',   'ops', 100, 7.25),
		(4, 'edsger', null,   90, null),
		(5, 'barb',   'ops', 120, 3.0),
		(6, 'don',    null,   80, 1.0)`)
}

// wantRows asserts an exact ordered result.
func wantRows(t *testing.T, d *DB, q string, want [][]any) {
	t.Helper()
	res := exec(t, d, q)
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("%s\n got: %v\nwant: %v", q, res.Rows, want)
	}
}

// TestTrickyNameResolution pins ORDER BY's two-tier name lookup: a bare
// name resolves against output aliases first and source columns second,
// while a qualified name is only ever a source-column reference — and
// once a table is aliased, its original name is no longer addressable.
func TestTrickyNameResolution(t *testing.T) {
	d := openDB(t)
	seedEmp(t, d)

	// The alias `id` shadows the real id column, so this orders by sal
	// (80,90,100,100,120,120), not by primary key. Ordering by the
	// source id instead would yield 100,120,100,90,120,80.
	wantRows(t, d, `select sal as id from emp order by id, name`, [][]any{
		{int64(80)}, {int64(90)}, {int64(100)}, {int64(100)}, {int64(120)}, {int64(120)},
	})

	// A plain (non-DISTINCT) select may order by a source column that
	// was never projected.
	wantRows(t, d, `select name from emp order by sal desc, name`, [][]any{
		{"barb"}, {"grace"}, {"ada"}, {"alan"}, {"edsger"}, {"don"},
	})

	// Positional ORDER BY counts select-list items, and mixes freely
	// with named keys.
	wantRows(t, d, `select name, sal from emp order by 2 desc, 1 asc limit 2`, [][]any{
		{"barb", int64(120)}, {"grace", int64(120)},
	})

	// Aliasing a table removes its original name from scope: emp.sal is
	// an invalid reference once the binding is `emp e` (Postgres rejects
	// this; accepting it would let queries silently bind to the wrong
	// relation in self-joins).
	execErr(t, d, `select e.sal from emp e order by emp.sal`)
}

// TestTrickyThreeValuedLogic pins the Kleene-logic edges: UNKNOWN is
// preserved through NOT, but AND/OR can still short-circuit to a known
// value when one side decides the result regardless of the other.
func TestTrickyThreeValuedLogic(t *testing.T) {
	d := openDB(t)
	seedEmp(t, d)

	// A literal `= NULL` comparison is rejected at parse time — a
	// deliberate divergence from Postgres (which silently returns
	// nothing) in favor of pointing the user at IS [NOT] NULL.
	if msg := execErr(t, d, `select count(*) from emp where dept = null`); !strings.Contains(msg, "IS [NOT] NULL") {
		t.Fatalf("= NULL error = %q; want the IS [NOT] NULL guidance", msg)
	}

	// UNKNOWN filtering through a column self-comparison: dept = dept
	// is UNKNOWN (not true) for the two NULL rows, and NOT(UNKNOWN) is
	// still UNKNOWN — so the two filters pass 4 and 0 rows, not
	// complementary sets summing to 6.
	wantRows(t, d, `select count(*) from emp where dept = dept`, [][]any{{int64(4)}})
	wantRows(t, d, `select count(*) from emp where not (dept = dept)`, [][]any{{int64(0)}})

	// A literal NULL as a BETWEEN bound gets the same parse-time
	// rejection as `= NULL`.
	if msg := execErr(t, d, `select count(*) from emp where sal between null and 200`); !strings.Contains(msg, "IS [NOT] NULL") {
		t.Fatalf("BETWEEN NULL error = %q; want the IS [NOT] NULL guidance", msg)
	}

	// A NULL that arrives through a column is UNKNOWN at runtime:
	// sal >= bonus is UNKNOWN for the two NULL-bonus rows, so BETWEEN
	// passes only the four rows with a real bonus.
	wantRows(t, d, `select count(*) from emp where sal between bonus and 200`, [][]any{{int64(4)}})

	// NULL AND false = false, NULL OR true = true: the known operand
	// decides. Engines that treat NULL as infectious get these wrong.
	wantRows(t, d, `select null and false, null or true`, [][]any{{false, true}})

	// CASE without ELSE yields NULL, and count(expr) skips NULLs — so a
	// never-true CASE counts to zero.
	wantRows(t, d, `select count(case when sal > 1000 then 1 end) from emp`, [][]any{{int64(0)}})

	// String concatenation is strict: any NULL operand nulls the result.
	wantRows(t, d, `select coalesce('x' || null, 'was null')`, [][]any{{"was null"}})
}

// TestTrickyIntegerArithmetic pins division/modulo semantics: integer
// division truncates toward zero (not floor), the remainder takes the
// dividend's sign, mixed int/float promotes to float, and int64
// overflow is a caught error rather than a silent wrap.
func TestTrickyIntegerArithmetic(t *testing.T) {
	d := openDB(t)

	wantRows(t, d, `select 7/2, -7/2, 7%2, -7%2, 7.0/2`, [][]any{
		{int64(3), int64(-3), int64(1), int64(-1), 3.5},
	})

	// The two overflow edges that survive naive checks: MaxInt64+1, and
	// MinInt64/-1 (the one quotient that is itself unrepresentable).
	// MinInt64 must be built by expression: the literal
	// -9223372036854775808 lexes as 9223372036854775808 under unary
	// minus, which exceeds int64 and becomes a float (Postgres likewise
	// promotes it to numeric), sidestepping integer overflow entirely.
	if msg := execErr(t, d, `select 9223372036854775807 + 1`); !strings.Contains(msg, "range") {
		t.Fatalf("overflow error = %q; want a range error", msg)
	}
	if msg := execErr(t, d, `select (-9223372036854775807 - 1) / -1`); !strings.Contains(msg, "range") {
		t.Fatalf("MinInt64/-1 error = %q; want a range error", msg)
	}
	// MinInt64 % -1 is 0, not a trap (both Go and Postgres guarantee
	// this), and the oversized literal path yields a float quotient.
	wantRows(t, d, `select (-9223372036854775807 - 1) % -1`, [][]any{{int64(0)}})
	wantRows(t, d, `select -9223372036854775808 / -1`, [][]any{{9.223372036854776e+18}})
}

// TestTrickyDistinctUnionNulls pins the "NULLs are distinct for =, but
// equal for grouping" split: DISTINCT and UNION dedup treat two NULLs
// as the same value even though NULL = NULL is UNKNOWN in a WHERE.
func TestTrickyDistinctUnionNulls(t *testing.T) {
	d := openDB(t)
	seedEmp(t, d)

	// Two NULL depts collapse to one DISTINCT row, ordered NULLS LAST
	// by the ascending default.
	wantRows(t, d, `select distinct dept from emp order by dept`, [][]any{
		{"eng"}, {"ops"}, {nil},
	})

	// UNION dedups across arms with the same NULLs-are-equal rule.
	wantRows(t, d, `select dept from emp union select dept from emp order by dept`, [][]any{
		{"eng"}, {"ops"}, {nil},
	})

	// UNION ALL keeps every duplicate: 6 rows per arm.
	wantRows(t, d,
		`select count(*) from (select dept from emp union all select dept from emp) x`,
		[][]any{{int64(12)}})

	// ORDER BY after a UNION sorts the combined result, not the last arm.
	wantRows(t, d, `select 1 as n union select 2 order by n desc`, [][]any{
		{int64(2)}, {int64(1)},
	})
}

// TestTrickyScalarSubqueries pins the scalar-subquery contract: zero
// rows is NULL (not an error), more than one row is an error (not a
// first-row pick), and a correlated subquery re-evaluates per outer row.
func TestTrickyScalarSubqueries(t *testing.T) {
	d := openDB(t)
	seedEmp(t, d)

	wantRows(t, d, `select coalesce((select sal from emp where false), -1)`, [][]any{{int64(-1)}})

	execErr(t, d, `select (select sal from emp)`)

	// Per-dept max via a correlated subquery, with coalesce making the
	// NULL-dept rows a comparable group of their own: eng max is grace
	// (120), ops max is barb (120), the NULL group's max is edsger (90).
	wantRows(t, d, `select name from emp o
		where sal = (select max(sal) from emp i where coalesce(i.dept,'') = coalesce(o.dept,''))
		order by name`,
		[][]any{{"barb"}, {"edsger"}, {"grace"}})

	// NOT IN against a list containing NULL can never be true: name <>
	// NULL is UNKNOWN, so every row's conjunction is at best UNKNOWN.
	wantRows(t, d, `select count(*) from emp where name not in (select dept from emp)`,
		[][]any{{int64(0)}})

	// EXISTS with an equality on a NULL key: NULL = NULL is UNKNOWN, so
	// NULL-dept rows find no partner even in their own table.
	wantRows(t, d,
		`select count(*) from emp o where exists (select 1 from emp i where i.dept = o.dept)`,
		[][]any{{int64(4)}})
}

// TestTrickyAggregateEdges pins the empty-input split (global
// aggregates yield one row of NULLs/zero, grouped aggregates yield no
// rows), HAVING without GROUP BY, and NULL-skipping in count(col).
func TestTrickyAggregateEdges(t *testing.T) {
	d := openDB(t)
	seedEmp(t, d)

	// Global aggregate over zero rows: exactly one row; count is 0, the
	// value aggregates are NULL.
	wantRows(t, d,
		`select sum(sal), count(*), count(dept), avg(sal), min(sal), max(sal) from emp where false`,
		[][]any{{nil, int64(0), int64(0), nil, nil, nil}})

	// The same empty input under GROUP BY produces no groups at all.
	wantRows(t, d, `select dept, count(*) from emp where false group by dept`, [][]any{})

	// HAVING without GROUP BY filters the single global row — legal, and
	// can leave an empty result.
	wantRows(t, d, `select count(*) from emp having count(*) > 100`, [][]any{})

	// count(col) skips NULLs; count(*) does not.
	wantRows(t, d, `select count(dept), count(*) from emp`, [][]any{{int64(4), int64(6)}})

	// GROUP BY puts all NULLs in one group, unlike `=` which would keep
	// them apart.
	wantRows(t, d, `select coalesce(dept, '-'), count(*) from emp group by dept order by 1`,
		[][]any{{"-", int64(2)}, {"eng", int64(2)}, {"ops", int64(2)}})

	// Sum types: pure-int input stays int64; a float in the expression
	// promotes the whole sum.
	wantRows(t, d, `select sum(sal), sum(sal + 0.0) from emp`, [][]any{{int64(610), 610.0}})
}

// TestTrickyWindowEdges pins the default-frame trap: with an ORDER BY
// and no explicit frame, the frame is RANGE UNBOUNDED PRECEDING TO
// CURRENT ROW, which includes the current row's *peers* — a running
// sum jumps by whole tie groups, it does not advance row by row.
func TestTrickyWindowEdges(t *testing.T) {
	d := openDB(t)
	seedEmp(t, d)

	// sals ascending: 80, 90, 100, 100, 120, 120. Peer-inclusive
	// running sums: 80, 170, 370, 370, 610, 610. A ROWS-mode default
	// would instead produce 270/370 and 490/610 at the ties.
	wantRows(t, d, `select sal, sum(sal) over (order by sal) from emp order by sal, id`, [][]any{
		{int64(80), int64(80)},
		{int64(90), int64(170)},
		{int64(100), int64(370)},
		{int64(100), int64(370)},
		{int64(120), int64(610)},
		{int64(120), int64(610)},
	})

	// rank leaves gaps after ties, dense_rank does not, row_number is
	// total given a deterministic order key.
	wantRows(t, d, `select name,
			rank() over (order by sal desc),
			dense_rank() over (order by sal desc),
			row_number() over (order by sal desc, name)
		from emp order by sal desc, name`, [][]any{
		{"barb", int64(1), int64(1), int64(1)},
		{"grace", int64(1), int64(1), int64(2)},
		{"ada", int64(3), int64(2), int64(3)},
		{"alan", int64(3), int64(2), int64(4)},
		{"edsger", int64(5), int64(3), int64(5)},
		{"don", int64(6), int64(4), int64(6)},
	})
}

// TestTrickyLeftJoinFilters pins the ON-vs-WHERE distinction: a
// predicate in ON decides *matching* (unmatched left rows survive,
// NULL-extended), while the same predicate in WHERE filters the joined
// result (discarding NULL-extended rows) — the difference between an
// outer join and an accidental inner join.
func TestTrickyLeftJoinFilters(t *testing.T) {
	d := openDB(t)
	seedEmp(t, d)
	// Deliberately partial dimension table: no 'ops' row, and an 'hr'
	// row no employee references.
	exec(t, d, `create table depts (dname text primary key, region text)`)
	exec(t, d, `insert into depts values ('eng', 'na'), ('hr', 'eu')`)

	// Every left row survives a LEFT JOIN regardless of match: 6 rows
	// (2 eng matched, 2 ops + 2 NULL-dept unmatched).
	wantRows(t, d, `select count(*) from emp e left join depts d on e.dept = d.dname`,
		[][]any{{int64(6)}})

	// Anti-join: keep only the unmatched left rows via IS NULL on the
	// right key. NULL-dept employees count as unmatched (NULL = NULL
	// found no partner).
	wantRows(t, d,
		`select count(*) from emp e left join depts d on e.dept = d.dname where d.dname is null`,
		[][]any{{int64(4)}})

	// An extra condition in ON restricts matching only: all 6 left rows
	// remain, but none matched ('eng' is 'na', not 'eu'), so the right
	// key is NULL everywhere.
	wantRows(t, d, `select count(*), count(d.dname)
		from emp e left join depts d on e.dept = d.dname and d.region = 'eu'`,
		[][]any{{int64(6), int64(0)}})

	// The same condition in WHERE runs after NULL-extension and kills
	// every row: unmatched rows have d.region NULL, matched rows have
	// 'na'.
	wantRows(t, d, `select count(*)
		from emp e left join depts d on e.dept = d.dname where d.region = 'eu'`,
		[][]any{{int64(0)}})

	// Comma FROM is a cross join: 6 × 2.
	wantRows(t, d, `select count(*) from emp, depts`, [][]any{{int64(12)}})
}

// TestTrickyOrderLimitEdges pins NULL placement defaults (ASC puts
// NULLs last, DESC puts them first — DESC is the reverse of ASC
// including the NULL block) and degenerate LIMIT/OFFSET.
func TestTrickyOrderLimitEdges(t *testing.T) {
	d := openDB(t)
	seedEmp(t, d)

	wantRows(t, d, `select distinct dept from emp order by dept desc`, [][]any{
		{nil}, {"ops"}, {"eng"},
	})

	wantRows(t, d, `select id from emp order by id limit 0`, [][]any{})
	wantRows(t, d, `select id from emp order by id offset 100`, [][]any{})

	// ORDER BY over a computed expression, with id as tiebreak:
	// sal % 7 maps 120→1, 100→2, 80→3, 90→6.
	wantRows(t, d, `select id from emp order by sal % 7, id`, [][]any{
		{int64(2)}, {int64(5)}, {int64(1)}, {int64(3)}, {int64(6)}, {int64(4)},
	})
}

// TestOrderByFormulationEquivalence is the property behind the DISTINCT
// qualified-ORDER-BY fix, generalized: for a random projection the same
// ORDER BY key is spelled three ways — bare name, qualified name, and
// select-list position — and every spelling must behave identically:
// all succeed with the same rows, or all fail. A formulation that
// errors while its siblings succeed (the fixed bug) or that silently
// binds to a different column (the dangerous variant) both surface as
// a mismatch here.
//
// Ties are common (small value domains), so rows are compared as
// multisets and each result is separately checked against the ORDER BY
// spec — byte-for-byte comparison would report false positives when two
// valid peer orders differ.
func TestOrderByFormulationEquivalence(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table fe (id int primary key, age int, city text, score float)`)
	rng := rand.New(rand.NewSource(4242))
	cities := []string{"london", "nyc", "austin"}
	scores := []string{"-1.5", "0.0", "2.5"}
	for id := 1; id <= 40; id++ {
		age := "null" // NULLs must sort identically across formulations too
		if rng.Intn(6) > 0 {
			age = fmt.Sprint(20 + rng.Intn(5))
		}
		exec(t, d, fmt.Sprintf(`insert into fe values (%d, %s, '%s', %s)`,
			id, age, cities[rng.Intn(len(cities))], scores[rng.Intn(len(scores))]))
	}

	cols := []string{"id", "age", "city", "score"}
	for iter := range 300 {
		// Random projection of 1–3 distinct columns; DISTINCT half the
		// time (the historically buggy path); alias the table half the
		// time so qualification is exercised through both alias and
		// table-name bindings.
		perm := rng.Perm(len(cols))[:1+rng.Intn(3)]
		distinct, alias := rng.Intn(2) == 0, rng.Intn(2) == 0
		binding, fromClause := "fe", "fe"
		if alias {
			binding, fromClause = "e", "fe e"
		}

		var items []string
		for _, c := range perm {
			items = append(items, cols[c])
		}
		kw := ""
		if distinct {
			kw = "distinct "
		}
		base := "select " + kw + strings.Join(items, ", ") + " from " + fromClause

		// One ORDER BY key drawn from the projection, in each spelling.
		k := rng.Intn(len(perm))
		dir := ""
		if rng.Intn(2) == 0 {
			dir = " desc"
		}
		bare := cols[perm[k]]
		forms := []string{
			base + " order by " + bare + dir,
			base + " order by " + binding + "." + bare + dir,
			base + fmt.Sprintf(" order by %d%s", k+1, dir),
		}

		var rows [][][]any
		var errs []error
		for _, q := range forms {
			res, err := d.Exec(q)
			errs = append(errs, err)
			if err == nil {
				rows = append(rows, res.Rows)
			}
		}
		for i := 1; i < len(errs); i++ {
			if (errs[i] == nil) != (errs[0] == nil) {
				t.Fatalf("iter %d: formulations disagree on validity:\n%s -> %v\n%s -> %v",
					iter, forms[0], errs[0], forms[i], errs[i])
			}
		}
		if errs[0] != nil {
			continue
		}
		for i := 1; i < len(rows); i++ {
			if !slices.Equal(multiset(rows[0]), multiset(rows[i])) {
				t.Fatalf("iter %d: formulations disagree on rows:\n%s -> %v\n%s -> %v",
					iter, forms[0], rows[0], forms[i], rows[i])
			}
		}
		// Each result must independently satisfy the ORDER BY key —
		// multiset equality alone would accept three identically wrong
		// orders.
		desc := dir != ""
		for fi, rs := range rows {
			for i := 1; i < len(rs); i++ {
				c := orderCmp(rs[i-1][k], rs[i][k])
				if desc {
					c = -c
				}
				if c > 0 {
					t.Fatalf("iter %d %q: rows %d,%d out of order: %v then %v",
						iter, forms[fi], i-1, i, rs[i-1], rs[i])
				}
			}
		}
	}
}
