package pgwire_test

// N-053 over the wire: citext columns through pgx in the three query
// modes. citext has no fixed oid in Postgres (CREATE EXTENSION assigns
// one) and pgx has no codec for it, so it travels as text both ways;
// bytdb reports its own fixed oid, found by name in pg_type as clients
// look it up.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestCITextOverWire(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create extension if not exists citext`)
	mustExec(t, c, `create table u (id int primary key, email citext unique)`)
	mustExec(t, c, `insert into u values ($1, $2)`, 1, "Ada@Example.com")
	mustExec(t, c, `insert into u values ($1, $2)`, 2, "grace@example.COM")

	var oid uint32
	if err := c.QueryRow(ctx, `select oid from pg_type where typname = 'citext'`).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeDescribeExec, pgx.QueryExecModeExec, pgx.QueryExecModeSimpleProtocol} {
		rows, err := c.Query(ctx, `select id, email from u where email = $1`, mode, "ADA@EXAMPLE.COM")
		if err != nil {
			t.Fatal(err)
		}
		if got := rows.FieldDescriptions()[1].DataTypeOID; got != oid {
			t.Errorf("mode %v: email column oid %d, pg_type says %d", mode, got, oid)
		}
		n := 0
		for rows.Next() {
			var id int
			var email string
			if err := rows.Scan(&id, &email); err != nil {
				t.Fatal(err)
			}
			if id != 1 || email != "Ada@Example.com" {
				t.Errorf("mode %v: (%d, %s)", mode, id, email)
			}
			n++
		}
		if err := rows.Err(); err != nil || n != 1 {
			t.Fatalf("mode %v: %d rows, %v", mode, n, err)
		}
	}
	var typ string
	if err := c.QueryRow(ctx, `select format_type(a.atttypid, a.atttypmod) from pg_attribute a
		join pg_class c on c.oid = a.attrelid where c.relname = 'u' and a.attname = 'email'`).Scan(&typ); err != nil || typ != "citext" {
		t.Errorf("format_type = %q %v", typ, err)
	}
}
