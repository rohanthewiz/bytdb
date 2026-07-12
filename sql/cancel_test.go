package sql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// seedMany creates table n(id int primary key, v int) with rows rows.
func seedMany(t *testing.T, d *DB, rows int) {
	t.Helper()
	exec(t, d, `create table n (id int primary key, v int)`)
	var b strings.Builder
	b.WriteString("insert into n values ")
	for i := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "(%d, %d)", i, i%7)
	}
	exec(t, d, b.String())
}

func TestExecCtxPreCanceled(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := d.ExecCtx(ctx, `select * from users`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled in the chain", err)
	}

	// The DB itself must be unharmed: the same statement runs fine.
	if _, err := d.ExecCtx(context.Background(), `select * from users`); err != nil {
		t.Fatal(err)
	}
}

func TestExecCtxCancelMidQuery(t *testing.T) {
	d := openDB(t)
	seedMany(t, d, 500)

	// A triple self cross join (500^3 combined rows) runs long enough
	// that a cancel arriving shortly after start must interrupt it.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := d.ExecCtx(ctx, `select count(*) from n a, n b, n c where a.v + b.v + c.v = 100`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled in the chain", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancel took %v to take effect", elapsed)
	}
}

func TestStatementTimeout(t *testing.T) {
	d := openDB(t)
	seedMany(t, d, 500)
	s := d.NewSession()
	defer s.Close()

	if _, err := s.Exec(`set statement_timeout = '30ms'`); err != nil {
		t.Fatal(err)
	}
	_, err := s.Exec(`select count(*) from n a, n b, n c where a.v + b.v + c.v = 100`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded in the chain", err)
	}

	// Fast statements still succeed under the timeout, and disabling it
	// (SET ... TO DEFAULT) restores unbounded execution.
	if _, err := s.Exec(`select count(*) from n`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(`set statement_timeout to default`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(`select count(*) from n a, n b where a.v = b.v and a.id < 30`); err != nil {
		t.Fatal(err)
	}
}

func TestStatementTimeoutCancelsWrites(t *testing.T) {
	d := openDB(t)
	seedMany(t, d, 2000)
	s := d.NewSession()
	defer s.Close()

	if _, err := s.Exec(`set statement_timeout = '5ms'`); err != nil {
		t.Fatal(err)
	}
	// An UPDATE whose scan-heavy subquery outlives the deadline must
	// abort — and, running in its own transaction, leave no partial rows.
	_, err := s.Exec(`update n set v = (select count(*) from n a, n b where a.v = n.v and b.v = n.v)`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded in the chain", err)
	}
	if _, err := s.Exec(`set statement_timeout = 0`); err != nil {
		t.Fatal(err)
	}
	res, err := s.Exec(`select count(*) from n where v >= 7`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][0] != int64(0) {
		t.Fatalf("timed-out UPDATE leaked %v rows", res.Rows[0][0])
	}
}

func TestSetParsingAndForms(t *testing.T) {
	d := openDB(t)
	s := d.NewSession()
	defer s.Close()

	for _, q := range []string{
		`set statement_timeout = 100`,
		`set statement_timeout = '2s'`,
		`set statement_timeout to '250ms'`,
		`set session statement_timeout = '1min'`,
		`set local statement_timeout = 0`,
		`reset statement_timeout`,
		`set search_path to "$user", public`, // remembered, ignored
		`set extra_float_digits = 3`,
		`set timezone = 'UTC'`,
		`set time zone 'UTC'`,
		`set application_name = 'myapp'`,
		`reset all`,
	} {
		res, err := s.Exec(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		want := "SET"
		if strings.HasPrefix(q, "reset") {
			want = "RESET"
		}
		if res.Tag != want {
			t.Fatalf("%s: tag %q, want %q", q, res.Tag, want)
		}
	}

	for _, q := range []string{
		`set statement_timeout = '-5s'`,
		`set statement_timeout = 'soon'`,
		`set statement_timeout = '5 fortnights'`,
	} {
		if _, err := s.Exec(q); err == nil {
			t.Fatalf("%s: want error", q)
		}
	}
}
