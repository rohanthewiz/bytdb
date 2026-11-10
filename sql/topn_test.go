package sql

// Deep-look D-43: ORDER BY ... LIMIT without a serving index collects
// through a bounded heap. Its rows must be exactly the stable full sort's
// rows OFFSET..OFFSET+LIMIT, ties broken by arrival as the sort breaks
// them.

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestTopNMatchesFullSort(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table tn (id int primary key, a int, b text, c float)`)
	rng := rand.New(rand.NewSource(43))
	var sb strings.Builder
	sb.WriteString(`insert into tn values `)
	for i := 0; i < 300; i++ {
		if i > 0 {
			sb.WriteString(", ")
		}
		a, b, c := "null", "null", "null"
		if rng.Intn(6) > 0 {
			a = fmt.Sprint(rng.Intn(5)) // heavy ties
		}
		if rng.Intn(6) > 0 {
			b = fmt.Sprintf("'%c'", 'p'+rng.Intn(4))
		}
		if rng.Intn(6) > 0 {
			c = fmt.Sprintf("%d.5", rng.Intn(3))
		}
		fmt.Fprintf(&sb, "(%d, %s, %s, %s)", i, a, b, c)
	}
	exec(t, d, sb.String())

	cols := []string{"a", "b", "c", "a + 1", "id % 7"}
	for iter := 0; iter < 300; iter++ {
		n := 1 + rng.Intn(3)
		var keys []string
		for _, k := range rng.Perm(len(cols))[:n] {
			dir := []string{"", " asc", " desc"}[rng.Intn(3)]
			keys = append(keys, cols[k]+dir)
		}
		where := []string{"", " where a is not null", " where c > 0", " where b <> 'q'"}[rng.Intn(4)]
		base := "select id, a, b, c from tn" + where + " order by " + strings.Join(keys, ", ")
		limit, offset := rng.Intn(40), rng.Intn(5)*rng.Intn(80)
		full := exec(t, d, base).Rows
		got := exec(t, d, fmt.Sprintf("%s limit %d offset %d", base, limit, offset)).Rows
		lo, hi := min(offset, len(full)), min(offset+limit, len(full))
		if want := full[lo:hi]; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s limit %d offset %d\n got  %v\n want %v", base, limit, offset, got, want)
		}
	}
}

// The heap holds OFFSET+LIMIT rows, so a large table under a small row
// cap answers a small top-N instead of refusing it.
func TestTopNHoldsOnlyLimitRows(t *testing.T) {
	d := openDB(t)
	seedNums(t, d, 200)
	d.e.SetMaxResultRows(20)
	r := exec(t, d, `select id from nums order by v desc limit 3 offset 2`)
	if fmt.Sprint(r.Rows) != "[[197] [196] [195]]" {
		t.Fatalf("top-3 after 2 = %v", r.Rows)
	}
	if _, err := d.Exec(`select id from nums order by v desc limit 25`); err == nil ||
		!strings.Contains(err.Error(), "result row limit") {
		t.Fatalf("a heap larger than the cap was not refused: %v", err)
	}
	if r := exec(t, d, `select id from nums order by v limit 0`); len(r.Rows) != 0 {
		t.Fatalf("limit 0 = %v", r.Rows)
	}
}
