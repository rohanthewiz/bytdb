package sqlexecaudit

import "testing"

func TestTypePushdown(t *testing.T) {
	d, _ := open(t)
	must(t, d,
		`CREATE TABLE u (id uuid primary key, ts timestamp, dt date, v int)`,
		`CREATE INDEX u_ts ON u (ts)`,
		`CREATE INDEX u_dt ON u (dt)`,
		`INSERT INTO u VALUES ('00000000-0000-0000-0000-000000000001', '2024-01-01 00:00:00', '2024-01-01', 1)`,
		`INSERT INTO u VALUES ('00000000-0000-0000-0000-000000000002', '2024-06-01 00:00:00', '2024-06-01', 2)`,
	)
	logq(t, d, `EXPLAIN SELECT * FROM u WHERE id = '00000000-0000-0000-0000-000000000001'`)
	logq(t, d, `EXPLAIN SELECT * FROM u WHERE ts >= '2024-03-01'`)
	logq(t, d, `EXPLAIN SELECT * FROM u WHERE dt = '2024-06-01'`)
	logq(t, d, `SELECT v FROM u WHERE id = '00000000-0000-0000-0000-000000000001'`)
	logq(t, d, `SELECT v FROM u WHERE ts >= '2024-03-01'`)
	logq(t, d, `SELECT v FROM u WHERE dt = '2024-06-01'`)
	logq(t, d, `EXPLAIN SELECT * FROM u ORDER BY ts LIMIT 1`)
	must(t, d,
		`CREATE TABLE b (k bool primary key, v int)`,
		`CREATE TABLE by (k bytea primary key, v int)`,
	)
	logq(t, d, `EXPLAIN SELECT * FROM b WHERE k = true`)
}

func TestFloatKeyJoin(t *testing.T) {
	d, _ := open(t)
	must(t, d,
		`CREATE TABLE a (id int primary key, v int)`,
		`CREATE TABLE f (k float primary key, n text)`,
		`INSERT INTO a VALUES (1,1),(2,2),(3,3)`,
		`INSERT INTO f VALUES (1.0,'one'),(2.0,'two'),(2.5,'twohalf')`,
	)
	logq(t, d, `SELECT a.id, f.n FROM a JOIN f ON f.k = a.v ORDER BY a.id`)
	logq(t, d, `EXPLAIN SELECT a.id, f.n FROM a JOIN f ON f.k = a.v ORDER BY a.id`)
	logq(t, d, `SELECT n FROM f WHERE k = 2`)
	logq(t, d, `SELECT n FROM f WHERE k > 1 ORDER BY k`)
	logq(t, d, `SELECT a.id, f.n FROM a LEFT JOIN f ON f.k > a.v ORDER BY a.id, f.n`)
	// reverse: int key probed with float values
	logq(t, d, `SELECT f.k, a.id FROM f JOIN a ON a.id = f.k ORDER BY f.k`)
	// CTE with mixed types probing int PK: first row int then float
	logq(t, d, `WITH m(x) AS (SELECT 1 UNION ALL SELECT 2.0 UNION ALL SELECT 2.5) SELECT m.x, a.id FROM m JOIN a ON a.id = m.x`)
	logq(t, d, `WITH m(x) AS (SELECT 2.0 UNION ALL SELECT 1) SELECT m.x, a.id FROM m JOIN a ON a.id = m.x`)
	logq(t, d, `WITH m(x) AS (SELECT 1 UNION ALL SELECT 2.0 UNION ALL SELECT 2.5) SELECT m.x, (SELECT a.id FROM a WHERE a.id = m.x) FROM m`)
}

