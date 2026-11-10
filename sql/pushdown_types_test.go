package sql

// Deep-look D-37: equality and range conjuncts on timestamp, date and
// uuid columns push into point gets and bounded scans, as int and text
// keys always did. They used to fall through litFits, so a uuid
// primary-key lookup was a full scan, a join on one a scan per outer
// row, and every FK check against a uuid parent a parent scan.

import (
	"fmt"
	"strings"
	"testing"
)

func uuidN(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", i) }

func TestPushdownTemporalAndUUIDKeys(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table ev (id uuid primary key, at timestamp, day date, n int)`)
	exec(t, d, `create index ev_at on ev (at)`)
	exec(t, d, `create index ev_day on ev (day)`)
	for i := 1; i <= 30; i++ {
		exec(t, d, fmt.Sprintf(`insert into ev values ('%s', '2024-01-%02d 12:00:00', '2024-01-%02d', %d)`,
			uuidN(i), i, i, i))
	}

	cases := []struct {
		q, plan string
		want    string // the n values, in order
	}{
		{`select n from ev where id = '` + uuidN(7) + `'`,
			"Key: (id = '" + uuidN(7) + "'::uuid)", "[7]"},
		// A cast literal folds to the same pushed key.
		{`select n from ev where id = '` + uuidN(8) + `'::uuid`,
			"Point Get on ev", "[8]"},
		{`select n from ev where '` + uuidN(9) + `'::uuid = id`,
			"Point Get on ev", "[9]"},
		{`select n from ev where at >= '2024-01-28' order by n`,
			"Index Cond: (at >= '2024-01-28 00:00:00+00'::timestamp with time zone)",
			"[28 29 30]"},
		{`select n from ev where at < '2024-01-03 00:00:00'::timestamptz order by n`,
			"Index Scan using ev_at on ev", "[1 2]"},
		{`select n from ev where day = '2024-01-15'`,
			"Index Cond: (day = '2024-01-15'::date)", "[15]"},
		{`select n from ev where day between '2024-01-10'::date and '2024-01-12'::date order by n`,
			"Index Scan using ev_day on ev", "[10 11 12]"},
		// Postgres casts the date to a timestamp for this comparison; the
		// cast is on the column side, so it is not a key range — and must
		// still answer correctly.
		{`select n from ev where day < '2024-01-03 06:00:00'::timestamp order by n`,
			"Seq Scan on ev", "[1 2 3]"},
	}
	for _, c := range cases {
		if got := explainText(t, d, c.q); !strings.Contains(got, c.plan) {
			t.Errorf("%s\nplan:\n%s\nwant it to contain:\n%s", c.q, got, c.plan)
		}
		r := exec(t, d, c.q)
		var ns []any
		for _, row := range r.Rows {
			ns = append(ns, row[0])
		}
		if got := fmt.Sprint(ns); got != c.want {
			t.Errorf("%s = %s, want %s", c.q, got, c.want)
		}
	}

	// A bound parameter in text form (how the simple protocol and most
	// drivers send a uuid) pushes down too: the typed pass converts it.
	r, err := d.Exec(`select n from ev where id = $1`, uuidN(11))
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != int64(11) {
		t.Fatalf("param lookup = %v, %v", r, err)
	}
}

// A join on a uuid key uses the inner key per outer row instead of a
// scan per outer row, and an FK check against a uuid parent is a point
// get.
func TestPushdownUUIDJoinAndFK(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table p (id uuid primary key, name text)`)
	exec(t, d, `create table c (id int primary key, pid uuid references p (id), raw bytea)`)
	for i := 1; i <= 20; i++ {
		exec(t, d, fmt.Sprintf(`insert into p values ('%s', 'p%d')`, uuidN(i), i))
	}
	for i := 1; i <= 40; i++ {
		exec(t, d, fmt.Sprintf(`insert into c values (%d, '%s', null)`, i, uuidN(1+i%20)))
	}
	q := `select c.id, p.name from c join p on p.id = c.pid where c.id <= 3 order by c.id`
	if plan := explainText(t, d, q); !strings.Contains(plan, "Point Get on p") {
		t.Errorf("join inner side is not a keyed lookup:\n%s", plan)
	}
	r := exec(t, d, q)
	if got := fmt.Sprint(r.Rows); got != "[[1 p2] [2 p3] [3 p4]]" {
		t.Fatalf("join = %s", got)
	}
	if msg := execErr(t, d, `insert into c values (100, '`+uuidN(99)+`', null)`); !strings.Contains(msg, "foreign key") {
		t.Fatalf("FK violation not reported: %s", msg)
	}

	// A bytea column joined to the uuid key brings 16 bytes on some rows
	// and other lengths on others. The cached per-row plan pushes only
	// the 16-byte values; a short one used to be rebound into the uuid
	// key and fail to encode.
	hexOf := func(i int) string { return `'\x` + strings.ReplaceAll(uuidN(i), "-", "") + `'` }
	exec(t, d, `update c set raw = `+hexOf(2)+` where id = 1`)
	exec(t, d, `update c set raw = '\x0102' where id = 2`)
	exec(t, d, `update c set raw = `+hexOf(4)+` where id = 3`)
	r = exec(t, d, `select c.id, p.name from c join p on p.id = c.raw where c.id <= 3 order by c.id`)
	if got := fmt.Sprint(r.Rows); got != "[[1 p2] [3 p4]]" {
		t.Fatalf("bytea-to-uuid join = %s", got)
	}
}
