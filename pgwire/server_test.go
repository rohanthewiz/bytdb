package pgwire_test

// End-to-end tests through the real pgx driver: everything here
// crosses the wire as a Postgres client, exercising the startup
// handshake, the extended query protocol (pgx's default), the simple
// protocol, and both value formats.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"

	"github.com/rohanthewiz/bytdb/pgwire"
)

// startServer serves a fresh database on a loopback port and returns
// a connection string for it.
func startServer(t *testing.T) string {
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
	t.Cleanup(func() {
		srv.Close()
		e.Close()
	})
	return fmt.Sprintf("postgres://test@%s/test", ln.Addr())
}

func connect(t *testing.T, connString string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), connString)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func mustExec(t *testing.T, c *pgx.Conn, q string, args ...any) pgconn.CommandTag {
	t.Helper()
	tag, err := c.Exec(context.Background(), q, args...)
	if err != nil {
		t.Fatalf("Exec(%q): %v", q, err)
	}
	return tag
}

// TestExtendedProtocol drives pgx's default statement-cached extended
// protocol: Parse/Describe with inferred parameter types, binary
// binds, and binary results.
func TestExtendedProtocol(t *testing.T) {
	ctx := context.Background()
	// Default sslmode=prefer sends an SSLRequest first; the server's
	// polite 'N' must leave the connection usable.
	c := connect(t, startServer(t))

	mustExec(t, c, `create table users (id int primary key, name text, age int, score float, active bool, blob bytea)`)

	tag := mustExec(t, c, `insert into users values ($1, $2, $3, $4, $5, $6)`,
		int64(1), "ada", int64(36), 99.5, true, []byte{0xde, 0xad})
	if tag.RowsAffected() != 1 || !tag.Insert() {
		t.Fatalf("insert tag %q", tag)
	}
	mustExec(t, c, `insert into users (id, name) values ($1, $2)`, int64(2), "grace")

	var (
		name   string
		age    *int64
		score  *float64
		active *bool
		blob   []byte
	)
	if err := c.QueryRow(ctx, `select name, age, score, active, blob from users where id = $1`, int64(1)).
		Scan(&name, &age, &score, &active, &blob); err != nil {
		t.Fatal(err)
	}
	if name != "ada" || age == nil || *age != 36 || score == nil || *score != 99.5 ||
		active == nil || !*active || string(blob) != "\xde\xad" {
		t.Fatalf("row 1: %v %v %v %v %x", name, age, score, active, blob)
	}
	// Unset columns come back as SQL NULL.
	if err := c.QueryRow(ctx, `select name, age, score, active, blob from users where id = $1`, int64(2)).
		Scan(&name, &age, &score, &active, &blob); err != nil {
		t.Fatal(err)
	}
	if name != "grace" || age != nil || score != nil || active != nil || blob != nil {
		t.Fatalf("row 2: %v %v %v %v %v", name, age, score, active, blob)
	}

	// Re-running the same query exercises pgx's statement cache: the
	// named statement re-binds without a fresh Parse.
	for i := range 3 {
		var n int64
		if err := c.QueryRow(ctx, `select count(*) from users where age > $1`, int64(i)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("count run %d: %d", i, n)
		}
	}

	if tag := mustExec(t, c, `update users set age = $1 where id = $2`, int64(37), int64(1)); tag.RowsAffected() != 1 {
		t.Fatalf("update tag %q", tag)
	}
	if tag := mustExec(t, c, `delete from users where id = $1`, int64(2)); tag.RowsAffected() != 1 {
		t.Fatalf("delete tag %q", tag)
	}
}

func TestJoinAndAggregate(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table users (id int primary key, city text)`)
	mustExec(t, c, `create table orders (id int primary key, user_id int, total float)`)
	for i, city := range []string{"london", "nyc", "london"} {
		mustExec(t, c, `insert into users values ($1, $2)`, int64(i+1), city)
		mustExec(t, c, `insert into orders values ($1, $2, $3)`, int64(i+1), int64(i+1), float64(10*(i+1)))
	}
	rows, err := c.Query(ctx,
		`select u.city, sum(o.total) from users u join orders o on o.user_id = u.id group by u.city having sum(o.total) > $1 order by u.city`,
		5.0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for rows.Next() {
		var city string
		var total float64
		if err := rows.Scan(&city, &total); err != nil {
			t.Fatal(err)
		}
		got[city] = total
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if len(got) != 2 || got["london"] != 40 || got["nyc"] != 20 {
		t.Fatalf("got %v", got)
	}
}

// TestErrors checks that structured SQL-layer errors arrive as typed
// ErrorResponse fields: SQLSTATE, Detail, and a 1-based Position.
func TestErrors(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table t (id int primary key)`)
	mustExec(t, c, `insert into t values ($1)`, int64(1))

	// (`select frim t` reads as SELECT frim AS t now, as in Postgres,
	// so a stray token after * makes the syntax error.)
	var pgErr *pgconn.PgError
	_, err := c.Exec(ctx, `select * frim t`)
	if !errors.As(err, &pgErr) || pgErr.Code != "42601" || pgErr.Position <= 0 {
		t.Fatalf("syntax: %+v", err)
	}
	if _, err = c.Exec(ctx, `select * from missing`); !errors.As(err, &pgErr) || pgErr.Code != "42P01" {
		t.Fatalf("missing table: %+v", err)
	}
	if _, err = c.Exec(ctx, `insert into t values ($1)`, int64(1)); !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate pk: %+v", err)
	}
	if pgErr.Detail == "" { // duplicate pk carries table: t
		t.Fatalf("no detail: %+v", pgErr)
	}

	// The connection must stay usable after errors.
	var n int64
	if err := c.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("after errors: %v %d", err, n)
	}
}

// TestSimpleProtocol drives the 'Q' path, including a multi-statement
// buffer.
func TestSimpleProtocol(t *testing.T) {
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(startServer(t) + "?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	// One buffer, three statements.
	if _, err := c.PgConn().Exec(ctx,
		`create table t (id int primary key, name text); insert into t values (1, 'a'); insert into t values (2, 'b')`).ReadAll(); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := c.QueryRow(ctx, `select name from t where id = $1`, int64(2)).Scan(&name); err != nil || name != "b" {
		t.Fatalf("simple query: %v %q", err, name)
	}
	// An error in the middle of a buffer stops the rest.
	if _, err := c.PgConn().Exec(ctx, `insert into t values (3, 'c'); boom; insert into t values (4, 'd')`).ReadAll(); err == nil {
		t.Fatal("want error")
	}
	var n int64
	if err := c.QueryRow(ctx, `select count(*) from t`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("after failed buffer: %v %d", err, n)
	}
}

// TestDatabaseSQL smoke-tests the database/sql surface through pgx's
// stdlib adapter.
func TestDatabaseSQL(t *testing.T) {
	db, err := sql.Open("pgx", startServer(t)+"?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`create table kv (k text primary key, v int)`); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`insert into kv values ($1, $2), ($3, $4)`, "a", int64(1), "b", int64(2))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Fatalf("affected %d", n)
	}
	st, err := db.Prepare(`select v from kv where k = $1`)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for k, want := range map[string]int64{"a": 1, "b": 2} {
		var v int64
		if err := st.QueryRow(k).Scan(&v); err != nil || v != want {
			t.Fatalf("kv[%s]: %v %d", k, err, v)
		}
	}
}
