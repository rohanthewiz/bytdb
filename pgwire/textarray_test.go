package pgwire_test

// End-to-end text[] coverage through pgx: the _text (1009) OID in
// RowDescription, binary array format both directions (pgx's default
// for known OIDs in the extended protocol), and the text literal path
// of the simple protocol.

import (
	"context"
	"reflect"
	"testing"
)

func TestTextArrayWire(t *testing.T) {
	c := connect(t, startServer(t))
	ctx := context.Background()
	mustExec(t, c, `create table pages (
		id int primary key,
		positions text[]
	)`)

	// Extended protocol: pgx binds []string as a binary _text array and
	// scans the binary array format back.
	want := []string{"header", "main content", `quo"ted`, `back\slash`}
	mustExec(t, c, `insert into pages values ($1, $2)`, int64(1), want)

	var got []string
	if err := c.QueryRow(ctx, `select positions from pages where id = 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip = %q, want %q", got, want)
	}

	// Empty arrays round-trip as zero-dimension binary arrays.
	mustExec(t, c, `insert into pages values ($1, $2)`, int64(2), []string{})
	if err := c.QueryRow(ctx, `select positions from pages where id = 2`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty array = %q", got)
	}

	// Text-literal path: a quoted literal parameter canonicalizes and
	// serves back; ANY-membership works over the stored value.
	mustExec(t, c, `insert into pages values (3, '{ sidebar , footer }')`)
	var lit []string
	if err := c.QueryRow(ctx, `select positions from pages where 'footer' = any(positions)`).Scan(&lit); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lit, []string{"sidebar", "footer"}) {
		t.Fatalf("literal path = %q", lit)
	}

	// LIKE/ILIKE ride the same wire; the search shape apps rewrite to.
	var id int64
	err := c.QueryRow(ctx,
		`select id from pages where array_to_string(positions, ' ') ilike $1`, "%MAIN%").Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 {
		t.Fatalf("ilike over array_to_string: id = %d", id)
	}
}
