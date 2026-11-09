package edgeaudit

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPing(t *testing.T) {
	addr, _, _ := startServer(t, nil)
	ctx := context.Background()
	c := pgxConn(t, addr)
	t.Logf("pgx Conn.Ping: %v", c.Ping(ctx))
	db, _ := sql.Open("pgx", dsn(addr))
	defer db.Close()
	t.Logf("database/sql + pgx stdlib db.Ping: %v", db.PingContext(ctx))
	pq, _ := sql.Open("postgres", dsn(addr))
	defer pq.Close()
	t.Logf("database/sql + lib/pq db.Ping: %v", pq.PingContext(ctx))
	for _, q := range []string{"-- just a comment", "/* c */", "select 1; -- trailing comment", "select 1 /* c */;"} {
		_, err := c.PgConn().Exec(ctx, q).ReadAll()
		t.Logf("simple query %q -> %v", q, err)
	}
}
