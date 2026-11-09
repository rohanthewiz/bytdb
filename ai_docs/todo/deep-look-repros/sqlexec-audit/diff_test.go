package sqlexecaudit

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// TestOrderedScanDifferential compares an indexed table against an
// index-free twin over random WHERE/ORDER BY/LIMIT shapes. The ORDER BY
// always ends in the unique id so the expected output is fully determined.
func TestOrderedScanDifferential(t *testing.T) {
	d, _ := open(t)
	must(t, d,
		// indexed: composite PK, asc/desc/multi-col indexes
		`CREATE TABLE ti (c int, d int, id int NOT NULL, a int, b int, e int NOT NULL, f float NOT NULL, s text NOT NULL, PRIMARY KEY (c, d))`,
		`CREATE INDEX ti_a ON ti (a)`,
		`CREATE INDEX ti_ab ON ti (a, b DESC)`,
		`CREATE INDEX ti_bd ON ti (b DESC)`,
		`CREATE INDEX ti_e ON ti (e, id)`,
		`CREATE INDEX ti_ed ON ti (e DESC, id DESC)`,
		`CREATE INDEX ti_f ON ti (f DESC, id)`,
		`CREATE INDEX ti_s ON ti (s, e DESC)`,
		`CREATE UNIQUE INDEX ti_id ON ti (id)`,
		// plain twin: only a PK on id
		`CREATE TABLE tp (c int, d int, id int PRIMARY KEY, a int, b int, e int NOT NULL, f float NOT NULL, s text NOT NULL)`,
	)
	r := rand.New(rand.NewSource(42))
	nv := func() string {
		if r.Intn(5) == 0 {
			return "NULL"
		}
		return fmt.Sprint(r.Intn(6))
	}
	id := 0
	for c := 0; c < 12; c++ {
		for dd := 0; dd < 12; dd++ {
			id++
			vals := fmt.Sprintf("(%d, %d, %d, %s, %s, %d, %d.5, '%c')", c, dd, id, nv(), nv(), r.Intn(5), r.Intn(4)-2, 'a'+rune(r.Intn(3)))
			must(t, d, `INSERT INTO ti VALUES `+vals, `INSERT INTO tp VALUES `+vals)
		}
	}
	cols := []string{"a", "b", "c", "d", "e", "f", "s", "id"}
	ops := []string{"=", "<", "<=", ">", ">=", "<>"}
	lit := func(col string) string {
		switch col {
		case "s":
			return fmt.Sprintf("'%c'", 'a'+rune(r.Intn(3)))
		case "f":
			if r.Intn(2) == 0 {
				return fmt.Sprint(r.Intn(4) - 2)
			}
			return fmt.Sprintf("%d.5", r.Intn(4)-2)
		case "id":
			return fmt.Sprint(r.Intn(150))
		case "c", "d":
			return fmt.Sprint(r.Intn(13))
		}
		return fmt.Sprint(r.Intn(6))
	}
	pred := func() string {
		col := cols[r.Intn(len(cols))]
		switch r.Intn(12) {
		case 0:
			return col + " IS NULL"
		case 1:
			return col + " IS NOT NULL"
		case 2:
			return fmt.Sprintf("%s BETWEEN %s AND %s", col, lit(col), lit(col))
		case 3:
			return fmt.Sprintf("%s IN (%s, %s)", col, lit(col), lit(col))
		case 4:
			return fmt.Sprintf("(%s = %s OR %s = %s)", col, lit(col), cols[r.Intn(len(cols))], "1")
		case 5:
			return fmt.Sprintf("%s %s %s", lit(col), ops[r.Intn(len(ops))], col)
		}
		return fmt.Sprintf("%s %s %s", col, ops[r.Intn(len(ops))], lit(col))
	}
	bad := 0
	for i := 0; i < 6000 && bad < 8; i++ {
		var w []string
		for k := r.Intn(4); k > 0; k-- {
			w = append(w, pred())
		}
		var ob []string
		for k := r.Intn(4); k > 0; k-- {
			o := cols[r.Intn(len(cols))]
			if r.Intn(2) == 0 {
				o += " DESC"
			}
			ob = append(ob, o)
		}
		if r.Intn(2) == 0 {
			ob = append(ob, "id DESC")
		} else {
			ob = append(ob, "id")
		}
		q := "SELECT id, a, b, c, d, e, f, s FROM %s"
		if len(w) > 0 {
			q += " WHERE " + strings.Join(w, " AND ")
		}
		q += " ORDER BY " + strings.Join(ob, ", ")
		if r.Intn(3) > 0 {
			q += fmt.Sprintf(" LIMIT %d", r.Intn(8))
			if r.Intn(2) == 0 {
				q += fmt.Sprintf(" OFFSET %d", r.Intn(5))
			}
		}
		gi := show(d, fmt.Sprintf(q, "ti"))
		gp := show(d, fmt.Sprintf(q, "tp"))
		if gi != gp {
			bad++
			t.Errorf("MISMATCH\n  q: %s\n  indexed: %s\n  plain:   %s\n  explain: %s", fmt.Sprintf(q, "ti"), gi, gp, show(d, "EXPLAIN "+fmt.Sprintf(q, "ti")))
		}
		// UPDATE/DELETE-style counts via count(*) (no order by) too
		if len(w) > 0 {
			cq := "SELECT count(*), sum(id) FROM %s WHERE " + strings.Join(w, " AND ")
			if a, b := show(d, fmt.Sprintf(cq, "ti")), show(d, fmt.Sprintf(cq, "tp")); a != b {
				bad++
				t.Errorf("COUNT MISMATCH\n  q: %s\n  indexed: %s\n  plain: %s", cq, a, b)
			}
		}
	}
}

