package sqlexecaudit

import "testing"

func TestTemporalExprCompare(t *testing.T) {
	d, _ := open(t)
	must(t, d, `CREATE TABLE t (id int primary key, ts timestamp, dt date, u uuid)`,
		`INSERT INTO t VALUES (1, '2024-01-01 10:00:00', '2024-01-01', 'aaaaaaaa-0000-0000-0000-000000000001')`)
	for _, q := range []string{
		`SELECT ts::date, dt, ts FROM t`,
		`SELECT id FROM t WHERE ts::date = dt`,                         // PG: 1
		`SELECT id FROM t WHERE ts::date = '2024-01-01'`,               // PG: 1
		`SELECT id FROM t WHERE coalesce(ts, now()) < '2025-01-01'`,    // PG: 1
		`SELECT id FROM t WHERE coalesce(dt, current_date) = '2024-01-01'`, // PG: 1
		`SELECT id FROM t WHERE ts < '2025-01-01'`,                     // plain column: works
		`SELECT id FROM t WHERE ts < now()`,                            // PG: 1
		`SELECT id FROM t WHERE dt < current_date`,                     // PG: 1
		`SELECT id FROM t WHERE dt < now()`,                            // PG: 1
		`SELECT id FROM t WHERE ts >= dt`,                              // PG: 1
		`SELECT id FROM t WHERE (SELECT max(ts) FROM t) > '2023-01-01'`, // PG: 1
		`SELECT id FROM t WHERE coalesce(u, gen_random_uuid()) = 'aaaaaaaa-0000-0000-0000-000000000001'`, // PG: 1
		`SELECT CASE WHEN ts > '2023-01-01' THEN 'new' ELSE 'old' END FROM t`,
		`SELECT CASE WHEN coalesce(ts, now()) > '2023-01-01' THEN 'new' ELSE 'old' END FROM t`, // PG: new
		`SELECT max(ts) FROM t HAVING max(ts) > '2023-01-01'`,
		`SELECT id FROM t x WHERE EXISTS (SELECT 1 FROM t y WHERE y.ts::date = x.dt)`,
	} {
		logq(t, d, q)
	}
}
