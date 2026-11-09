package sqlexecaudit

import "testing"

func TestUpdateSeesOwnWrites(t *testing.T) {
	d, _ := open(t)
	must(t, d,
		`CREATE TABLE t (id int primary key, grp int, x int)`,
		`INSERT INTO t VALUES (1,1,10),(2,1,20),(3,1,30),(4,2,5)`,
	)
	// PG: every row sees the pre-statement snapshot: grp 1 -> 31 for all three rows, grp 2 -> 6.
	logq(t, d, `UPDATE t SET x = (SELECT max(t2.x) FROM t t2 WHERE t2.grp = t.grp) + 1`)
	logq(t, d, `SELECT id, x FROM t ORDER BY id`)
	must(t, d, `DELETE FROM t`, `INSERT INTO t VALUES (1,1,10),(2,1,20),(3,1,30),(4,2,5)`)
	// PG: count of rows with larger x in the pre-statement snapshot: 2,1,0,3
	logq(t, d, `UPDATE t SET x = (SELECT count(*) FROM t t2 WHERE t2.x > t.x)`)
	logq(t, d, `SELECT id, x FROM t ORDER BY id`)
	must(t, d, `DELETE FROM t`, `INSERT INTO t VALUES (1,1,10),(2,1,20),(3,1,30),(4,2,5)`)
	// PG: each RETURNING row sees pre-statement snapshot => 4,4,4,4
	logq(t, d, `DELETE FROM t RETURNING id, (SELECT count(*) FROM t)`)
	must(t, d, `INSERT INTO t VALUES (1,1,10),(2,1,20),(3,1,30),(4,2,5)`)
	logq(t, d, `DELETE FROM t RETURNING id, (SELECT count(*) FROM t t2 WHERE t2.grp = t.grp)`) // PG: 3,3,3,1
	must(t, d, `INSERT INTO t VALUES (1,1,10),(2,1,20),(3,1,30),(4,2,5)`)
	// swap-style: PG evaluates on snapshot
	logq(t, d, `UPDATE t SET x = (SELECT t2.x FROM t t2 WHERE t2.id = 5 - t.id)`) // PG: 5,30,20,10
	logq(t, d, `SELECT id, x FROM t ORDER BY id`)
}

