package sqlexecaudit

import "testing"

func TestSubqueryOrderLimit(t *testing.T) {
	d, _ := open(t)
	must(t, d,
		`CREATE TABLE o (id int primary key, cust int, amt int)`,
		`INSERT INTO o VALUES (1,1,10),(2,1,50),(3,1,30),(4,2,5),(5,2,99)`,
		`CREATE TABLE c (id int primary key, name text)`,
		`INSERT INTO c VALUES (1,'a'),(2,'b')`,
	)
	logq(t, d, `SELECT (SELECT amt FROM o ORDER BY amt DESC LIMIT 1)`)
	logq(t, d, `SELECT c.id, (SELECT amt FROM o WHERE o.cust = c.id ORDER BY amt DESC LIMIT 1) FROM c`)
	logq(t, d, `SELECT id FROM o WHERE id IN (SELECT id FROM o ORDER BY amt DESC LIMIT 2) ORDER BY id`)
	logq(t, d, `SELECT id FROM o WHERE id IN (SELECT id FROM o LIMIT 2) ORDER BY id`)
	logq(t, d, `SELECT ARRAY(SELECT id FROM o ORDER BY amt DESC LIMIT 2)`)
	logq(t, d, `SELECT ARRAY(SELECT id FROM o ORDER BY amt DESC)`)
	logq(t, d, `SELECT (SELECT coalesce(sum(amt),0) FROM o WHERE cust = 9)`)
	logq(t, d, `SELECT (SELECT count(*) + 1 FROM o)`)
	logq(t, d, `SELECT (SELECT count(DISTINCT cust) FROM o)`)
	logq(t, d, `SELECT c.id, (SELECT count(DISTINCT amt) FROM o WHERE o.cust = c.id) FROM c`)
	logq(t, d, `SELECT EXISTS (SELECT count(*) FROM o WHERE cust = 9)`)
	logq(t, d, `SELECT EXISTS (SELECT 1 FROM o LIMIT 0)`)
	logq(t, d, `SELECT EXISTS (SELECT 1 FROM o OFFSET 10)`)
	logq(t, d, `SELECT (SELECT max(amt) FROM o GROUP BY cust ORDER BY 1 LIMIT 1)`)
	logq(t, d, `SELECT * FROM c WHERE id = (SELECT cust FROM o ORDER BY amt DESC LIMIT 1)`)
	logq(t, d, `SELECT id FROM o WHERE amt > ALL (SELECT amt FROM o WHERE cust = 2 ORDER BY amt LIMIT 1)`)
}