// TestJoinDifferential compares joins over indexed tables with joins
// over index-free twins (so nested-loop index probes, hash joins and
// plain nested loops must all agree).
func TestJoinDifferential(t *testing.T) {
	d, _ := open(t)
	must(t, d,
		`CREATE TABLE xa (id int primary key, k int, f float, s text)`,
		`CREATE TABLE xb (id int primary key, k int, f float, s text)`,
		`CREATE TABLE xc (id int primary key, k int, f float, s text)`,
		`CREATE INDEX xb_k ON xb (k)`, `CREATE INDEX xc_f ON xc (f DESC)`, `CREATE INDEX xb_s ON xb (s, k)`,
		`CREATE TABLE ya (pk int primary key, id int, k int, f float, s text)`,
		`CREATE TABLE yb (pk int primary key, id int, k int, f float, s text)`,
		`CREATE TABLE yc (pk int primary key, id int, k int, f float, s text)`,
	)
	r := rand.New(rand.NewSource(7))
	nv := func(f bool) string {
		if r.Intn(5) == 0 {
			return "NULL"
		}
		if f {
			return fmt.Sprintf("%d.0", r.Intn(5))
		}
		return fmt.Sprint(r.Intn(5))
	}
	for _, tb := range []string{"a", "b", "c"} {
		for i := 1; i <= 14; i++ {
			k, f := nv(false), nv(true)
			s := fmt.Sprintf("'%c'", 'a'+rune(r.Intn(3)))
			must(t, d, fmt.Sprintf(`INSERT INTO x%s VALUES (%d, %s, %s, %s)`, tb, i, k, f, s),
				fmt.Sprintf(`INSERT INTO y%s VALUES (%d, %d, %s, %s, %s)`, tb, 100-i, i, k, f, s))
		}
	}
	cols := []string{"id", "k", "f", "s"}
	num := []string{"id", "k", "f"}
	ops := []string{"=", "=", "=", "<", ">=", "<>"}
	cond := func(l, rt string) string {
		if r.Intn(6) == 0 {
			return fmt.Sprintf("%s.s = %s.s", l, rt)
		}
		return fmt.Sprintf("%s.%s %s %s.%s", l, num[r.Intn(3)], ops[r.Intn(len(ops))], rt, num[r.Intn(3)])
	}
	local := func(tb string) string {
		c := cols[r.Intn(4)]
		switch r.Intn(4) {
		case 0:
			return fmt.Sprintf("%s.%s IS NULL", tb, c)
		case 1:
			return fmt.Sprintf("%s.%s IS NOT NULL", tb, c)
		}
		if c == "s" {
			return fmt.Sprintf("%s.s = 'a'", tb)
		}
		return fmt.Sprintf("%s.%s %s %d", tb, c, ops[r.Intn(len(ops))], r.Intn(5))
	}
	joins := []string{"JOIN", "LEFT JOIN"}
	bad := 0
	for i := 0; i < 5000 && bad < 8; i++ {
		j1, j2 := joins[r.Intn(2)], joins[r.Intn(2)]
		on1 := cond("b", "a")
		if r.Intn(3) == 0 {
			on1 += " AND " + local([]string{"a", "b"}[r.Intn(2)])
		}
		if r.Intn(4) == 0 {
			on1 += " AND " + cond("a", "b")
		}
		on2 := cond("c", []string{"a", "b"}[r.Intn(2)])
		if r.Intn(3) == 0 {
			on2 += " AND " + local([]string{"a", "b", "c"}[r.Intn(3)])
		}
		var w []string
		for k := r.Intn(3); k > 0; k-- {
			if r.Intn(3) == 0 {
				w = append(w, cond([]string{"a", "b", "c"}[r.Intn(3)], []string{"a", "b", "c"}[r.Intn(3)]))
			} else {
				w = append(w, local([]string{"a", "b", "c"}[r.Intn(3)]))
			}
		}
		q := "SELECT a.id, b.id, c.id FROM %[1]sa a " + j1 + " %[1]sb b ON " + on1 + " " + j2 + " %[1]sc c ON " + on2
		if len(w) > 0 {
			q += " WHERE " + strings.Join(w, " AND ")
		}
		q += " ORDER BY 1, 2, 3"
		gx, gy := show(d, fmt.Sprintf(q, "x")), show(d, fmt.Sprintf(q, "y"))
		if gx != gy {
			bad++
			t.Errorf("MISMATCH\n  q: %s\n  indexed: %s\n  plain:   %s\n  explain: %s", fmt.Sprintf(q, "x"), gx, gy, show(d, "EXPLAIN "+fmt.Sprintf(q, "x")))
		}
	}
}
