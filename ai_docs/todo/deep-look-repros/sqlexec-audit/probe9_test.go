package sqlexecaudit

import "testing"

func TestDateVsTimestamp(t *testing.T) {
	d, _ := open(t)
	must(t, d, `CREATE TABLE ev (id int primary key, due date, at timestamp)`,
		`INSERT INTO ev VALUES (1, '2099-01-01', '2024-06-01 12:00:00'), (2, '2000-01-01', '2024-06-01 12:00:00')`)
	for _, q := range []string{
		`SELECT id FROM ev WHERE due > now()`,           // PG: 1
		`SELECT id FROM ev WHERE due > current_date`,    // PG: 1
		`SELECT id FROM ev WHERE due > at`,              // PG: 1
		`SELECT id FROM ev WHERE at > due ORDER BY id`,  // PG: 2
		`SELECT id FROM ev WHERE at > current_date`,     // PG: none (2024 < today)
		`SELECT id FROM ev WHERE at::date = '2024-06-01' ORDER BY id`, // PG: 1,2
		`SELECT id, CASE WHEN at > '2030-01-01' THEN 'future' WHEN at > '2020-01-01' THEN 'recent' ELSE 'old' END FROM ev ORDER BY id`, // PG: recent, recent
		`SELECT a.id, b.id FROM ev a JOIN ev b ON a.due = b.at`,
	} {
		logq(t, d, q)
	}
}
