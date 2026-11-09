package edgeaudit

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/lib/pq"
)

// What parameter OIDs does Describe report for common placeholder
// positions, and can pgx (binary, typed) and lib/pq (text, untyped)
// actually run them with a natural Go int argument?
func TestParamInference(t *testing.T) {
	addr, _, _ := startServer(t, nil)
	ctx := context.Background()
	c := pgxConn(t, addr)
	if _, err := c.Exec(ctx, `create table t (id int primary key, n int, f float, s text, ts timestamp)`); err != nil {
		t.Fatal(err)
	}
	c.Exec(ctx, `insert into t values (1, 7, 1.5, 'x', now())`)
	pq, err := sql.Open("postgres", dsn(addr))
	if err != nil {
		t.Fatal(err)
	}
	defer pq.Close()
	for _, q := range []string{
		"select id from t where n = $1",
		"select id from t where n > $1 + 1",
		"select n + $1 from t",
		"update t set n = n + $1 where id = 1",
		"select id from t where n = $1::int",
		"select $1::int",
		"select $1::bigint + 1",
		"select id from t order by id limit $1",
		"select id from t order by id limit $1 offset $1",
		"select id from t where n in ($1, 8)",
		"select id from t where n between $1 and 9",
		"select coalesce($1, n) from t",
		"select id from t where $1 = n",
		"select id from t where n = $1 or $1 = 0",
		"select id from t where ($1 = 0 or n = $1)",
		"insert into t (id, n) values (2, $1) on conflict (id) do update set n = $1",
		"select id from t where f < $1",
		"select case when n > $1 then 1 else 0 end from t",
		"select id from t where length(s) = $1",
		"select id from t where id = (select max(id) from t where n >= $1)",
	} {
		sd, err := c.PgConn().Prepare(ctx, "", q, nil)
		oids := "prepare-error"
		if err == nil {
			oids = ""
			for _, o := range sd.ParamOIDs {
				oids += " " + map[uint32]string{20: "int8", 25: "text", 701: "float8", 16: "bool", 1184: "timestamptz"}[o]
			}
		}
		rows, perr := c.Query(ctx, q, int64(7))
		if perr == nil {
			rows.Close()
			perr = rows.Err()
		}
		prows, qerr := pq.QueryContext(ctx, q, 7)
		if qerr == nil {
			for prows.Next() {
			}
			qerr = prows.Err()
			prows.Close()
		}
		status := "ok  "
		if perr != nil || qerr != nil {
			status = "FAIL"
		}
		t.Logf("%s params=[%s] %-70s\n        pgx: %v\n        lib/pq: %v", status, oids, q, short(perr), short(qerr))
	}
}

func short(err error) string {
	if err == nil {
		return "ok"
	}
	s := err.Error()
	if len(s) > 140 {
		s = s[:140]
	}
	return s
}
