package pgwire_test

// End-to-end jsonb coverage through pgx: the jsonb (3802) OID in
// RowDescription, the version-prefixed binary format both directions
// (pgx's default in the extended protocol), and the text literal path.

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestJSONBWire(t *testing.T) {
	c := connect(t, startServer(t))
	ctx := context.Background()
	mustExec(t, c, `create table docs (
		id int primary key,
		body jsonb
	)`)

	// Extended protocol: pgx marshals a Go map as binary jsonb; the
	// scan back unmarshals the stored document.
	want := map[string]any{"name": "grace", "tags": []any{"a", "b"}, "n": float64(3)}
	mustExec(t, c, `insert into docs values ($1, $2)`, int64(1), want)

	var got map[string]any
	if err := c.QueryRow(ctx, `select body from docs where id = 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip = %v, want %v", got, want)
	}

	// The stored text is canonical regardless of the wire spelling.
	var raw json.RawMessage
	err := c.QueryRow(ctx, `select body from docs where id = 1`).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"n":3,"name":"grace","tags":["a","b"]}` {
		t.Fatalf("canonical text = %s", raw)
	}

	// Text-literal path: a non-canonical literal canonicalizes on
	// write, and a differently-spelled literal still compares equal.
	mustExec(t, c, `insert into docs values (2, '{ "b": 2, "a": 1 }')`)
	var id int64
	if err := c.QueryRow(ctx,
		`select id from docs where body = '{"a": 1, "b": 2}'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != 2 {
		t.Fatalf("document equality over the wire: id = %d", id)
	}

	// Malformed JSON in a parameter is a clean SQL error, not a hang.
	if _, err := c.Exec(ctx, `insert into docs values (3, $1)`, json.RawMessage(`{oops`)); err == nil {
		t.Fatal("malformed jsonb parameter accepted")
	}
}
