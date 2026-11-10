package pgwire_test

// Deep-look D-51 over the wire: pg_backend_pid() agrees with the
// BackendKeyData pid, SQL DEALLOCATE reaches the connection's
// protocol-level statements, startup parameters apply as SET does,
// advisory locks are per connection, and the new errors carry their
// Postgres SQLSTATEs.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSessionSurfaceWire(t *testing.T) {
	ctx := context.Background()
	url := startServer(t)
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["application_name"] = "migrator"
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	var pid int64
	if err := c.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if uint32(pid) != c.PgConn().PID() {
		t.Errorf("pg_backend_pid() = %d, BackendKeyData pid = %d", pid, c.PgConn().PID())
	}
	var app string
	if err := c.QueryRow(ctx, `show application_name`).Scan(&app); err != nil || app != "migrator" {
		t.Errorf("application_name = %q, %v", app, err)
	}
	if err := c.QueryRow(ctx, `select application_name from pg_stat_activity where pid = pg_backend_pid()`).Scan(&app); err != nil || app != "migrator" {
		t.Errorf("pg_stat_activity row for own pid: %q, %v", app, err)
	}

	// DEALLOCATE drops a protocol-level statement by name; executing it
	// afterwards fails as an unknown statement would.
	if _, err := c.Prepare(ctx, "ps1", `select 1`); err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, `deallocate ps1`)
	_, err = c.PgConn().ExecPrepared(ctx, "ps1", nil, nil, nil).Close()
	if err == nil {
		t.Error("statement survived DEALLOCATE")
	}
	var pgErr *pgconn.PgError
	if _, err := c.Exec(ctx, `deallocate ps1`); !errors.As(err, &pgErr) || pgErr.Code != "26000" {
		t.Errorf("DEALLOCATE of an unknown name: %v", err)
	}
	// DISCARD ALL drops every protocol-level statement, pgx's own cache
	// entries included (pgx cannot know), so the reset goes through the
	// simple protocol, as a pool's reset query does.
	if _, err := c.Exec(ctx, `discard all`, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `show app.none`, pgx.QueryExecModeSimpleProtocol); !errors.As(err, &pgErr) || pgErr.Code != "42704" {
		t.Errorf("SHOW of an unknown parameter: %v", err)
	}

	// Advisory locks belong to the connection.
	c2 := connect(t, url)
	mustExec(t, c, `select pg_advisory_lock(42)`)
	var got bool
	if err := c2.QueryRow(ctx, `select pg_try_advisory_lock(42)`).Scan(&got); err != nil || got {
		t.Errorf("second connection took the lock: %v %v", got, err)
	}
	c.Close(ctx) // a disconnect releases the session's locks
	ok := false
	for i := 0; i < 200 && !ok; i++ { // the server notices the close asynchronously
		if err := c2.QueryRow(ctx, `select pg_try_advisory_lock(42)`).Scan(&ok); err != nil {
			t.Fatal(err)
		}
	}
	if !ok {
		t.Error("lock outlived its connection")
	}

	// A bad startup parameter fails the connection, as in Postgres.
	bad, _ := pgx.ParseConfig(url)
	bad.RuntimeParams["statement_timeout"] = "soon"
	if conn, err := pgx.ConnectConfig(ctx, bad); err == nil {
		conn.Close(ctx)
		t.Error("connection with an invalid statement_timeout succeeded")
	}
}
