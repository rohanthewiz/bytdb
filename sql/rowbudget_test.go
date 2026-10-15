package sql

// rowbudget_test.go: the result row cap and the memory ceiling as the
// SQL executor enforces them — every materialization that charges the
// budget, the shapes that must stay exempt (streaming aggregates,
// early-stopping LIMITs), and DELETE's exemption from the ceiling.

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

// seedNums creates nums(id, grp, v) with n rows: grp cycles 0..2, so a
// GROUP BY grp is three groups whatever n is.
func seedNums(t *testing.T, d *DB, n int) {
	t.Helper()
	exec(t, d, `create table nums (id int primary key, grp int, v int)`)
	var b strings.Builder
	b.WriteString(`insert into nums values `)
	for i := range n {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "(%d, %d, %d)", i, i%3, i*10)
	}
	exec(t, d, b.String())
}

func TestResultRowCap(t *testing.T) {
	d := openDB(t)
	seedNums(t, d, 10)
	exec(t, d, `create table other (id int primary key, nid int)`)
	exec(t, d, `insert into other values (1, 10), (2, 20), (3, 30)`)
	d.e.SetMaxResultRows(5)

	over := []string{
		`select * from nums`,
		`select * from nums order by v desc limit 3`, // unindexed sort holds every row
		`select id, count(*) from nums group by id`,  // ten groups
		`select id, row_number() over (order by id) from nums`,
		`select id from nums where id = 1 and id in (select id from nums)`,             // 1-row result, 10-value subquery set
		`select n.id from other o join nums n on n.v = o.nid`,                          // 3-row result, 10-row hash build side
		`select id from nums where id < 3 union all select id from nums where id >= 7`, // 3+3 concatenated
		`with c as (select * from nums) select count(*) from c`,                        // CTE materialization
		`select array(select id from nums)`,
		`insert into nums values (100, 0, 0), (101, 0, 0), (102, 0, 0), (103, 0, 0), (104, 0, 0), (105, 0, 0) returning id`,
	}
	for _, q := range over {
		_, err := d.Exec(q)
		if err == nil || !strings.Contains(err.Error(), "result row limit") {
			t.Errorf("%s: err = %v, want the result row limit refusal", q, err)
		}
	}

	under := []string{
		`select * from nums limit 5`,                  // collection stops at the LIMIT
		`select * from nums order by id limit 5`,      // PK order: no sort, early stop
		`select count(*), sum(v) from nums`,           // streams: one group
		`select grp, count(*) from nums group by grp`, // three groups
		`select * from nums where id < 5`,
	}
	for _, q := range under {
		if _, err := d.Exec(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}

	// The refused INSERT ... RETURNING rolled back as a whole.
	if res := exec(t, d, `select count(*) from nums`); res.Rows[0][0] != int64(10) {
		t.Errorf("count after refused insert = %v, want 10", res.Rows[0][0])
	}

	// DELETE ... RETURNING is held to the cap too (the ceiling exempts
	// DELETE, the cap does not: its output is still a result set).
	if _, err := d.Exec(`delete from nums returning id`); err == nil ||
		!strings.Contains(err.Error(), "result row limit") {
		t.Errorf("delete returning 10 rows: err = %v, want the row limit refusal", err)
	}

	// Disabling the cap lets everything through.
	d.e.SetMaxResultRows(0)
	for _, q := range over[:len(over)-1] {
		if _, err := d.Exec(q); err != nil {
			t.Errorf("cap disabled: %s: %v", q, err)
		}
	}
}

// TestMemoryCeilingSQL drives the ceiling through SQL with an explicit
// one-byte ceiling — always exceeded once any GC has run — so no fake
// heap reading is needed from outside the bytdb package.
func TestMemoryCeilingSQL(t *testing.T) {
	d := openDB(t)
	// Past memCheckEvery rows, so a full read polls the ceiling.
	const n = 3 * memCheckEvery
	exec(t, d, `create table nums (id int primary key, grp int, v int)`)
	for lo := 0; lo < n; lo += 500 {
		var b strings.Builder
		b.WriteString(`insert into nums values `)
		for i := lo; i < min(lo+500, n); i++ {
			if i > lo {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "(%d, %d, %d)", i, i%3, i)
		}
		exec(t, d, b.String())
	}
	runtime.GC() // populate /gc/heap/live, which the guard reads
	d.e.SetMemoryCeiling(1)
	t.Cleanup(func() { d.e.SetMemoryCeiling(0) })

	isOOM := func(q string) {
		t.Helper()
		_, err := d.Exec(q)
		if !errors.Is(err, bytdb.ErrOutOfMemory) {
			t.Errorf("%s: err = %v, want ErrOutOfMemory", q, err)
		}
	}
	isOOM(`insert into nums values (999999, 0, 0)`)
	isOOM(`update nums set v = 0 where id = 1`)
	isOOM(`select * from nums`) // materializes 3072 rows
	isOOM(`create index nums_v on nums (v)`)

	// Small reads and streaming aggregates never reach a poll.
	for _, q := range []string{
		`select * from nums where id = 7`,
		`select count(*) from nums`,
		`select grp, sum(v) from nums group by grp`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}

	// DELETE is the way back under the ceiling: it is exempt even when
	// its RETURNING list and subquery key set cross polling points.
	res, err := d.Exec(`delete from nums where id in (select id from nums where id >= 10) returning id`)
	if err != nil {
		t.Fatalf("delete under the ceiling: %v", err)
	}
	if len(res.Rows) != n-10 {
		t.Errorf("delete returned %d rows, want %d", len(res.Rows), n-10)
	}
}
