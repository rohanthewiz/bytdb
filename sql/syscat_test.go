package sql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestSelectWithoutFrom(t *testing.T) {
	d := openDB(t)

	res := exec(t, d, `select 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) ||
		!reflect.DeepEqual(res.Cols, []string{"?column?"}) ||
		res.Types[0] != bytdb.TInt {
		t.Fatalf("select 1: %v %v %v", res.Rows, res.Cols, res.Types)
	}
	res = exec(t, d, `select version()`)
	if res.Cols[0] != "version" || !strings.HasPrefix(res.Rows[0][0].(string), "PostgreSQL") {
		t.Fatalf("version(): %v %v", res.Cols, res.Rows)
	}
	res = exec(t, d, `select pg_catalog.version()`)
	if !strings.HasPrefix(res.Rows[0][0].(string), "PostgreSQL") {
		t.Fatalf("qualified version(): %v", res.Rows)
	}
	res = exec(t, d, `select current_database(), current_schema(), -2, 'x', true`)
	want := [][]any{{"bytdb", "public", int64(-2), "x", true}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("got %v", res.Rows)
	}
	// Literals mix with columns over a real table.
	seedUsers(t, d)
	res = exec(t, d, `select 1, name from users where id = 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1), "ada"}}) {
		t.Fatalf("mixed: %v", res.Rows)
	}
	if _, err := d.Exec(`select * `); err == nil || !strings.Contains(err.Error(), "FROM") {
		t.Fatalf("select * without FROM: %v", err)
	}
}

