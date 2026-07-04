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

// TestPsqlBackslashQueries runs the load-bearing queries psql 17
// sends for \dt and \d <table> — CASE, IN, !~, OPERATOR()/COLLATE,
// functions with arguments, and the three-arm publication UNION —
// through the wire.
func TestPsqlBackslashQueries(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table users (id int primary key, name text, age int)`)

	// \dt
	var schema, name, typ, owner string
	err := c.QueryRow(ctx, `SELECT n.nspname as "Schema",
  c.relname as "Name",
  CASE c.relkind WHEN 'r' THEN 'table' WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' WHEN 'i' THEN 'index' WHEN 'S' THEN 'sequence' WHEN 't' THEN 'TOAST table' WHEN 'f' THEN 'foreign table' WHEN 'p' THEN 'partitioned table' WHEN 'I' THEN 'partitioned index' END as "Type",
  pg_catalog.pg_get_userbyid(c.relowner) as "Owner"
FROM pg_catalog.pg_class c
     LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
     LEFT JOIN pg_catalog.pg_am am ON am.oid = c.relam
WHERE c.relkind IN ('r','p','')
      AND n.nspname <> 'pg_catalog'
      AND n.nspname !~ '^pg_toast'
      AND n.nspname <> 'information_schema'
  AND pg_catalog.pg_table_is_visible(c.oid)
ORDER BY 1,2`).Scan(&schema, &name, &typ, &owner)
	if err != nil || schema != "public" || name != "users" || typ != "table" {
		t.Fatalf("\\dt: %v %s.%s %s %s", err, schema, name, typ, owner)
	}

	// \d users, first step: name resolution.
	var oid int64
	err = c.QueryRow(ctx, `SELECT c.oid, n.nspname, c.relname
FROM pg_catalog.pg_class c
     LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname OPERATOR(pg_catalog.~) '^(users)$' COLLATE pg_catalog.default
  AND pg_catalog.pg_table_is_visible(c.oid)
ORDER BY 2, 3`).Scan(&oid, &schema, &name)
	if err != nil || name != "users" || oid == 0 {
		t.Fatalf("name resolution: %v %d %s", err, oid, name)
	}

	// The publication UNION probe returns zero rows without erroring.
	rows, err := c.Query(ctx, `SELECT pubname, NULL, NULL
FROM pg_catalog.pg_publication p
     JOIN pg_catalog.pg_publication_namespace pn ON p.oid = pn.pnpubid
     JOIN pg_catalog.pg_class pc ON pc.relnamespace = pn.pnnspid
WHERE pc.oid ='1' and pg_catalog.pg_relation_is_publishable('1')
UNION
SELECT pubname, NULL, NULL FROM pg_catalog.pg_publication p
WHERE p.puballtables AND pg_catalog.pg_relation_is_publishable('1')
ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		t.Fatal("expected no publications")
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
}
