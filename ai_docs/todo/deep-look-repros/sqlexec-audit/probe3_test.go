package sqlexecaudit

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func uuidOf(i int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", i) }

func TestUUIDKeys(t *testing.T) {
	d, _ := open(t)
	must(t, d,
		`CREATE TABLE users (id uuid primary key, name text)`,
		`CREATE TABLE orders (id uuid primary key, user_id uuid NOT NULL REFERENCES users(id), amt int)`,
		`CREATE INDEX orders_user ON orders (user_id)`,
	)
	const n = 3000
	t0 := time.Now()
	for i := 0; i < n; i++ {
		if _, err := d.Exec(`INSERT INTO users VALUES ($1, 'u')`, uuidOf(i)); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("insert %d users: %v", n, time.Since(t0))
	t0 = time.Now()
	for i := 0; i < n; i++ {
		if _, err := d.Exec(`INSERT INTO orders VALUES ($1, $2, 1)`, uuidOf(i+100000), uuidOf(i)); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("insert %d orders (FK check on uuid parent PK): %v", n, time.Since(t0))
	logq(t, d, `EXPLAIN SELECT o.id, u.name FROM orders o JOIN users u ON u.id = o.user_id`)
	t0 = time.Now()
	res, err := d.Exec(`SELECT count(*) FROM orders o JOIN users u ON u.id = o.user_id`)
	t.Logf("uuid join %dx%d: %v rows=%v err=%v", n, n, time.Since(t0), res.Rows, err)
	t0 = time.Now()
	for i := 0; i < 200; i++ {
		d.Exec(`SELECT name FROM users WHERE id = $1`, uuidOf(i))
	}
	t.Logf("200 uuid pk lookups over %d rows: %v", n, time.Since(t0))
	logq(t, d, `EXPLAIN SELECT name FROM users WHERE id = $1`, uuidOf(5))
	logq(t, d, `EXPLAIN DELETE FROM users WHERE id = '00000000-0000-0000-0000-000000000005'`)

	// same shape with int keys for comparison
	must(t, d,
		`CREATE TABLE users2 (id int primary key, name text)`,
		`CREATE TABLE orders2 (id int primary key, user_id int NOT NULL REFERENCES users2(id), amt int)`,
	)
	t0 = time.Now()
	for i := 0; i < n; i++ {
		d.Exec(`INSERT INTO users2 VALUES ($1, 'u')`, int64(i))
	}
	for i := 0; i < n; i++ {
		d.Exec(`INSERT INTO orders2 VALUES ($1, $2, 1)`, int64(i), int64(i))
	}
	t.Logf("int: insert %d users + %d orders: %v", n, n, time.Since(t0))
	t0 = time.Now()
	res, err = d.Exec(`SELECT count(*) FROM orders2 o JOIN users2 u ON u.id = o.user_id`)
	t.Logf("int join: %v rows=%v err=%v", time.Since(t0), res.Rows, err)
}

func TestUpsertCoercion(t *testing.T) {
	d, _ := open(t)
	must(t, d,
		`CREATE TABLE k (id int primary key, uid uuid UNIQUE, ts timestamp UNIQUE, n int UNIQUE, f float UNIQUE)`,
		`INSERT INTO k VALUES (1, 'AAAAAAAA-0000-0000-0000-000000000001', '2024-01-01 00:00:00', 7, 1.5)`,
	)
	logq(t, d, `INSERT INTO k VALUES (2, 'aaaaaaaa-0000-0000-0000-000000000001', NULL, NULL, NULL) ON CONFLICT (uid) DO NOTHING`)
	logq(t, d, `INSERT INTO k VALUES (2, 'AAAAAAAA000000000000000000000001', NULL, NULL, NULL) ON CONFLICT (uid) DO NOTHING`)
	logq(t, d, `INSERT INTO k VALUES (2, $1, NULL, NULL, NULL) ON CONFLICT (uid) DO NOTHING`, "AAAAAAAA-0000-0000-0000-000000000001")
	logq(t, d, `INSERT INTO k VALUES (2, NULL, $1, NULL, NULL) ON CONFLICT (ts) DO NOTHING`, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	logq(t, d, `INSERT INTO k VALUES (2, NULL, '2024-01-01', NULL, NULL) ON CONFLICT (ts) DO NOTHING`)
	logq(t, d, `INSERT INTO k VALUES (2, NULL, NULL, $1, NULL) ON CONFLICT (n) DO NOTHING`, "7")
	logq(t, d, `INSERT INTO k VALUES (2, NULL, NULL, 3 + 4, NULL) ON CONFLICT (n) DO NOTHING`)
	logq(t, d, `INSERT INTO k VALUES (2, NULL, NULL, NULL, 1.5) ON CONFLICT (f) DO NOTHING`)
	logq(t, d, `INSERT INTO k VALUES ($1, NULL, NULL, NULL, NULL) ON CONFLICT (id) DO NOTHING`, "1")
	logq(t, d, `INSERT INTO k VALUES ('1'||'', NULL, NULL, NULL, NULL) ON CONFLICT (id) DO NOTHING`)
	logq(t, d, `INSERT INTO k VALUES (2, lower('AAAAAAAA-0000-0000-0000-000000000001')::uuid, NULL, NULL, NULL) ON CONFLICT (uid) DO NOTHING`)
	logq(t, d, `INSERT INTO k VALUES (2, upper('aaaaaaaa-0000-0000-0000-000000000001'), NULL, NULL, NULL) ON CONFLICT (uid) DO NOTHING`)
	logq(t, d, `INSERT INTO k VALUES (2, NULL, now(), NULL, NULL) ON CONFLICT (ts) DO UPDATE SET n = 9`)
	logq(t, d, `SELECT id, n FROM k ORDER BY id`)
}

func TestUnionSnapshot(t *testing.T) {
	d, _ := open(t)
	must(t, d, `CREATE TABLE t (id int primary key)`)
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			d.Exec(`INSERT INTO t VALUES ($1)`, int64(i))
		}
	}()
	diff := 0
	var ex string
	for i := 0; i < 3000; i++ {
		res, err := d.Exec(`SELECT count(*) FROM t UNION ALL SELECT count(*) FROM t`)
		if err != nil {
			t.Fatal(err)
		}
		if res.Rows[0][0] != res.Rows[1][0] {
			diff++
			ex = fmt.Sprint(res.Rows)
		}
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("UNION ALL arms disagreed in %d/3000 runs, e.g. %s", diff, ex)
	// subquery in select list vs main query share a snapshot?
	stop.Store(false)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1000000; !stop.Load(); i++ {
			d.Exec(`INSERT INTO t VALUES ($1)`, int64(i))
		}
	}()
	diff = 0
	for i := 0; i < 3000; i++ {
		res, err := d.Exec(`SELECT count(*), (SELECT count(*) FROM t) FROM t`)
		if err != nil {
			t.Fatal(err)
		}
		if res.Rows[0][0] != res.Rows[0][1] {
			diff++
		}
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("scalar subquery vs outer disagreed in %d/3000 runs", diff)
}

func TestSubAggExpr(t *testing.T) {
	d, _ := open(t)
	must(t, d, `CREATE TABLE o (id int primary key, cust int, amt int)`,
		`INSERT INTO o VALUES (1,1,10),(2,1,50)`,
		`CREATE TABLE c (id int primary key)`, `INSERT INTO c VALUES (1),(2)`)
	logq(t, d, `SELECT (SELECT coalesce(sum(amt),0) FROM o)`)
	logq(t, d, `SELECT c.id, (SELECT coalesce(sum(amt),0) FROM o WHERE o.cust = c.id) FROM c`)
	logq(t, d, `SELECT c.id, (SELECT sum(amt) FROM o WHERE o.cust = c.id) FROM c`)
	logq(t, d, `SELECT c.id FROM c WHERE (SELECT count(*) FROM o WHERE o.cust = c.id) > 0`)
	logq(t, d, `SELECT c.id FROM c WHERE c.id IN (SELECT cust FROM o GROUP BY cust HAVING count(*) > 1)`)
	logq(t, d, `SELECT c.id FROM c WHERE c.id IN (SELECT max(cust) FROM o)`)
	logq(t, d, `SELECT c.id FROM c WHERE EXISTS (SELECT 1 FROM o WHERE o.cust = c.id GROUP BY cust)`)
	logq(t, d, `SELECT c.id FROM c WHERE c.id IN (SELECT cust FROM o UNION SELECT 2)`)
}
