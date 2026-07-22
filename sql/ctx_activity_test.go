package sql

// Coverage for the context-bounded execution wrappers, the
// pg_stat_activity provider hook, and the view branch of pg_class
// synthesis — API surface reached only through the wire server or a
// direct embedder, and therefore not exercised by the plain d.Exec path
// the rest of the suite uses.

import (
	"context"
	"testing"
)

func TestContextExecWrappers(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, n int)`)
	exec(t, d, `insert into t values (1, 10), (2, 20)`)

	ctx := context.Background()
	st, err := d.Prepare(`select n from t where id = $1`)
	if err != nil {
		t.Fatal(err)
	}

	// Session.ExecStmtCtx — the session-scoped, ctx-bounded prepared exec.
	sess := d.NewSession()
	defer sess.Close()
	res, err := sess.ExecStmtCtx(ctx, st, int64(1))
	if err != nil {
		t.Fatalf("ExecStmtCtx: %v", err)
	}
	if got := res.Rows[0][0]; got != int64(10) {
		t.Fatalf("ExecStmtCtx result = %v, want 10", got)
	}

	// Stmt.ExecCtx — the DB-scoped, ctx-bounded prepared exec.
	res, err = st.ExecCtx(ctx, int64(2))
	if err != nil {
		t.Fatalf("Stmt.ExecCtx: %v", err)
	}
	if got := res.Rows[0][0]; got != int64(20) {
		t.Fatalf("Stmt.ExecCtx result = %v, want 20", got)
	}

	// An already-cancelled context must fail rather than run.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.ExecCtx(cancelled, int64(1)); err == nil {
		t.Error("Stmt.ExecCtx with a cancelled context returned no error")
	}
}

func TestActivityProviderFeedsStatActivity(t *testing.T) {
	d := openDB(t)
	// Install a synthetic pg_stat_activity source, as the wire server does.
	d.SetActivityProvider(func() []Activity {
		return []Activity{
			{PID: 42, User: "ada", State: "active", Query: "select 1"},
			{PID: 43, User: "grace", State: "idle", Query: "select 2"},
		}
	})
	res := exec(t, d, `select pid, usename, state from pg_stat_activity order by pid`)
	if len(res.Rows) != 2 {
		t.Fatalf("pg_stat_activity returned %d rows, want 2", len(res.Rows))
	}
	if got := res.Rows[0][0]; got != int64(42) {
		t.Errorf("first backend pid = %v, want 42", got)
	}
}

func TestViewAppearsInPgClass(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key)`)
	exec(t, d, `create view v as select id from t`)
	// Reading pg_class synthesizes one row per view, which is what calls
	// viewOID to assign the view's stable synthetic oid.
	res := exec(t, d, `select relname, relkind from pg_class where relname = 'v'`)
	if len(res.Rows) != 1 {
		t.Fatalf("view not present in pg_class (%d rows)", len(res.Rows))
	}
	if got := res.Rows[0][1]; got != "v" {
		t.Errorf("relkind for view = %v, want \"v\"", got)
	}
}