func TestSystemCatalog(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	exec(t, d, `create table orders (id int primary key, user_id int, total float)`)
	exec(t, d, `create unique index by_city on users (city, id)`)

	// pg_class lists tables and indexes; bare names resolve because
	// pg_catalog is on the search path.
	res := exec(t, d, `select relname from pg_class where relkind = 'r' order by 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"orders"}, {"users"}}) {
		t.Fatalf("pg_class tables: %v", res.Rows)
	}
	res = exec(t, d, `select relname from pg_catalog.pg_class where relkind = 'i' order by 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"by_city"}, {"orders_pkey"}, {"users_pkey"}}) {
		t.Fatalf("pg_class indexes: %v", res.Rows)
	}

	// The namespace join psql-style tools lean on.
	res = exec(t, d, `select c.relname from pg_class c
		join pg_namespace n on n.oid = c.relnamespace
		where n.nspname = 'public' and c.relkind = 'r' order by 1`)
	if len(res.Rows) != 2 {
		t.Fatalf("namespace join: %v", res.Rows)
	}

	// Column introspection: pg_attribute joined to pg_class and
	// pg_type, primary-key not-null included.
	res = exec(t, d, `select a.attname, t.typname, a.attnotnull
		from pg_attribute a
		join pg_class c on c.oid = a.attrelid
		join pg_type t on t.oid = a.atttypid
		where c.relname = 'users' order by a.attnum`)
	want := [][]any{
		{"id", "int8", true}, {"name", "text", false},
		{"age", "int8", false}, {"city", "text", false},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("pg_attribute join: %v", res.Rows)
	}

	// Primary keys and unique indexes via pg_index.
	res = exec(t, d, `select c.relname, i.indisunique, i.indkey from pg_index i
		join pg_class c on c.oid = i.indexrelid
		where i.indisprimary = false and i.indnatts = 2`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"by_city", true, "4 1"}}) {
		t.Fatalf("pg_index: %v", res.Rows)
	}

	// information_schema, including the folded-function probe GORM's
	// HasTable sends.
	res = exec(t, d, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = CURRENT_SCHEMA() AND table_name = 'users' AND table_type = 'BASE TABLE'`)
	if !reflect.DeepEqual(res.Rows, [][]any{{int64(1)}}) {
		t.Fatalf("HasTable probe: %v", res.Rows)
	}
	res = exec(t, d, `select column_name, data_type, is_nullable from information_schema.columns
		where table_name = 'orders' order by ordinal_position`)
	want = [][]any{
		{"id", "bigint", "NO"}, {"user_id", "bigint", "YES"}, {"total", "double precision", "YES"},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("info columns: %v", res.Rows)
	}

	// Aggregates group over virtual rows like any others.
	res = exec(t, d, `select relkind, count(*) from pg_class group by relkind order by 1`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"i", int64(3)}, {"r", int64(2)}}) {
		t.Fatalf("group by relkind: %v", res.Rows)
	}

	// public.users is plain users; information_schema is not on the
	// search path; unknown schemas fail.
	if res := exec(t, d, `select count(*) from public.users`); res.Rows[0][0] != int64(5) {
		t.Fatalf("public.users: %v", res.Rows)
	}
	if _, err := d.Exec(`select * from tables`); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("bare information_schema name: %v", err)
	}
	if _, err := d.Exec(`select * from myschema.t`); err == nil || !strings.Contains(err.Error(), "no such schema") {
		t.Fatalf("unknown schema: %v", err)
	}
}

func TestSystemCatalogReadOnly(t *testing.T) {
	d := openDB(t)
	for _, q := range []string{
		`insert into pg_class values (1, 'x', 1, 'r', 1, 'p', false, 0.0, 0)`,
		`update pg_catalog.pg_class set relname = 'x'`,
		`delete from information_schema.tables`,
		`create table pg_class (id int primary key)`,
		`create table pg_catalog.mine (id int primary key)`,
		`drop table pg_catalog.pg_type`,
		`alter table pg_class add column x int`,
	} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("Exec(%q): err %v, want read-only", q, err)
		}
	}
}

func TestOrderByOrdinal(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	res := exec(t, d, `select city, age from users order by 1, 2 desc limit 3`)
	want := [][]any{{"austin", int64(39)}, {"london", int64(41)}, {"london", int64(36)}}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("ordinals: %v", res.Rows)
	}
	// Aggregate queries take ordinals too.
	res = exec(t, d, `select city, count(*) from users group by city order by 2 desc, 1 limit 2`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"london", int64(2)}, {"nyc", int64(2)}}) {
		t.Fatalf("agg ordinals: %v", res.Rows)
	}
	for _, q := range []string{
		`select city from users order by 3`,
		`select city, count(*) from users group by city order by 5`,
		`select city from users order by 'x'`,
	} {
		if _, err := d.Exec(q); err == nil {
			t.Errorf("Exec(%q): want error", q)
		}
	}
}

// TestInfoSchemaConstraints pins the information_schema constraint
// pair ORMs join to introspect keys: table_constraints lists each
// constraint with its kind; key_column_usage lists key columns in key
// order, with the parent-key position on foreign-key rows.
func TestInfoSchemaConstraints(t *testing.T) {
	d := openDB(t)
	for _, q := range []string{
		`create table orgs (id int, region text, code text, primary key (region, id))`,
		`create unique index orgs_code on orgs (code)`,
		`create index orgs_plain on orgs (code)`,
		`create table users (id int primary key, org_region text, org_id int,
			age int constraint users_age_chk check (age >= 0),
			constraint users_org_fk foreign key (org_region, org_id) references orgs (region, id))`,
	} {
		exec(t, d, q)
	}

	res := exec(t, d, `select table_name, constraint_name, constraint_type, nulls_distinct
		from information_schema.table_constraints
		where table_schema = 'public' order by table_name, constraint_name`)
	want := [][]any{
		{"orgs", "orgs_code", "UNIQUE", "YES"},
		{"orgs", "orgs_pkey", "PRIMARY KEY", nil},
		{"users", "users_age_chk", "CHECK", nil},
		{"users", "users_org_fk", "FOREIGN KEY", nil},
		{"users", "users_pkey", "PRIMARY KEY", nil},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("table_constraints: %v", res.Rows)
	}

	// The canonical ORM join: primary-key columns, in key order.
	res = exec(t, d, `select kcu.column_name, kcu.ordinal_position
		from information_schema.table_constraints tc
		join information_schema.key_column_usage kcu
		  on kcu.constraint_name = tc.constraint_name and kcu.table_name = tc.table_name
		where tc.table_name = 'orgs' and tc.constraint_type = 'PRIMARY KEY'
		order by kcu.ordinal_position`)
	if !reflect.DeepEqual(res.Rows, [][]any{{"region", int64(1)}, {"id", int64(2)}}) {
		t.Fatalf("pk columns: %v", res.Rows)
	}

	// Foreign-key columns carry their position in the parent key; the
	// CHECK constraint contributes no key columns.
	res = exec(t, d, `select constraint_name, column_name, ordinal_position, position_in_unique_constraint
		from information_schema.key_column_usage where table_name = 'users'
		order by constraint_name, ordinal_position`)
	want = [][]any{
		{"users_org_fk", "org_region", int64(1), int64(1)},
		{"users_org_fk", "org_id", int64(2), int64(2)},
		{"users_pkey", "id", int64(1), nil},
	}
	if !reflect.DeepEqual(res.Rows, want) {
		t.Fatalf("key_column_usage: %v", res.Rows)
	}

	// Both names are reserved like the rest of the catalog.
	if _, err := d.Exec(`delete from information_schema.table_constraints`); err == nil {
		t.Fatal("write to table_constraints accepted")
	}
}
