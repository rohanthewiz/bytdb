package sqlfrontauditwire

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/pgwire"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

func start(t *testing.T) *pgx.Conn {
	t.Helper()
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := pgwire.NewServer(bsql.New(e))
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); e.Close() })
	c, err := pgx.Connect(context.Background(), fmt.Sprintf("postgres://test@%s/test", ln.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func TestWire(t *testing.T) {
	ctx := context.Background()
	c := start(t)
	ex := func(q string, args ...any) {
		_, err := c.Exec(ctx, q, args...)
		t.Logf("EXEC %-70s %v => err=%v", q, args, err)
	}
	ex(`create table t (id int primary key, i int, u uuid, ts timestamp, d date, s text)`)
	ex(`insert into t values (1, 10, 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '2024-02-29 12:00:00', '2024-02-29', 'x')`)
	ex(`insert into t values (2, null, null, null, null, null)`)

	var n int64
	q1 := func(dst any, q string, args ...any) {
		err := c.QueryRow(ctx, q, args...).Scan(dst)
		t.Logf("QUERY %-70s %v => err=%v", q, args, err)
	}
	q1(&n, `select count(*) from t where id in ($1, $2)`, 1, 2)
	t.Logf("   n=%d", n)
	q1(&n, `select count(*) from t where id = any($1)`, []int64{1, 2})
	t.Logf("   n=%d", n)
	q1(&n, `select count(*) from t where u in ($1)`, "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11")
	t.Logf("   n=%d (want 1)", n)
	q1(&n, `select count(*) from t where u = $1`, "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11")
	t.Logf("   n=%d (want 1)", n)
	q1(&n, `select count(*) from t where ts in ($1)`, time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC))
	t.Logf("   n=%d (want 1)", n)
	q1(&n, `select count(*) from t where d in ($1)`, time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC))
	t.Logf("   n=%d (want 1)", n)
	q1(&n, `select coalesce(i, 0) from t where id = 2`)
	q1(&n, `select coalesce(sum(i), 0) from t`)
	q1(&n, `select (select max(id) from t)`)
	q1(&n, `select case when id = 1 then i else 0 end from t where id = 1`)
	q1(&n, `select case when id = 9 then null else i end from t where id = 1`)
	q1(&n, `select nullif(i, 0) from t where id = 1`)
	q1(&n, `select $1::int + 1`, 5)
	q1(&n, `select i + $1 from t where id = 1`, 5)
	q1(&n, `select d - d from t where id = 1`)
	var b bool
	q1(&b, `select exists(select 1 from t where id = 1)`)
	q1(&b, `select 1::bool`)
	q1(&b, `select i is null from t where id = 2`)
	var tm time.Time
	q1(&tm, `select ts::date from t where id = 1`)
	t.Logf("   ts::date=%v", tm)
	q1(&tm, `select d + 1 from t where id = 1`)
	q1(&tm, `select coalesce(ts, now()) from t where id = 1`)
	q1(&tm, `select max(ts) from t`)
	var s string
	q1(&s, `select ts::text from t where id = 1`)
	t.Logf("   ts::text=%q", s)
	q1(&s, `select u::text from t where id = 1`)
	t.Logf("   u::text=%q", s)
	q1(&s, `select $1`, time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC))
	t.Logf("   $1 time=%q", s)
	var f float64
	q1(&f, `select 1000000.0::text::float`)
	q1(&s, `select 123456789.125`)
	t.Logf("   float text=%q", s)

	// transaction: failed parse/describe inside a block
	ex(`begin`)
	ex(`insert into t (id) values (100)`)
	ex(`insert into nope (id) values ($1)`, 1)
	ex(`insrt typo`)
	ex(`insert into t (id) values (101)`)
	tag, err := c.Exec(ctx, `commit`)
	t.Logf("COMMIT tag=%q err=%v", tag.String(), err)
	q1(&n, `select count(*) from t where id >= 100`)
	t.Logf("   rows surviving the 'failed' block = %d (Postgres: 0)", n)
}
