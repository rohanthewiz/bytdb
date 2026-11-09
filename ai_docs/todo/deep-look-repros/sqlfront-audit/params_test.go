package sqlfrontaudit

import (
	"testing"
	"time"

	bsql "github.com/rohanthewiz/bytdb/sql"
)

type myUUID [16]byte
type myInt int
type myStr string

func pexec(t *testing.T, d *bsql.DB, q string, args ...any) {
	t.Helper()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("%-66s %v => PANIC: %v", q, args, r)
			}
		}()
		res, err := d.Exec(q, args...)
		t.Logf("%-66s %v => %s", q, args, show(res, err))
	}()
}

func TestParamProbes(t *testing.T) {
	d := openDB(t)
	must(t, d, `create table t (id int primary key, i int, f float, s text, b bool, ts timestamp, d date, u uuid, j jsonb, by bytea, a text[])`)
	must(t, d, `insert into t values (1, 10, 1.5, 'héllo', true, '2024-02-29 12:00:00', '2024-02-29', 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '{"a":1}', '\x0102', '{x,y}')`)
	must(t, d, `insert into t values (2, null, null, null, null, null, null, null, null, null, null)`)
	us := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	ub := [16]byte{0xa0, 0xee, 0xbc, 0x99, 0x9c, 0xb, 0x4e, 0xf8, 0xbb, 0x6d, 0x6b, 0xb9, 0xbd, 0x38, 0xa, 0x11}
	tm := time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)

	pexec(t, d, `select id from t where i = $1`, nil)
	pexec(t, d, `select id from t where i <> $1`, nil)
	pexec(t, d, `select id from t where id = $1`, nil)
	pexec(t, d, `select id from t where not (i = $1)`, nil)
	pexec(t, d, `select id from t where u = $1`, us)
	pexec(t, d, `select id from t where u = $1`, ub)
	pexec(t, d, `select id from t where u = $1`, myUUID(ub))
	pexec(t, d, `select id from t where u in ($1)`, us)
	pexec(t, d, `select id from t where u in ($1, $2)`, us, us)
	pexec(t, d, `select id from t where u in ($1)`, ub)
	pexec(t, d, `select id from t where u = any($1)`, "{"+us+"}")
	pexec(t, d, `select id from t where ts in ($1)`, "2024-02-29 12:00:00")
	pexec(t, d, `select id from t where ts in ($1)`, tm)
	pexec(t, d, `select id from t where ts = $1`, tm)
	pexec(t, d, `select id from t where d = $1`, tm)
	pexec(t, d, `select id from t where d in ($1)`, tm)
	pexec(t, d, `select id from t where d in ($1)`, "2024-02-29")
	pexec(t, d, `select id from t where ts > $1 or id = $2`, "2024-01-01", 99)
	pexec(t, d, `select id from t where coalesce(d, $1) = $2`, "2024-01-01", "2024-02-29")
	pexec(t, d, `select id from t where id = $1`, "1")
	pexec(t, d, `select id from t where id in ($1)`, "1")
	pexec(t, d, `select id from t where id = $1`, 1.0)
	pexec(t, d, `select id from t where id = $1`, 1.5)
	pexec(t, d, `select id from t where id = $1`, myInt(1))
	pexec(t, d, `select id from t where s = $1`, myStr("héllo"))
	pexec(t, d, `select id from t where s = $1`, []byte("héllo"))
	pexec(t, d, `select id from t where s = $1`, 5)
	pexec(t, d, `select id from t where id = $1`, uint64(1<<63))
	pexec(t, d, `select id from t where b = $1`, 1)
	pexec(t, d, `select id from t where b = $1`, "t")
	pexec(t, d, `select id from t where id = $1`, true)
	pexec(t, d, `select $1`, 5)
	pexec(t, d, `select $1 + 1`, 5)
	pexec(t, d, `select $1 + 1`, "5")
	pexec(t, d, `select $1::int + 1`, "5")
	pexec(t, d, `select $2`, 1, 2)
	pexec(t, d, `select $2`, 2)
	pexec(t, d, `select $0`)
	pexec(t, d, `select $99999999999999999999`)
	pexec(t, d, `select $1, $1`, tm)
	pexec(t, d, `select id from t limit $1 offset $2`, 1, 1)
	pexec(t, d, `select id from t limit $1`, "1")
	pexec(t, d, `select id from t limit $1`, -1)
	pexec(t, d, `select id from t where a = $1`, []string{"x", "y"})
	pexec(t, d, `select id from t where a = $1`, "{x,y}")
	pexec(t, d, `select id from t where $1 = any(a)`, "x")
	pexec(t, d, `select * from (select id from t where id = $1) q`, 1)
	pexec(t, d, `with q as (select id from t where id = $1) select * from q where id = $2`, 1, 1)
	pexec(t, d, `select (select max(id) from t where id <= $1)`, 1)
	pexec(t, d, `select id from t where exists (select 1 from t t2 where t2.id = $1)`, 1)
	pexec(t, d, `insert into t (id, ts, d, u) values ($1, $2, $3, $4)`, 3, tm, tm, us)
	pexec(t, d, `insert into t (id, ts) values ($1, $2)`, 4, "2024-02-29T12:00:00Z")
	pexec(t, d, `insert into t (id, i) values ($1, $2 + 1)`, 5, "7")
	pexec(t, d, `insert into t (id, i) values ($1, $2)`, 6, 1.9)
	pexec(t, d, `insert into t (id, i) values ($1, $2)`, 7, "1.9")
	pexec(t, d, `insert into t (id, b) values ($1, $2)`, 8, 2)
	pexec(t, d, `insert into t (id, s) values ($1, $2)`, 9, 42)
	pexec(t, d, `insert into t (id, s) values ($1, $2)`, 10, tm)
	pexec(t, d, `insert into t (id, j) values ($1, $2)`, 11, []byte(`{"k": [1,2]}`))
	pexec(t, d, `insert into t (id, j) values ($1, $2)`, 12, map[string]any{"k": 1})
	pexec(t, d, `insert into t (id, f) values ($1, $2)`, 13, 3)
	pexec(t, d, `insert into t (id, ts) values ($1, $2)`, 14, int64(5))
	pexec(t, d, `insert into t (id, d) values ($1, $2)`, 15, "2024-02-29 23:59:59-08")
	pexec(t, d, `update t set i = i + $1 where id = $2`, "5", 1)
	pexec(t, d, `update t set ts = $1 where ts < $1`, tm.Add(time.Hour))
	pexec(t, d, `select id, i, f, s, ts, d, j from t order by id`)
	pexec(t, d, `create view v as select $1 as x`)
	pexec(t, d, `select * from v`)
	pexec(t, d, `create table p (id int primary key, x int default $1)`)
	pexec(t, d, `select 1; select 2`)
	pexec(t, d, ``)
	pexec(t, d, `;`)
	pexec(t, d, `-- just a comment`)

	// Describe: what types do placeholders infer?
	for _, q := range []string{
		`select id from t where id in ($1, $2)`,
		`select id from t where u = any($1)`,
		`select id from t where lower(s) = $1`,
		`select id from t where i + 1 = $1`,
		`select id from t where ts between $1 and $2`,
		`select id from t where s like $1`,
		`select id from t where $1 = id`,
		`select $1, $2::int, coalesce(i, $3) from t`,
		`insert into t (id, ts) values ($1, coalesce($2, now()))`,
		`select id from t where (id = $1 or $1 is null)`,
		`select case when id = $1 then 1 else null end from t`,
		`select null, 1 + 1.5, i / 2, d + 1, ts, count(*), sum(i), avg(i), max(d) from t`,
	} {
		st, err := d.Prepare(q)
		if err != nil {
			t.Logf("PREPARE %-60s => ERR %v", q, err)
			continue
		}
		info, err := st.Describe()
		if err != nil {
			t.Logf("DESCRIBE %-60s => ERR %v", q, err)
			continue
		}
		t.Logf("DESCRIBE %-60s => params=%q cols=%v types=%v", q, info.Params, info.Cols, info.Types)
	}
}
