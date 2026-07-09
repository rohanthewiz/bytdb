package pgwire_test

// returning_test.go: INSERT/UPDATE/DELETE ... RETURNING over the wire
// — the round trip ORMs depend on to learn server-generated IDs. pgx's
// default path exercises the extended protocol end to end: Describe
// must announce the row description before execution, rows arrive
// before the command tag, and the tag still reports the affected
// count.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestReturningOverWire(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table users (id serial primary key, name text)`)

	// QueryRow over INSERT is the ORM idiom (pgx, GORM, SQLAlchemy all
	// reduce to this once IDs are server-generated).
	var id int64
	if err := c.QueryRow(ctx, `insert into users (name) values ($1) returning id`, "ada").Scan(&id); err != nil {
		t.Fatalf("insert returning: %v", err)
	}
	if id != 1 {
		t.Fatalf("id: got %d, want 1", id)
	}

	// Multi-row RETURNING streams like a SELECT.
	rows, err := c.Query(ctx, `insert into users (name) values ('grace'), ('alan') returning id, name`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		got[id] = name
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	// The command tag keeps the affected count alongside the rows.
	if tag := rows.CommandTag(); tag.String() != "INSERT 0 2" {
		t.Fatalf("tag: got %q, want INSERT 0 2", tag)
	}
	if len(got) != 2 || got[2] != "grace" || got[3] != "alan" {
		t.Fatalf("rows: got %v", got)
	}

	// UPDATE and DELETE return their rows the same way.
	var name string
	if err := c.QueryRow(ctx, `update users set name = 'ADA' where id = 1 returning name`).Scan(&name); err != nil || name != "ADA" {
		t.Fatalf("update returning: %v %q", err, name)
	}
	if err := c.QueryRow(ctx, `delete from users where id = 1 returning name`).Scan(&name); err != nil || name != "ADA" {
		t.Fatalf("delete returning: %v %q", err, name)
	}

	// No matching row: pgx reports ErrNoRows from the empty result,
	// exactly as a SELECT would.
	err = c.QueryRow(ctx, `delete from users where id = 999 returning id`).Scan(&id)
	if err != pgx.ErrNoRows {
		t.Fatalf("no-match delete: got %v, want pgx.ErrNoRows", err)
	}

	// The genuine simple protocol ('Q', RowDescription inline, text
	// format) carries the rows too — pgx only takes this path when the
	// exec mode says so.
	var last int64
	if err := c.QueryRow(ctx, `insert into users (name) values ('barbara') returning id`,
		pgx.QueryExecModeSimpleProtocol).Scan(&last); err != nil || last != 4 {
		t.Fatalf("simple-protocol insert: %v id %d, want 4", err, last)
	}
}
