package pgwire_test

// returning_test.go: INSERT/UPDATE/DELETE ... RETURNING across the
// wire — the Describe-then-Execute shape ORMs use to read back
// server-generated ids, plus the simple protocol's inline results.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestReturningOverWire(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table users (id serial primary key, name text)`)

	// The ORM idiom: pgx's default extended protocol, so Parse/Describe
	// must announce the row shape before any row exists.
	var id int64
	if err := c.QueryRow(ctx,
		`insert into users (name) values ($1) returning id`, "ada").Scan(&id); err != nil || id != 1 {
		t.Fatalf("insert returning: %v id=%d", err, id)
	}

	// Multi-row RETURNING, and the command tag still counts inserts.
	rows, err := c.Query(ctx, `insert into users (name) values ($1), ($2) returning id, name`, "grace", "alan")
	if err != nil {
		t.Fatal(err)
	}
	var got [][2]any
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		got = append(got, [2]any{id, name})
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if tag := rows.CommandTag(); tag.String() != "INSERT 0 2" {
		t.Fatalf("tag: %q", tag.String())
	}
	if len(got) != 2 || got[0] != [2]any{int64(2), "grace"} || got[1] != [2]any{int64(3), "alan"} {
		t.Fatalf("rows: %v", got)
	}

	// UPDATE and DELETE round-trip their images too.
	var name string
	if err := c.QueryRow(ctx,
		`update users set name = $1 where id = $2 returning name`, "ADA", int64(1)).Scan(&name); err != nil || name != "ADA" {
		t.Fatalf("update returning: %v %q", err, name)
	}
	if err := c.QueryRow(ctx,
		`delete from users where id = $1 returning name`, int64(3)).Scan(&name); err != nil || name != "alan" {
		t.Fatalf("delete returning: %v %q", err, name)
	}

	// Zero matched rows: RowDescription with no DataRows, which pgx
	// surfaces as ErrNoRows rather than a protocol error.
	err = c.QueryRow(ctx, `delete from users where id = 99 returning id`).Scan(&id)
	if err != pgx.ErrNoRows {
		t.Fatalf("empty returning: %v", err)
	}

	// The simple protocol path sends its RowDescription inline.
	var sid int64
	if err := c.QueryRow(ctx, `insert into users (name) values ('barbara') returning id`,
		pgx.QueryExecModeSimpleProtocol).Scan(&sid); err != nil || sid != 4 {
		t.Fatalf("simple protocol returning: %v id=%d", err, sid)
	}
}
