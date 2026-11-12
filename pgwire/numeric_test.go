package pgwire_test

// N-048 over the wire: numeric columns, parameters and results through
// pgx (binary by default), the error codes, and the catalog a client
// introspects.

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestNumericOverWire(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table acct (id int primary key, bal numeric(10,2), raw numeric, f float8)`)

	// Parameters of every shape pgx encodes for a numeric: a float64
	// (its shortest text), a string, an int, and pgtype.Numeric, all in
	// binary because Describe types the placeholders numeric.
	exact := pgtype.Numeric{Int: big.NewInt(-12345678901234567), Exp: -9, Valid: true}
	rows := []struct {
		id       int
		bal, raw any
	}{
		{1, 0.1, "123.4500"},
		{2, "19.995", 7},
		{3, int64(-5), exact},
		{4, "0", "0.000"},
	}
	for _, r := range rows {
		mustExec(t, c, `insert into acct (id, bal, raw) values ($1, $2, $3)`, r.id, r.bal, r.raw)
	}
	want := map[int][2]string{
		1: {"0.10", "123.4500"},
		2: {"20.00", "7"},
		3: {"-5.00", "-12345678.901234567"},
		4: {"0.00", "0.000"},
	}
	// Binary results (pgx's default) scanned into strings, then the
	// same through the simple protocol, which is text.
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeCacheStatement, pgx.QueryExecModeSimpleProtocol} {
		r, err := c.Query(ctx, `select id, bal, raw from acct order by id`, mode)
		if err != nil {
			t.Fatal(err)
		}
		for r.Next() {
			var id int
			var bal, raw string
			if err := r.Scan(&id, &bal, &raw); err != nil {
				t.Fatal(err)
			}
			w := want[id]
			if id == 4 && mode == pgx.QueryExecModeCacheStatement {
				// pgx's binary numeric decoder drops a zero's display
				// scale (ndigits 0); the bytes are numeric_send's
				// (TestNumericBinaryMatchesPostgres), so Postgres
				// reads back the same "0" here.
				w = [2]string{"0", "0"}
			}
			if bal != w[0] || raw != w[1] {
				t.Errorf("mode %v row %d: (%s, %s), want %v", mode, id, bal, raw, w)
			}
		}
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
	}

	// pgtype.Numeric keeps the exact digits and the scale.
	var n pgtype.Numeric
	if err := c.QueryRow(ctx, `select raw from acct where id = 3`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n.Int.Cmp(exact.Int) != 0 || n.Exp != -9 {
		t.Errorf("pgtype.Numeric = %v e%d", n.Int, n.Exp)
	}

	// Arithmetic and aggregates come back numeric (OID 1700) and exact.
	var sum, avg, third string
	if err := c.QueryRow(ctx, `select sum(bal), avg(bal), 1/3.0 from acct`).Scan(&sum, &avg, &third); err != nil {
		t.Fatal(err)
	}
	if sum != "15.10" || avg != "3.7750000000000000" || third != "0.33333333333333333333" {
		t.Errorf("sum, avg, 1/3.0 = %s, %s, %s", sum, avg, third)
	}
	desc, err := c.Prepare(ctx, "probe", `select bal, raw * 2, f + raw from acct`)
	if err != nil {
		t.Fatal(err)
	}
	for i, wantOID := range []uint32{1700, 1700, 701} { // numeric + float8 is float8
		if got := desc.Fields[i].DataTypeOID; got != wantOID {
			t.Errorf("column %d OID %d, want %d", i, got, wantOID)
		}
	}

	// Errors: typmod overflow with Postgres's DETAIL, division by zero,
	// bad input, NaN.
	for q, code := range map[string]string{
		`insert into acct (id, bal) values (9, 123456789.5)`: "22003",
		`select 1 / 0.0`:                  "22012",
		`select 'abc'::numeric`:           "22P02",
		`select 'NaN'::numeric`:           "0A000",
		`create table bad (x numeric(0))`: "22023",
	} {
		_, err := c.Exec(ctx, q)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != code {
			t.Errorf("%s: %v, want SQLSTATE %s", q, err, code)
			continue
		}
		if code == "22003" && pe.Detail != "A field with precision 10, scale 2 must round to an absolute value less than 10^8." {
			t.Errorf("overflow DETAIL = %q", pe.Detail)
		}
	}

	// What an introspecting client sees.
	var fmtType, dataType string
	var prec, scale int
	if err := c.QueryRow(ctx, `select format_type(a.atttypid, a.atttypmod), i.data_type,
			i.numeric_precision, i.numeric_scale
		from pg_attribute a join pg_class t on t.oid = a.attrelid
		join information_schema.columns i on i.table_name = t.relname and i.column_name = a.attname
		where t.relname = 'acct' and a.attname = 'bal'`).Scan(&fmtType, &dataType, &prec, &scale); err != nil {
		t.Fatal(err)
	}
	if fmtType != "numeric(10,2)" || dataType != "numeric" || prec != 10 || scale != 2 {
		t.Errorf("catalog: %s %s %d %d", fmtType, dataType, prec, scale)
	}
}
