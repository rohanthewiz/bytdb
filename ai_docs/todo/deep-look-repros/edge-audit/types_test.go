package edgeaudit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Differential: every expression is fetched once through the simple
// protocol (text) and once through pgx's default extended/binary path;
// a decode error or a differing value means RowDescription (from
// Describe) and DataRow encoding (from execution) disagree.
func TestDescribeVsExecTypes(t *testing.T) {
	addr, _, _ := startServer(t, nil)
	ctx := context.Background()
	c := pgxConn(t, addr)
	for _, q := range []string{
		`create table t (id int primary key, n int, f float, s text, b bool, by bytea, ts timestamp, d date, u uuid, a text[], j jsonb)`,
		`insert into t values (1, 7, 1.5, 'x', true, '\x0102', '2024-01-02 03:04:05', '2024-01-02', '00000000-0000-0000-0000-000000000001', '{a,b}', '{"k":1}')`,
		`insert into t (id) values (2)`,
	} {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	exprs := []string{
		"1", "1+1", "1.5", "'x'", "true", "null", "n", "n+1", "n*2.5", "f", "f+1", "s", "s||'y'", "b", "not b", "by", "ts", "d", "u", "a", "j",
		"count(*)", "sum(n)", "sum(f)", "avg(n)", "min(ts)", "max(d)", "min(u)", "max(s)",
		"coalesce(n, 0)", "coalesce(s, 'z')", "coalesce(ts, now())", "case when b then n else 0 end", "case when b then ts end",
		"case when b then 1 else 2.5 end", "n::text", "s::text", "n::float", "f::int", "cast(n as bigint)",
		"length(s)", "upper(s)", "abs(n)", "abs(f)", "now()", "current_date", "n = 7", "n is null", "n in (1,7)",
		"j->'k'", "j->>'k'", "array_length(a, 1)", "array_to_string(a, ',')", "nullif(n, 7)", "greatest(n, 3)", "least(f, 3)",
		"n / 2", "n % 2", "-n", "-f", "round(f)", "floor(f)", "(select max(n) from t)", "(select ts from t where id=1)",
		"exists (select 1 from t)", "row_number() over (order by id)", "sum(n) over ()", "lag(ts) over (order by id)",
		"ts::date", "d::timestamp", "u::text", "ts::text", "extract(year from ts)", "date_trunc('day', ts)",
		"gen_random_uuid() is not null", "n::float8/3", "1e20", "f*1e300*1e300", "0.1+0.2", "1000000.0", "9223372036854775807", "-9223372036854775808", "''", "'a\tb'", "'{}'::text[]", "'{a,NULL,\"x y\"}'::text[]", "'[1,2]'::jsonb", "'1970-01-01'::date", "'0001-01-01'::date", "'9999-12-31 23:59:59.999999'::timestamp", "'\\x'::bytea", "string_agg(s, ',')", "count(distinct n)", "bool_and(b)", "f/0", "n/0", "lower(s) like 'x%'", "n between 1 and 9", "$1", "$1::int", "$1::timestamp", "n + $1",
	}
	bad := 0
	for _, e := range exprs {
		q := "select " + e + " as v from t order by id"
		if containsAny(e, "count(", "sum(n)", "sum(f)", "avg(", "min(", "max(") && !containsAny(e, "over", "select") {
			q = "select " + e + " as v from t"
		}
		if containsAny(e, "over") {
			q = "select " + e + " as v from t order by id"
		}
		var args []any
		simpleArgs := []any{pgx.QueryExecModeSimpleProtocol}
		if containsAny(e, "$1") {
			var a any = int64(3)
			if containsAny(e, "timestamp") {
				a = "2024-05-05 00:00:00"
			} else if e == "$1" {
				a = "3"
			}
			args = append(args, a)
			simpleArgs = append(simpleArgs, a)
		}
		txt, err1 := collect(ctx, c, q, simpleArgs...)
		bin, err2 := collect(ctx, c, q, args...)
		if err1 != nil && err2 != nil {
			t.Logf("unsupported (both fail): %-40s %v", e, firstLine(err2))
			continue
		}
		if err1 != nil || err2 != nil || txt != bin {
			bad++
			t.Errorf("MISMATCH %-40s\n   simple/text : %s err=%v\n   extended/bin: %s err=%v", e, txt, err1, bin, err2)
		}
	}
	t.Logf("%d expressions, %d mismatches", len(exprs), bad)
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		for i := 0; i+len(x) <= len(s); i++ {
			if s[i:i+len(x)] == x {
				return true
			}
		}
	}
	return false
}

func firstLine(err error) string {
	s := err.Error()
	if len(s) > 110 {
		s = s[:110]
	}
	return s
}

func collect(ctx context.Context, c *pgx.Conn, q string, args ...any) (string, error) {
	rows, err := c.Query(ctx, q, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	out := ""
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return out, err
		}
		oid := rows.FieldDescriptions()[0].DataTypeOID
		if tm, ok := vals[0].(time.Time); ok {
			vals[0] = tm.UTC().Truncate(time.Second).Format(time.RFC3339)
		}
		out += fmt.Sprintf("[oid=%d %T %v]", oid, vals[0], vals[0])
	}
	return out, rows.Err()
}
