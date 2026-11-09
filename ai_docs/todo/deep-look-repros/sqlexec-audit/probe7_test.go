package sqlexecaudit

import "testing"

func TestViewNextval(t *testing.T) {
	d, _ := open(t)
	must(t, d, `CREATE SEQUENCE sq`, `CREATE TABLE t (id int primary key, ts timestamp, dt date)`,
		`INSERT INTO t VALUES (1, '2024-01-01 10:00:00', '2024-01-01')`)
	logq(t, d, `CREATE VIEW nv AS SELECT nextval('sq') AS n`)
	logq(t, d, `SELECT * FROM nv`)
	logq(t, d, `SELECT * FROM nv`)
	logq(t, d, `SELECT nextval('sq')`)
	// cross-scope: cast result vs quoted literal compares as unknown
	logq(t, d, `SELECT id FROM t WHERE ts::date = '2024-01-01'`)
	logq(t, d, `SELECT id FROM t WHERE dt = '2024-01-01'`)
	logq(t, d, `SELECT id FROM t WHERE ts::date = dt`)
	logq(t, d, `SELECT id FROM t WHERE lower('A') = 'a' AND id + 0 = '1'`)
	logq(t, d, `SELECT id FROM t WHERE ts > now()`)
	logq(t, d, `SELECT id FROM t WHERE coalesce(ts, now()) < '2025-01-01'`)
}