func TestConvenience(t *testing.T) {
	d, _ := open(t)
	must(t, d,
		`CREATE TABLE t (id int primary key, grp int, x int, s text, ts timestamp)`,
		`CREATE TABLE t2 (id int primary key, grp int, x int, s text, ts timestamp)`,
		`INSERT INTO t VALUES (1,1,10,'a','2024-01-01 10:00:00'),(2,1,NULL,'b','2024-02-01 10:00:00'),(3,2,30,NULL,NULL)`,
	)
	for _, q := range []string{
		`INSERT INTO t2 SELECT * FROM t`,
		`INSERT INTO t2 (id, grp) SELECT id + 10, grp FROM t`,
		`UPDATE t SET x = t2.x FROM t2 WHERE t2.id = t.id`,
		`DELETE FROM t USING t2 WHERE t2.id = t.id AND t2.x > 1000`,
		`SELECT * FROM t WHERE id = 1 FOR UPDATE`,
		`SELECT * FROM t ORDER BY x NULLS FIRST`,
		`SELECT * FROM t ORDER BY x DESC NULLS LAST`,
		`SELECT id FROM t WHERE x IS DISTINCT FROM 10`,
		`SELECT id FROM t WHERE x IS NOT DISTINCT FROM NULL`,
		`SELECT count(*) FILTER (WHERE x > 5) FROM t`,
		`SELECT string_agg(s, ',') FROM t`,
		`SELECT array_agg(id) FROM t`,
		`SELECT bool_or(x > 5) FROM t`,
		`SELECT id FROM t INTERSECT SELECT id FROM t`,
		`SELECT id FROM t EXCEPT SELECT 1`,
		`SELECT CAST(x AS text) FROM t`,
		`SELECT EXTRACT(year FROM ts) FROM t`,
		`SELECT date_trunc('month', ts) FROM t`,
		`SELECT ts + interval '1 day' FROM t`,
		`SELECT now() - ts FROM t`,
		`SELECT substring(s from 1 for 1), substr(s, 1, 1), trim(s), replace(s, 'a', 'b'), concat(s, 'x') FROM t`,
		`SELECT abs(x), round(1.5), floor(1.5), ceil(1.5), greatest(1,2), least(1,2) FROM t`,
		`SELECT position('a' in s), left(s, 1), right(s,1), char_length(s), strpos(s, 'a') FROM t`,
		`SELECT random(), md5('a'), to_char(ts, 'YYYY') FROM t`,
		`SELECT * FROM generate_series(1, 3)`,
		`SELECT * FROM t FETCH FIRST 1 ROWS ONLY`,
		`SELECT * FROM t t1 JOIN t t3 USING (id)`,
		`SELECT grp, count(*) FROM t GROUP BY grp HAVING count(*) > 1 ORDER BY count(*) DESC`,
		`SELECT * FROM t WHERE (id, grp) = (1, 1)`,
		`SELECT * FROM t WHERE (id, grp) IN ((1, 1), (2, 1))`,
		`SELECT * FROM (VALUES (1, 'a'), (2, 'b')) v(a, b)`,
		`VALUES (1), (2)`,
		`SELECT x::numeric(10,2), x::decimal FROM t`,
		`SELECT id FROM t WHERE s LIKE 'a%' ESCAPE '!'`,
		`SELECT COUNT(*) OVER () FROM t`,
		`SELECT id, percent_rank() OVER (ORDER BY id), ntile(2) OVER (ORDER BY id) FROM t`,
		`WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n < 3) SELECT * FROM r`,
		`WITH d AS (DELETE FROM t2 RETURNING id) SELECT * FROM d`,
		`SELECT EXISTS (SELECT 1 FROM t WHERE id = 1)`,
		`SELECT 1 WHERE EXISTS (SELECT 1 FROM t)`,
		`SELECT * FROM t LIMIT ALL`,
		`SELECT DISTINCT ON (grp) grp, id FROM t ORDER BY grp, id`,
		`UPDATE t SET (x, s) = (1, 'z') WHERE id = 1`,
		`INSERT INTO t2 (id) VALUES (99) ON CONFLICT DO NOTHING RETURNING id`,
		`SELECT id FROM t ORDER BY random()`,
		`SELECT id, CASE WHEN x > 5 THEN 'hi' ELSE 'lo' END FROM t`,
		`SELECT id FROM t WHERE ts > now() - interval '7 days'`,
		`SELECT id FROM t WHERE ts::date = '2024-01-01'`,
		`SELECT count(DISTINCT grp), sum(DISTINCT x) FROM t`,
		`SELECT t.* , t2.id FROM t RIGHT JOIN t2 ON t.id = t2.id`,
		`SELECT id FROM t WHERE x = ANY(ARRAY[10, 30])`,
		`SELECT coalesce(max(x), 0) + 1 FROM t`,
		`SELECT max(x) - min(x) FROM t`,
		`TRUNCATE t2`,
		`SELECT id FROM t WHERE s IN ('a', 'b') AND x BETWEEN 1 AND 100`,
		`SELECT lower(s) AS l, count(*) FROM t GROUP BY l`,
		`SELECT s || '-' || id FROM t`,
		`SELECT id / 2.0, id % 2, -id, power(id, 2), sqrt(4) FROM t`,
		`SELECT current_timestamp, current_date, localtimestamp`,
		`SELECT id FROM t WHERE NOT (x > 5)`,
		`SELECT to_timestamp(0), age(ts) FROM t`,
		`EXPLAIN SELECT id FROM t WHERE id IN (1, 2, 3)`,
		`EXPLAIN SELECT id FROM t WHERE id = 1 OR id = 2`,
		`EXPLAIN SELECT id FROM t WHERE id = ANY('{1,2}')`,
		`EXPLAIN SELECT id FROM t WHERE id BETWEEN 1 AND 2`,
		`EXPLAIN SELECT id FROM t WHERE s LIKE 'a%'`,
	} {
		logq(t, d, q)
	}
}
