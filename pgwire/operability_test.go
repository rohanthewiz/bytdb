package pgwire_test

// Tests for the operability knobs: MaxConns, QueryLog, and the
// SHOW/TRUNCATE statements as clients see them over the wire.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rohanthewiz/bytdb/pgwire"
	"github.com/rohanthewiz/bytdb/sql"
)

func TestMaxConns(t *testing.T) {
	addr := startConfiguredServer(t, func(s *pgwire.Server) { s.MaxConns = 2 })
	url := fmt.Sprintf("postgres://any@%s/any?sslmode=disable", addr)

	c1 := connect(t, url)
	c2 := connect(t, url)

	// The third client is refused with Postgres's exact error.
	var pgErr *pgconn.PgError
	if _, err := pgx.Connect(context.Background(), url); !errors.As(err, &pgErr) ||
		pgErr.Code != "53300" || !strings.Contains(pgErr.Message, "too many clients") {
		t.Fatalf("over-limit connect: %+v", err)
	}

	// Cancellation still reaches a full server: the cancel connection
	// is exempt from the cap (it would otherwise be impossible to
	// relieve an overloaded server).
	seedHeavy(t, c1, 400)
	errCh := make(chan error, 1)
	go func() {
		_, err := c1.Exec(context.Background(), heavyQuery)
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := c1.PgConn().CancelRequest(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
			t.Fatalf("cancel under conn cap: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not land")
	}

	// A slot freed by disconnect is reusable.
	c2.Close(context.Background())
	time.Sleep(50 * time.Millisecond) // the server-side deregister is async
	connect(t, url)
}

func TestQueryLog(t *testing.T) {
	var mu sync.Mutex
	type entry struct {
		q   string
		err error
	}
	var got []entry
	addr := startConfiguredServer(t, func(s *pgwire.Server) {
		s.QueryLog = func(q string, d time.Duration, err error) {
			mu.Lock()
			got = append(got, entry{q, err})
			mu.Unlock()
		}
	})
	c := connect(t, fmt.Sprintf("postgres://any@%s/any?sslmode=disable", addr))
	mustExec(t, c, `create table ql (id int primary key)`)
	mustExec(t, c, `insert into ql values ($1)`, int64(1)) // extended protocol
	c.Exec(context.Background(), `select nope from ql`)    // errors, still logged

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 3 {
		t.Fatalf("logged %d entries: %+v", len(got), got)
	}
	last := got[len(got)-1]
	if !strings.Contains(last.q, "select nope") || last.err == nil {
		t.Fatalf("error entry: %+v", last)
	}
	if !strings.Contains(got[len(got)-2].q, "insert into ql") || got[len(got)-2].err != nil {
		t.Fatalf("extended-protocol entry: %+v", got[len(got)-2])
	}
}

// TestShowAndTruncateWire drives the new statements the way clients
// do: pgx probes SHOW on connect-adjacent paths, and TRUNCATE's tag
// must round-trip.
func TestShowAndTruncateWire(t *testing.T) {
	c := connect(t, startServer(t))

	var v string
	if err := c.QueryRow(context.Background(), `show server_version`).Scan(&v); err != nil ||
		v != sql.ServerVersion {
		t.Fatalf("show server_version: %v %q", err, v)
	}
	mustExec(t, c, `set application_name = 'wiretest'`)
	if err := c.QueryRow(context.Background(), `show application_name`).Scan(&v); err != nil ||
		v != "wiretest" {
		t.Fatalf("show application_name: %v %q", err, v)
	}

	mustExec(t, c, `create table tw (id int primary key)`)
	mustExec(t, c, `insert into tw values (1), (2)`)
	tag := mustExec(t, c, `truncate tw`)
	if tag.String() != "TRUNCATE TABLE" {
		t.Fatalf("truncate tag: %q", tag.String())
	}
	var n int64
	if err := c.QueryRow(context.Background(), `select count(*) from tw`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after truncate: %v %d", err, n)
	}
}
