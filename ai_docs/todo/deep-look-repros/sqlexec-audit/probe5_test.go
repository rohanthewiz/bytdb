package sqlexecaudit

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// A DML statement that reads a view in autocommit: the view is
// materialized on one snapshot, the statement body runs in a later
// write transaction.
func TestViewDMLSnapshot(t *testing.T) {
	d, _ := open(t)
	must(t, d,
		`CREATE TABLE t (id int primary key)`,
		`CREATE TABLE log (id int primary key, via_view int, direct int)`,
		`CREATE VIEW v AS SELECT count(*) AS n FROM t`,
	)
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			d.Exec(`INSERT INTO t VALUES ($1)`, int64(i))
		}
	}()
	for i := 0; i < 300; i++ {
		if _, err := d.Exec(`INSERT INTO log VALUES ($1, (SELECT n FROM v), (SELECT count(*) FROM t))`, int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("rows where the view's count != the direct count in the SAME statement: %s",
		show(d, `SELECT count(*) FROM log WHERE via_view <> direct`))
	t.Logf("example: %s", show(d, `SELECT * FROM log WHERE via_view <> direct LIMIT 3`))
}

func TestDistinctBudget(t *testing.T) {
	d, e := open(t)
	must(t, d, `CREATE TABLE big (id int primary key, v int)`)
	load(t, d, "big", 20000, func(i int) string { return fmt.Sprintf("(%d, %d)", i, i) })
	e.SetMaxResultRows(1000)
	logq(t, d, `SELECT DISTINCT v FROM big`)
	logq(t, d, `SELECT v FROM big GROUP BY v`)
	logq(t, d, `SELECT count(DISTINCT v) FROM big`) // holds 20000 values in the seen-set, cap is 1000
	logq(t, d, `SELECT count(*) FROM big WHERE v IN (SELECT v FROM big)`)
	logq(t, d, `SELECT count(*) FROM (SELECT v, row_number() OVER (ORDER BY v) FROM big LIMIT 5) x`)
}

func TestPanicHunt(t *testing.T) {
	d, _ := open(t)
	must(t, d, `CREATE TABLE t (id int primary key, x int, f float, s text, j jsonb, a text[])`,
		`INSERT INTO t VALUES (1, 1, 1.5, 'a', '{"a":[1,2,{"b":null}]}', '{x,y}'), (2, NULL, NULL, NULL, NULL, NULL)`)
	for _, q := range []string{
		`SELECT * FROM t GROUP BY 0`,
		`SELECT x FROM t GROUP BY 2`,
		`SELECT count(*) FROM t GROUP BY 1`,
		`SELECT * FROM t ORDER BY 0`,
		`SELECT * FROM t ORDER BY 99`,
		`SELECT lag(x, 9223372036854775807) OVER (ORDER BY id), lead(x, -9223372036854775808) OVER (ORDER BY id) FROM t`,
		`SELECT nth_value(x, 9223372036854775807) OVER (ORDER BY id) FROM t`,
		`SELECT sum(x) OVER (ORDER BY id ROWS BETWEEN 9223372036854775807 PRECEDING AND 9223372036854775807 FOLLOWING) FROM t`,
		`SELECT sum(x) OVER (ORDER BY x RANGE BETWEEN 9223372036854775807 PRECEDING AND 9223372036854775807 FOLLOWING) FROM t`,
		`SELECT sum(x) OVER (ORDER BY id GROUPS BETWEEN 3 FOLLOWING AND 1 PRECEDING) FROM t`,
		`SELECT j -> -9223372036854775808, j #> '{a,-9223372036854775808}', j - -9223372036854775808 FROM t`,
		`SELECT j #> '{a,2,b,c}', j -> 'a' -> 5, j #>> '{}' FROM t`,
		`SELECT a[0], a[99], a[-1] FROM t`,
		`SELECT array_length(a, 0), array_length(a, 2), array_to_string(a, NULL) FROM t`,
		`SELECT x / 0, x % 0, f / 0 FROM t`,
		`SELECT -9223372036854775808 / -1`,
		`SELECT (-9223372036854775807 - 1) / -1, (-9223372036854775807 - 1) % -1`,
		`SELECT 9223372036854775807 + 1`,
		`SELECT sum(x), sum(9223372036854775807) FROM t`,
		`SELECT avg(9223372036854775807) FROM t`,
		`SELECT * FROM t LIMIT 0 OFFSET 9223372036854775807`,
		`SELECT * FROM t t1, t t2, t t3 WHERE t1.id = t2.id AND t2.id = t3.id AND t3.x = t1.x`,
		`SELECT (SELECT (SELECT (SELECT t.x))) FROM t`,
		`SELECT count(*) FROM t HAVING count(*) > (SELECT count(*) FROM t WHERE x > 100)`,
		`SELECT x, count(*) OVER (PARTITION BY x), sum(count(*)) OVER () FROM t GROUP BY x`,
		`SELECT max(x) OVER (ORDER BY id), min(s) OVER (ORDER BY id DESC) FROM t ORDER BY 1 DESC LIMIT 1`,
		`UPDATE t SET id = id + 1`,
		`UPDATE t SET id = id - 1 RETURNING id`,
		`SELECT 1 FROM t WHERE s ~ '('`,
		`SELECT 1 FROM t WHERE s LIKE '\'`,
		`SELECT CASE WHEN x = 1 THEN 'a' ELSE 2 END FROM t ORDER BY 1`,
		`SELECT x FROM t UNION SELECT s FROM t ORDER BY 1`,
		`SELECT DISTINCT j, a FROM t ORDER BY 1`,
		`INSERT INTO t VALUES (3, 1, 1, 's', '[]', '{}') ON CONFLICT (id) DO UPDATE SET x = excluded.x + t.x RETURNING *`,
		`INSERT INTO t (id) VALUES (3), (3) ON CONFLICT (id) DO UPDATE SET x = 5`,
		`INSERT INTO t (id) VALUES (7), (7) ON CONFLICT (id) DO UPDATE SET x = 5`,
		`SELECT * FROM t ORDER BY id`,
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("PANIC on %s: %v", q, r)
				}
			}()
			logq(t, d, q)
		}()
	}
}
