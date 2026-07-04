package pgwire_test

// Introspection probes real clients and ORMs send on connect or
// before migrations, verbatim, through the wire.

import (
	"context"
	"strings"
	"testing"
)

func TestClientProbes(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table users (id int primary key, name text, age int)`)
	mustExec(t, c, `create index by_age on users (age)`)

	// SQLAlchemy's first breath.
	var v string
	if err := c.QueryRow(ctx, `select pg_catalog.version()`).Scan(&v); err != nil || !strings.HasPrefix(v, "PostgreSQL") {
		t.Fatalf("version probe: %v %q", err, v)
	}
	// The universal liveness check.
	var one int64
	if err := c.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("select 1: %v %d", err, one)
	}

	// GORM Migrator.HasTable, verbatim (parameterized over extended
	// protocol, with a folded function in a predicate).
	var n int64
	if err := c.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = CURRENT_SCHEMA() AND table_name = $1 AND table_type = 'BASE TABLE'`,
		"users").Scan(&n); err != nil || n != 1 {
		t.Fatalf("HasTable: %v %d", err, n)
	}

	// Column introspection in the pg_catalog idiom (ActiveRecord /
	// SQLAlchemy shape, minus casts).
	rows, err := c.Query(ctx, `select a.attname, t.typname, a.attnotnull
		from pg_catalog.pg_attribute a
		join pg_catalog.pg_class c on c.oid = a.attrelid
		join pg_catalog.pg_type t on t.oid = a.atttypid
		where c.relname = $1 order by a.attnum`, "users")
	if err != nil {
		t.Fatal(err)
	}
	type att struct {
		name, typ string
		notnull   bool
	}
	var atts []att
	for rows.Next() {
		var a att
		if err := rows.Scan(&a.name, &a.typ, &a.notnull); err != nil {
			t.Fatal(err)
		}
		atts = append(atts, a)
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if len(atts) != 3 || atts[0] != (att{"id", "int8", true}) || atts[2] != (att{"age", "int8", false}) {
		t.Fatalf("attributes: %+v", atts)
	}

	// Index discovery via pg_index.
	var idx string
	var unique bool
	if err := c.QueryRow(ctx, `select c.relname, i.indisunique from pg_index i
		join pg_class c on c.oid = i.indexrelid
		where i.indisprimary = false`).Scan(&idx, &unique); err != nil || idx != "by_age" || unique {
		t.Fatalf("pg_index: %v %q %v", err, idx, unique)
	}

	// The catalog is read-only, with a clean error over the wire.
	if _, err := c.Exec(ctx, `drop table pg_catalog.pg_class`); err == nil ||
		!strings.Contains(err.Error(), "read-only") {
		t.Fatalf("catalog write: %v", err)
	}
}
