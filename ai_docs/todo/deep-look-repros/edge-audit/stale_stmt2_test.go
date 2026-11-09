package edgeaudit

import (
	"context"
	"encoding/binary"
	"testing"
)

// Same scenario at the protocol level: one named statement, parsed once,
// executed with per-column binary result formats before and after another
// connection's DDL.
func TestNamedStatementAfterDDL_Protocol(t *testing.T) {
	addr, _, _ := startServer(t, nil)
	ctx := context.Background()
	a := pgxConn(t, addr)
	b := pgxConn(t, addr)
	a.Exec(ctx, `create table t (id int primary key, n int)`)
	a.Exec(ctx, `insert into t values (1, 42)`)
	pc := a.PgConn()
	sd, err := pc.Prepare(ctx, "s", `select * from t`, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("described at Parse: %d columns, oids %d,%d", len(sd.Fields), sd.Fields[0].DataTypeOID, sd.Fields[1].DataTypeOID)
	run := func(label string) {
		rr := pc.ExecPrepared(ctx, "s", nil, nil, []int16{1, 1})
		res := rr.Read()
		if res.Err != nil {
			t.Logf("%s: error: %v", label, res.Err)
			return
		}
		for _, row := range res.Rows {
			if len(row[1]) == 8 {
				t.Logf("%s: %d cols; n decoded as the int8 the RowDescription promised = %d", label, len(row), int64(binary.BigEndian.Uint64(row[1])))
			} else {
				t.Logf("%s: %d cols; n raw=%q", label, len(row), row[1])
			}
		}
	}
	run("before DDL")
	b.Exec(ctx, `alter table t alter column n type text using 'ABCDEFGH'`)
	run("after ALTER TYPE int->text (value is now 'ABCDEFGH')")
	if _, err := b.Exec(ctx, `alter table t add column extra int default 5`); err != nil {
		t.Fatal(err)
	}
	run("after ADD COLUMN (Bind still carries 2 result formats)")
	t.Logf("connection closed by server: %v", pc.IsClosed())
}