func TestFKEndOfStatement(t *testing.T) {
	d, _ := open(t)
	must(t, d,
		`CREATE TABLE node (id int primary key, parent int REFERENCES node(id))`,
	)
	logq(t, d, `INSERT INTO node VALUES (1, 2), (2, NULL)`) // PG: OK (end of statement)
	logq(t, d, `INSERT INTO node VALUES (2, NULL), (1, 2)`)
	logq(t, d, `UPDATE node SET id = id + 10, parent = parent + 10`) // PG: ok
	logq(t, d, `SELECT * FROM node ORDER BY id`)

	must(t, d,
		`CREATE TABLE p (id int primary key, name text)`,
		`CREATE TABLE ch (id int primary key, pid int REFERENCES p(id))`,
		`INSERT INTO p VALUES (1,'a'),(3,'c')`,
		`INSERT INTO ch VALUES (1,1)`,
	)
	// key shuffle: 1->2, 3->1 ; key 1 still exists at end. PG: OK.
	logq(t, d, `UPDATE p SET id = CASE WHEN id = 1 THEN 2 ELSE 1 END`)
	logq(t, d, `SELECT * FROM p ORDER BY id`)
	// upsert DO UPDATE moving a referenced parent key: PG errors (23503).
	logq(t, d, `INSERT INTO p VALUES (1,'x') ON CONFLICT (id) DO UPDATE SET id = 100`)
	logq(t, d, `SELECT * FROM p ORDER BY id`)
	logq(t, d, `SELECT ch.id, ch.pid, p.id FROM ch LEFT JOIN p ON p.id = ch.pid`)
}

func TestIdentityCheck(t *testing.T) {
	d, _ := open(t)
	must(t, d, `CREATE TABLE s (id serial primary key CHECK (id > 5), v int)`)
	logq(t, d, `INSERT INTO s (v) VALUES (1)`) // PG: violates check (id=1)
	logq(t, d, `INSERT INTO s VALUES (2, 1)`)
	logq(t, d, `SELECT * FROM s`)
	must(t, d, `CREATE TABLE s2 (id int primary key, a int, b float CHECK (b > a))`)
	logq(t, d, `INSERT INTO s2 VALUES (1, 5, $1)`, "7")
	logq(t, d, `INSERT INTO s2 VALUES (2, 5, $1)`, int64(3))
	logq(t, d, `INSERT INTO s2 VALUES (3, 5, 2 + 1)`)
}

func TestFloatGroup(t *testing.T) {
	d, _ := open(t)
	must(t, d,
		`CREATE TABLE g (id int primary key, x float, i int)`,
		`INSERT INTO g VALUES (1, 0.0, 1), (2, -0.0, 1), (3, 1.0, 1)`,
	)
	logq(t, d, `SELECT x, count(*) FROM g GROUP BY x`)
	logq(t, d, `SELECT DISTINCT x FROM g`)
	logq(t, d, `SELECT count(DISTINCT x) FROM g`)
	logq(t, d, `SELECT x * -1 AS y, count(*) FROM g GROUP BY x * -1`)
	logq(t, d, `SELECT 'NaN'::float`)
	logq(t, d, `SELECT x / 0.0 FROM g`)
	logq(t, d, `SELECT DISTINCT 'NaN'::float + x FROM g`)
	logq(t, d, `SELECT 'NaN'::float + x AS y, count(*) FROM g GROUP BY 1`)
	logq(t, d, `SELECT 1 UNION SELECT 1.0`)
	logq(t, d, `SELECT i + 0.0 AS k, count(*) FROM g GROUP BY 1`)
	logq(t, d, `SELECT CASE WHEN id = 1 THEN 1 ELSE 1.0 END AS k, count(*) FROM g GROUP BY 1`)
	logq(t, d, `SELECT DISTINCT CASE WHEN id = 1 THEN 1 ELSE 1.0 END FROM g`)
	logq(t, d, `SELECT avg(i), sum(i), min(x), max(x) FROM g WHERE id > 100`)
	logq(t, d, `SELECT count(*) FROM g WHERE id > 100 GROUP BY i`)
	logq(t, d, `SELECT sum(i) FROM g HAVING sum(i) > 100`)
}
