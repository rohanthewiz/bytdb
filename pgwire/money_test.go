package pgwire_test

// N-053 over the wire: money columns, parameters and results through
// pgx in the three query modes, the error codes and the catalog. pgx
// has no money codec of its own, so money travels as text (a string
// argument) and scans into a string; binary results are cash_send's
// int8, which pgx hands back raw.

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMoneyOverWire(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table acct (id int primary key, bal money)`)
	mustExec(t, c, `insert into acct values ($1, $2)`, 1, "$1,234.567")
	mustExec(t, c, `insert into acct values ($1, $2)`, 2, "(7)")
	mustExec(t, c, `insert into acct values (3, 5)`)

	want := map[int]string{1: "$1,234.57", 2: "-$7.00", 3: "$5.00"}
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeExec, pgx.QueryExecModeSimpleProtocol} {
		rows, err := c.Query(ctx, `select id, bal from acct order by id`, mode)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id int
			var bal string
			if err := rows.Scan(&id, &bal); err != nil {
				t.Fatal(err)
			}
			if bal != want[id] {
				t.Errorf("mode %v row %d: %s, want %s", mode, id, bal, want[id])
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	// Binary results: the cents as an int8, OID 790.
	rows, err := c.Query(ctx, `select bal from acct where id = 1`, pgx.QueryExecModeDescribeExec)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal(rows.Err())
	}
	raw := rows.RawValues()[0]
	oid := rows.FieldDescriptions()[0].DataTypeOID
	format := rows.FieldDescriptions()[0].Format
	rows.Close()
	if oid != 790 {
		t.Errorf("money OID %d", oid)
	}
	if format == 1 && (len(raw) != 8 || int64(binary.BigEndian.Uint64(raw)) != 123457) {
		t.Errorf("binary money % x, want int8 123457", raw)
	}

	// Arithmetic results keep their Postgres types.
	var total, scaled string
	var ratio float64
	if err := c.QueryRow(ctx, `select sum(bal), max(bal) * 1.5, max(bal) / min(bal) from acct`).Scan(&total, &scaled, &ratio); err != nil {
		t.Fatal(err)
	}
	if total != "$1,232.57" || scaled != "$1,851.86" || ratio < -176.4 || ratio > -176.3 {
		t.Errorf("sum %s, scaled %s, ratio %v", total, scaled, ratio)
	}

	for q, code := range map[string]string{
		`select 'abc'::money`:                           "22P02",
		`select '92233720368547758.07'::money + '0.01'`: "22003",
		`select '1'::money + 1`:                         "42883",
		`select avg(bal) from acct`:                     "42883",
		`select '10'::money / 0`:                        "22012",
	} {
		_, err := c.Exec(ctx, q)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != code {
			t.Errorf("%s: %v, want SQLSTATE %s", q, err, code)
		}
	}

	var typ string
	if err := c.QueryRow(ctx, `select format_type(a.atttypid, a.atttypmod) from pg_attribute a
		join pg_class c on c.oid = a.attrelid where c.relname = 'acct' and a.attname = 'bal'`).Scan(&typ); err != nil || typ != "money" {
		t.Errorf("format_type = %q %v", typ, err)
	}
}
