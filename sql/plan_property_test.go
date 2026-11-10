package sql

import (
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestPlannerEquivalenceProperty turns the whole planner into a checked
// equivalence: two tables carry identical rows, one richly indexed and
// one bare, and every random query must return the same rows from both
// — whatever access path the planner picks for the indexed table, its
// result must equal the only path the bare table has (seq scan + filter
// + sort). Ordered results are additionally checked against the ORDER
// BY spec directly, so an index-order shortcut that returns a plausible
// but wrong order fails even when both tables err identically.
//
// Bound-swap, composite-prefix, DESC-index, and tie-break regressions
// in the planner all surface here as a row-set or row-order mismatch.
func TestPlannerEquivalenceProperty(t *testing.T) {
	d := openDB(t)
	// uid, at and day cover the key types D-37 made pushable: uuid,
	// timestamp and date conjuncts reach point gets and bounded scans,
	// and must agree with the bare table's filter.
	exec(t, d, `create table ti (id int primary key, age int, city text, score float, uid uuid, at timestamp, day date)`)
	exec(t, d, `create table ts (id int primary key, age int, city text, score float, uid uuid, at timestamp, day date)`)

	rng := rand.New(rand.NewSource(1234))
	cities := []string{"london", "nyc", "austin", "sf"}
	scores := []string{"-2.5", "0.0", "1.5", "3.25", "7.0"}
	uids := []string{
		"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-0000000000a0",
		"7f000000-0000-4000-8000-000000000000", "80000000-0000-4000-8000-000000000000",
		"ffffffff-ffff-4fff-bfff-ffffffffffff",
	}
	ats := []string{"1969-12-31 23:59:59", "2024-01-01 00:00:00", "2024-01-01 00:00:01", "2024-06-15 12:30:00", "2031-03-04 05:06:07"}
	days := []string{"1960-01-01", "2024-01-01", "2024-01-02", "2024-06-15", "2031-03-04"}
	orNull := func(vals []string) string {
		if rng.Intn(7) == 0 {
			return "null"
		}
		return "'" + vals[rng.Intn(len(vals))] + "'"
	}
	for id := 1; id <= 80; id++ {
		age := "null"
		if rng.Intn(8) > 0 {
			age = fmt.Sprint(18 + rng.Intn(13))
		}
		row := fmt.Sprintf("(%d, %s, '%s', %s, %s, %s, %s)",
			id, age, cities[rng.Intn(len(cities))], scores[rng.Intn(len(scores))],
			orNull(uids), orNull(ats), orNull(days))
		exec(t, d, "insert into ti values "+row)
		exec(t, d, "insert into ts values "+row)
	}
	// Index shapes chosen to cover the planner's choices: single
	// column, composite with a DESC component, and DESC-leading.
	exec(t, d, `create index ti_age on ti (age)`)
	exec(t, d, `create index ti_city_age on ti (city, age desc)`)
	exec(t, d, `create index ti_score on ti (score desc)`)
	exec(t, d, `create index ti_uid on ti (uid)`)
	exec(t, d, `create index ti_day_at on ti (day, at desc)`)
	exec(t, d, `create index ti_at on ti (at)`)

	// Random WHERE: 0-2 type-correct conjuncts over indexed columns,
	// biased toward shapes the planner can serve from an index.
	randWhere := func() string {
		conj := func() string {
			age := func() int { return 16 + rng.Intn(17) }
			pick := func(vals []string) string { return vals[rng.Intn(len(vals))] }
			// some renders 1-4 values drawn from vals (repeats allowed),
			// quoted when q, for the IN-list shapes (D-38).
			some := func(vals []string, q bool) string {
				n := 1 + rng.Intn(4)
				parts := make([]string, n)
				for i := range parts {
					parts[i] = pick(vals)
					if q {
						parts[i] = "'" + parts[i] + "'"
					}
				}
				return strings.Join(parts, ", ")
			}
			ids := []string{"1", "5", "17", "40", "79", "80", "200"}
			ages := []string{"18", "21", "25", "30", "null"}
			switch rng.Intn(21) {
			case 0:
				return fmt.Sprintf("age = %d", age())
			case 1:
				return fmt.Sprintf("age > %d", age())
			case 2:
				return fmt.Sprintf("age >= %d and age < %d", age(), age())
			case 3:
				return fmt.Sprintf("city = '%s'", cities[rng.Intn(len(cities))])
			case 4:
				return fmt.Sprintf("city = '%s' and age > %d", cities[rng.Intn(len(cities))], age())
			case 5:
				return fmt.Sprintf("score > %s", scores[rng.Intn(len(scores))])
			case 6:
				return fmt.Sprintf("score <= %s", scores[rng.Intn(len(scores))])
			case 7:
				return "age is null"
			case 8:
				return fmt.Sprintf("uid = '%s'", pick(uids))
			case 9:
				return fmt.Sprintf("uid > '%s'::uuid", pick(uids))
			case 10:
				return fmt.Sprintf("at >= '%s'", pick(ats))
			case 11:
				return fmt.Sprintf("at < '%s'::timestamp", pick(ats))
			case 12:
				return fmt.Sprintf("day = '%s'", pick(days))
			case 13:
				return fmt.Sprintf("day = '%s' and at <= '%s'", pick(days), pick(ats))
			case 14:
				return fmt.Sprintf("id in (%s)", some(ids, false))
			case 15:
				return fmt.Sprintf("age in (%s)", some(ages, false))
			case 16:
				return fmt.Sprintf("city in (%s)", some(cities, true))
			case 17:
				return fmt.Sprintf("uid = any(array[%s]::uuid[])", some(uids, true))
			case 18:
				return fmt.Sprintf("(day = '%s' or day = '%s')", pick(days), pick(days))
			case 19:
				return fmt.Sprintf("age = any('{%s}')", strings.ReplaceAll(some(ages, false), " ", ""))
			default:
				return fmt.Sprintf("id = %d", 1+rng.Intn(90))
			}
		}
		switch rng.Intn(3) {
		case 0:
			return ""
		case 1:
			return " where " + conj()
		default:
			return " where " + conj() + " and " + conj()
		}
	}
	// Random ORDER BY over 0-2 keys with directions. Ties are legal and
	// common (small value domains), so results are compared as multisets
	// and the order is checked semantically instead of byte-for-byte.
	type orderKey struct {
		col  int // ordinal in the fixed projection below
		desc bool
	}
	orderCols := []string{"id", "age", "city", "score", "uid", "at", "day"}
	randOrder := func() (string, []orderKey) {
		n := rng.Intn(3)
		if n == 0 {
			return "", nil
		}
		perm := rng.Perm(len(orderCols))[:n]
		var parts []string
		var keys []orderKey
		for _, ord := range perm {
			k := orderKey{col: ord, desc: rng.Intn(2) == 0}
			dir := " asc"
			if k.desc {
				dir = " desc"
			}
			parts = append(parts, orderCols[ord]+dir)
			keys = append(keys, k)
		}
		return " order by " + strings.Join(parts, ", "), keys
	}

	indexed := 0
	for iter := range 400 {
		where := randWhere()
		orderBy, keys := randOrder()
		body := "select id, age, city, score, uid, at, day from %s" + where + orderBy

		ri := exec(t, d, fmt.Sprintf(body, "ti"))
		rs := exec(t, d, fmt.Sprintf(body, "ts"))

		// Same rows regardless of access path.
		mi, ms := multiset(ri.Rows), multiset(rs.Rows)
		if !slices.Equal(mi, ms) {
			t.Fatalf("iter %d %q: indexed and bare tables disagree\nti: %v\nts: %v",
				iter, fmt.Sprintf(body, "t*"), mi, ms)
		}

		// Both results must satisfy the ORDER BY spec: keys
		// non-decreasing per direction, NULLs last ascending / first
		// descending (orderCmp's contract, matching Postgres).
		for _, res := range []*Result{ri, rs} {
			for i := 1; i < len(res.Rows); i++ {
				c := 0
				for _, k := range keys {
					c = orderCmp(res.Rows[i-1][k.col], res.Rows[i][k.col])
					if k.desc {
						c = -c
					}
					if c != 0 {
						break
					}
				}
				if c > 0 {
					t.Fatalf("iter %d %q: rows %d,%d violate ORDER BY: %v then %v",
						iter, fmt.Sprintf(body, "ti/ts"), i-1, i, res.Rows[i-1], res.Rows[i])
				}
			}
		}

		// Track how often the planner actually served ti from an index,
		// so this test cannot silently degrade into seq-vs-seq.
		ex := exec(t, d, "explain "+fmt.Sprintf(body, "ti"))
		if len(ex.Rows) > 0 {
			if title, _ := ex.Rows[0][0].(string); strings.Contains(title, "Index Scan") ||
				strings.Contains(title, "Point Get") {
				indexed++
			}
		}
	}
	// With these predicates and indexes a healthy planner picks an
	// index path for a large share of queries; a collapse to zero would
	// mean the property degenerated into comparing seq scan to itself.
	if indexed < 50 {
		t.Fatalf("only %d/400 queries used an index path; planner coverage collapsed", indexed)
	}
	t.Logf("index-path queries: %d/400", indexed)
}

// multiset renders rows order-independently for set comparison.
func multiset(rows [][]any) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fmt.Sprintf("%#v", r)
	}
	sort.Strings(out)
	return out
}
