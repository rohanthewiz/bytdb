package pgwire_test

// N-053 over the wire: bit and bit varying through pgx, whose
// pgtype.Bits codec sends and reads Postgres's binary form (a bit count,
// then the bits packed most significant first), alongside text, in the
// three query modes.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestBitOverWire(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table b (id int primary key, f bit(10), v varbit)`)
	// Binary parameters: 10 bits 1011000011 and 3 bits 101, the last
	// byte padded with zero bits.
	mustExec(t, c, `insert into b values ($1, $2, $3)`, 1,
		pgtype.Bits{Bytes: []byte{0xB0, 0xC0}, Len: 10, Valid: true},
		pgtype.Bits{Bytes: []byte{0xA0}, Len: 3, Valid: true})
	// Text parameters.
	mustExec(t, c, `insert into b values ($1, $2, $3)`, 2, "b1111111111", "")

	want := map[int][2]string{1: {"1011000011", "101"}, 2: {"1111111111", ""}}
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeDescribeExec, pgx.QueryExecModeExec, pgx.QueryExecModeSimpleProtocol} {
		rows, err := c.Query(ctx, `select id, f::text, v::text, f, v from b order by id`, mode)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id int
			var f, v string
			var fb, vb pgtype.Bits
			if err := rows.Scan(&id, &f, &v, &fb, &vb); err != nil {
				t.Fatalf("mode %v: %v", mode, err)
			}
			if w := want[id]; f != w[0] || v != w[1] {
				t.Errorf("mode %v row %d: (%s, %s), want %v", mode, id, f, v, w)
			}
			if id == 1 && (fb.Len != 10 || len(fb.Bytes) != 2 || fb.Bytes[0] != 0xB0 || fb.Bytes[1] != 0xC0 ||
				vb.Len != 3 || len(vb.Bytes) != 1 || vb.Bytes[0] != 0xA0) {
				t.Errorf("mode %v: Bits %+v %+v", mode, fb, vb)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	var oid uint32
	var typ string
	if err := c.QueryRow(ctx, `select a.atttypid, format_type(a.atttypid, a.atttypmod) from pg_attribute a
		join pg_class c on c.oid = a.attrelid where c.relname = 'b' and a.attname = 'f'`).Scan(&oid, &typ); err != nil ||
		oid != 1560 || typ != "bit(10)" {
		t.Errorf("catalog: %d %q %v", oid, typ, err)
	}
}
