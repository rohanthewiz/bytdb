package sqlexecaudit

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// nint is a nullable int for the brute-force oracle.
type nint struct {
	v    int
	null bool
}

func (n nint) sql() string {
	if n.null {
		return "NULL"
	}
	return fmt.Sprint(n.v)
}

// cmp3 is SQL three-valued comparison: 1 true, 0 false, -1 unknown.
func cmp3(a nint, op string, b nint) int {
	if a.null || b.null {
		return -1
	}
	var r bool
	switch op {
	case "=":
		r = a.v == b.v
	case "<":
		r = a.v < b.v
	case ">=":
		r = a.v >= b.v
	case "<>":
		r = a.v != b.v
	case ">":
		r = a.v > b.v
	case "<=":
		r = a.v <= b.v
	}
	if r {
		return 1
	}
	return 0
}

type term struct {
	text string
	eval func(a, b [2]nint) int // a,b = (id,k) rows; b may be all-NULL
}

// TestLeftJoinOracle checks LEFT/INNER JOIN with ON vs WHERE conjuncts
// against a brute-force three-valued evaluation, over an indexed inner
// table (index nested loop) and an unindexed one (hash join / nested loop).
func TestLeftJoinOracle(t *testing.T) {
	d, _ := open(t)
	must(t, d,
		`CREATE TABLE la (id int primary key, k int)`,
		`CREATE TABLE lb (id int primary key, k int)`, `CREATE INDEX lb_k ON lb (k)`,
		`CREATE TABLE lc (pk int primary key, id int, k int)`,
	)
	r := rand.New(rand.NewSource(3))
	var A, B [][2]nint
	for i := 1; i <= 9; i++ {
		a := [2]nint{{v: i}, {v: r.Intn(4), null: r.Intn(4) == 0}}
		b := [2]nint{{v: i}, {v: r.Intn(4), null: r.Intn(4) == 0}}
		A, B = append(A, a), append(B, b)
		must(t, d, fmt.Sprintf(`INSERT INTO la VALUES (%s, %s)`, a[0].sql(), a[1].sql()),
			fmt.Sprintf(`INSERT INTO lb VALUES (%s, %s)`, b[0].sql(), b[1].sql()),
			fmt.Sprintf(`INSERT INTO lc VALUES (%d, %s, %s)`, 100-i, b[0].sql(), b[1].sql()))
	}
	ops := []string{"=", "=", "<", ">=", "<>", ">", "<="}
	colNames := []string{"id", "k"}
	mk := func() term {
		op := ops[r.Intn(len(ops))]
		switch r.Intn(7) {
		case 0, 1: // b.col op a.col
			bc, ac := r.Intn(2), r.Intn(2)
			return term{fmt.Sprintf("b.%s %s a.%s", colNames[bc], op, colNames[ac]),
				func(a, b [2]nint) int { return cmp3(b[bc], op, a[ac]) }}
		case 2: // a.col op b.col
			bc, ac := r.Intn(2), r.Intn(2)
			return term{fmt.Sprintf("a.%s %s b.%s", colNames[ac], op, colNames[bc]),
				func(a, b [2]nint) int { return cmp3(a[ac], op, b[bc]) }}
		case 3:
			c, n := r.Intn(2), r.Intn(5)
			return term{fmt.Sprintf("b.%s %s %d", colNames[c], op, n),
				func(a, b [2]nint) int { return cmp3(b[c], op, nint{v: n}) }}
		case 4:
			c, n := r.Intn(2), r.Intn(5)
			return term{fmt.Sprintf("a.%s %s %d", colNames[c], op, n),
				func(a, b [2]nint) int { return cmp3(a[c], op, nint{v: n}) }}
		case 5:
			c := r.Intn(2)
			return term{fmt.Sprintf("b.%s IS NULL", colNames[c]),
				func(a, b [2]nint) int {
					if b[c].null {
						return 1
					}
					return 0
				}}
		}
		c := r.Intn(2)
		return term{fmt.Sprintf("b.%s IS NOT NULL", colNames[c]),
			func(a, b [2]nint) int {
				if b[c].null {
					return 0
				}
				return 1
			}}
	}
	all := func(ts []term, a, b [2]nint) bool {
		for _, tm := range ts {
			if tm.eval(a, b) != 1 {
				return false
			}
		}
		return true
	}
	texts := func(ts []term) string {
		s := make([]string, len(ts))
		for i, tm := range ts {
			s[i] = tm.text
		}
		return strings.Join(s, " AND ")
	}
	bad := 0
	for i := 0; i < 20000 && bad < 6; i++ {
		left := r.Intn(3) > 0
		var on, wh []term
		for k := 1 + r.Intn(3); k > 0; k-- {
			on = append(on, mk())
		}
		for k := r.Intn(3); k > 0; k-- {
			wh = append(wh, mk())
		}
		// oracle
		var want []string
		null2 := [2]nint{{null: true}, {null: true}}
		for _, a := range A {
			matched := false
			for _, b := range B {
				if all(on, a, b) {
					matched = true
					if all(wh, a, b) {
						want = append(want, fmt.Sprintf("[%s %s]", a[0].sql(), b[0].sql()))
					}
				}
			}
			if left && !matched && all(wh, a, null2) {
				want = append(want, fmt.Sprintf("[%s <nil>]", a[0].sql()))
			}
		}
		sort.Strings(want)
		for _, inner := range []string{"lb", "lc"} {
			j := "JOIN"
			if left {
				j = "LEFT JOIN"
			}
			q := fmt.Sprintf("SELECT a.id, b.id FROM la a %s %s b ON %s", j, inner, texts(on))
			if len(wh) > 0 {
				q += " WHERE " + texts(wh)
			}
			res, err := d.Exec(q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			var got []string
			for _, row := range res.Rows {
				got = append(got, fmt.Sprintf("[%v %v]", row[0], row[1]))
			}
			sort.Strings(got)
			if strings.Join(got, "") != strings.Join(want, "") {
				bad++
				t.Errorf("MISMATCH %s\n got  %v\n want %v\n %s", q, got, want, show(d, "EXPLAIN "+q))
			}
		}
	}
}
