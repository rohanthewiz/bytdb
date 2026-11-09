package sqlexecaudit

import (
	"sync"
	"testing"
)

// Prepared statements are documented safe for concurrent use; run every
// executor shape from several goroutines under -race.
func TestPreparedRace(t *testing.T) {
	d, _ := open(t)
	must(t, d,
		`CREATE TABLE a (id int primary key, k int, s text)`,
		`CREATE TABLE b (id int primary key, aid int, v float)`,
		`CREATE INDEX b_aid ON b (aid)`,
		`INSERT INTO a VALUES (1,1,'x'),(2,2,'y'),(3,NULL,'3')`,
		`INSERT INTO b VALUES (1,1,1.5),(2,1,2.5),(3,2,3.5)`,
		`CREATE VIEW vv AS SELECT a.id, count(*) AS n FROM a JOIN b ON b.aid = a.id GROUP BY a.id`,
	)
	qs := []string{
		`SELECT a.id, b.v FROM a JOIN b ON b.aid = a.id WHERE a.k = '1' ORDER BY b.v DESC`,
		`SELECT a.id, (SELECT count(*) FROM b WHERE b.aid = a.id) FROM a WHERE a.id IN (SELECT aid FROM b) ORDER BY 1`,
		`SELECT k, count(*), sum(id) + 1 FROM a GROUP BY k HAVING count(*) >= 1 ORDER BY 2 DESC, k`,
		`SELECT id, row_number() OVER (PARTITION BY k ORDER BY id), sum(id) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) FROM a`,
		`SELECT DISTINCT k FROM a ORDER BY k`,
		`SELECT id FROM a UNION SELECT aid FROM b ORDER BY 1`,
		`WITH c AS (SELECT * FROM a WHERE s = 'x') SELECT * FROM c JOIN vv ON vv.id = c.id`,
		`SELECT * FROM a WHERE s = '3' AND id = '3' AND EXISTS (SELECT 1 FROM b WHERE b.aid < a.id)`,
		`SELECT a.id FROM a LEFT JOIN b ON b.aid = a.id AND b.v > 2 WHERE b.id IS NULL ORDER BY a.id LIMIT 2`,
		`EXPLAIN SELECT a.id FROM a JOIN b ON b.aid = a.id WHERE a.id = '2'`,
	}
	want := make([]string, len(qs))
	for i, q := range qs {
		want[i] = show(d, q)
	}
	var wg sync.WaitGroup
	for i, q := range qs {
		st, err := d.Prepare(q)
		if err != nil {
			t.Fatal(q, err)
		}
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := 0; n < 200; n++ {
					res, err := st.Exec()
					if err != nil {
						t.Errorf("%s: %v", q, err)
						return
					}
					_ = res
				}
			}()
		}
		_ = i
	}
	wg.Wait()
	for i, q := range qs {
		if got := show(d, q); got != want[i] {
			t.Errorf("result drifted after concurrent prepared runs: %s\n %s\n %s", q, got, want[i])
		}
	}
}

func TestUnionAggMisc(t *testing.T) {
	d, _ := open(t)
	must(t, d, `CREATE TABLE t (id int primary key, x int, s text)`,
		`INSERT INTO t VALUES (1, 10, 'a'), (2, 20, 'b'), (3, NULL, 'a')`,
		`CREATE TABLE a (id int primary key)`, `CREATE TABLE b (id int primary key)`,
		`INSERT INTO a VALUES (1),(2)`, `INSERT INTO b VALUES (1),(2)`)
	for _, q := range []string{
		`SELECT id FROM t UNION ALL SELECT id FROM t ORDER BY id LIMIT 1`,
		`SELECT id FROM t UNION ALL SELECT id FROM t LIMIT 4`,
		`(SELECT id FROM t ORDER BY id DESC LIMIT 1) UNION ALL (SELECT id FROM t ORDER BY id LIMIT 1)`,
		`SELECT id FROM t WHERE id = 1 UNION ALL SELECT id FROM t ORDER BY id DESC LIMIT 1 OFFSET 1`,
		`SELECT x FROM t UNION SELECT x FROM t`,
		`SELECT count(*) FROM t LIMIT 0`,
		`SELECT s FROM t GROUP BY s ORDER BY count(*) DESC, s`,
		`SELECT s, sum(x) FROM t GROUP BY s ORDER BY sum(x) DESC`,
		`SELECT s AS x, count(*) FROM t GROUP BY s ORDER BY x`,
		`SELECT x AS s FROM t ORDER BY s`,
		`SELECT DISTINCT s FROM t ORDER BY s DESC LIMIT 1`,
		`SELECT min(s), max(s), min(x), max(x), avg(x), count(x), count(*) FROM t`,
		`SELECT sum(x) FILTER (WHERE x > 10) FROM t`,
		`SELECT x.id FROM (SELECT a.id, b.id FROM a JOIN b ON a.id = b.id) x`,
		`SELECT id, rank() OVER (ORDER BY s), dense_rank() OVER (ORDER BY s), lag(x) OVER (ORDER BY id), first_value(x) OVER (PARTITION BY s ORDER BY id DESC), sum(x) OVER (PARTITION BY s) FROM t ORDER BY id`,
		`SELECT id, sum(x) OVER (ORDER BY x) FROM t ORDER BY id`,
		`SELECT id, count(*) OVER (ORDER BY s), last_value(id) OVER (ORDER BY s) FROM t ORDER BY id`,
		`SELECT id, sum(x) OVER (ORDER BY id ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING), avg(x) OVER (ORDER BY id RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM t ORDER BY id`,
		`SELECT id FROM t WHERE x > ALL (SELECT x FROM t)`,
		`SELECT id FROM t WHERE x NOT IN (SELECT x FROM t WHERE id = 3)`,
		`SELECT id FROM t WHERE id NOT IN (SELECT id FROM t WHERE x IS NULL)`,
		`SELECT id FROM t ORDER BY x`,
		`SELECT id FROM t ORDER BY x DESC`,
		`SELECT id FROM t ORDER BY x DESC LIMIT 1`,
	} {
		logq(t, d, q)
	}
}
