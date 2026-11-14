package sql

// N-049: the DDL and catalog gaps that GORM's captured statements hit
// (pgwire/orm_verbatim_test.go drives the statements themselves). These
// pin each piece on its own, because a GORM query joins several catalog
// views and a gap in one can hide behind another's filter.

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestBareUniqueIndexNotAConstraint: CREATE UNIQUE INDEX makes an
// index, not a UNIQUE constraint. Every constraint view leaves it out,
// as Postgres's do, while UNIQUE written as a constraint (column-level,
// table-level, ALTER TABLE ADD) is listed in all of them.
func TestBareUniqueIndexNotAConstraint(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table t (id int primary key, a text unique, b text, c text,
		constraint t_c_named unique (c))`)
	exec(t, d, `create unique index t_b_bare on t (b)`)
	exec(t, d, `alter table t add column e text`)
	exec(t, d, `alter table t add constraint t_e_added unique (e)`)

	want := [][]any{{"t_a_key"}, {"t_c_named"}, {"t_e_added"}}
	for name, q := range map[string]string{
		"pg_constraint":           `select conname from pg_constraint where conrelid = 't'::regclass and contype = 'u' order by 1`,
		"table_constraints":       `select constraint_name from information_schema.table_constraints where table_name = 't' and constraint_type = 'UNIQUE' order by 1`,
		"key_column_usage":        `select constraint_name from information_schema.key_column_usage where table_name = 't' and constraint_name <> 't_pkey' order by 1`,
		"constraint_column_usage": `select constraint_name from information_schema.constraint_column_usage where table_name = 't' and constraint_name <> 't_pkey' order by 1`,
	} {
		if got := exec(t, d, q).Rows; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	// The index itself is listed with the indexes, and still enforces.
	if got := exec(t, d, `select count(*) from pg_indexes where indexname = 't_b_bare'`).Rows; got[0][0] != int64(1) {
		t.Errorf("pg_indexes: %v", got)
	}
	exec(t, d, `insert into t values (1, 'x', 'y', 'z', 'w')`)
	wantErr(t, d, `insert into t values (2, 'x2', 'y', 'z2', 'w2')`, "duplicate key value violates unique constraint")
}

// TestUniqueConstraintNames: a CONSTRAINT name on UNIQUE is kept (it
// used to be dropped for table_cols_key), and ALTER TABLE ADD UNIQUE
// exists, named or not.
func TestUniqueConstraintNames(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table u (id int primary key, a int, b int, c int,
		constraint "uni_u_a" unique (a), unique (b))`)
	names := func() [][]any {
		t.Helper()
		return exec(t, d, `select constraint_name from information_schema.table_constraints
			where table_name = 'u' and constraint_type = 'UNIQUE' order by 1`).Rows
	}
	if got, want := names(), [][]any{{"u_b_key"}, {"uni_u_a"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CREATE TABLE: got %v, want %v", got, want)
	}

	// Unnamed ADD UNIQUE takes table_cols_key, numbered past a taken one.
	exec(t, d, `alter table u add unique (c)`)
	exec(t, d, `alter table u drop constraint u_c_key`)
	exec(t, d, `create index u_c_key on u (a)`) // takes the name the next ADD would use
	exec(t, d, `alter table u add unique (c)`)
	if got, want := names(), [][]any{{"u_b_key"}, {"u_c_key1"}, {"uni_u_a"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ADD UNIQUE naming: got %v, want %v", got, want)
	}

	// Existing duplicates refuse the constraint, and a taken name
	// conflicts.
	exec(t, d, `insert into u values (1, 1, 1, 1), (2, 2, 2, 2)`)
	exec(t, d, `alter table u add column dup int`)
	exec(t, d, `update u set dup = 7`)
	wantErr(t, d, `alter table u add constraint u_dup unique (dup)`, "existing rows violate unique index")
	wantErr(t, d, `alter table u add constraint uni_u_a unique (dup)`, `relation "uni_u_a" already exists`)
	wantErr(t, d, `alter table u add unique (nope)`, "not declared")
	// The constraint enforces, and drops by its name.
	wantErr(t, d, `insert into u values (3, 1, 3, 3, 0)`, "duplicate key value violates unique constraint")
	exec(t, d, `alter table u drop constraint uni_u_a`)
	exec(t, d, `insert into u values (3, 1, 3, 3, 0)`)
	if tag := describeTag(t, d, `alter table u add constraint k unique (id, a)`); tag != "ALTER TABLE" {
		t.Errorf("ADD UNIQUE command tag: %q", tag)
	}
}

// describeTag runs q and returns its command tag.
func describeTag(t *testing.T, d *DB, q string) string {
	t.Helper()
	st, err := Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	exec(t, d, q)
	return command(st)
}

// TestAlterIndexRename covers ALTER INDEX ... RENAME TO (GORM's
// Migrator.RenameIndex) and the index-name forms DROP INDEX shares.
func TestAlterIndexRename(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table r (id int primary key, a int, b int)`)
	exec(t, d, `create unique index r_a on r (a)`)
	exec(t, d, `create index r_b on r (b)`)
	exec(t, d, `insert into r values (1, 1, 1)`)

	exec(t, d, `alter index r_a rename to r_a2`)
	idx := func() [][]any {
		t.Helper()
		return exec(t, d, `select indexname from pg_indexes where tablename = 'r' order by 1`).Rows
	}
	if got, want := idx(), [][]any{{"r_a2"}, {"r_b"}, {"r_pkey"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after rename: %v", got)
	}
	// Renamed, not rebuilt: uniqueness and the IndexOnly kind carry over.
	wantErr(t, d, `insert into r values (2, 1, 2)`, "duplicate key value violates unique constraint")
	if ix := d.e.Table("r").Index("r_a2"); ix == nil || !ix.IndexOnly {
		t.Fatalf("renamed index: %+v", ix)
	}

	// Schema-qualified names (GORM and pg_dump write "public"."idx"),
	// the ON extension, and a same-name rename.
	exec(t, d, `alter index "public"."r_b" rename to r_b2`)
	exec(t, d, `alter index r_b2 on r rename to r_b`)
	exec(t, d, `alter index r_b rename to r_b`)

	if res := exec(t, d, `alter index if exists nope rename to x`); !strings.Contains(res.Notice, `relation "nope" does not exist, skipping`) {
		t.Errorf("IF EXISTS notice: %q", res.Notice)
	}
	wantErr(t, d, `alter index nope rename to x`, `relation "nope" does not exist`)
	wantErr(t, d, `alter index r_b rename to r_a2`, `relation "r_a2" already exists`)
	wantErr(t, d, `alter index r_b rename to r_pkey`, `relation "r_pkey" already exists`)
	wantErr(t, d, `alter index r_pkey rename to r_key2`, "a primary key index cannot be renamed")
	wantErr(t, d, `alter index other.r_b rename to x`, "no such schema")
	wantErr(t, d, `alter index r_b set tablespace fast`, `syntax error at or near "set"`)

	// A name on two tables needs ON.
	exec(t, d, `create table r2 (id int primary key, b int)`)
	exec(t, d, `create index r_b on r2 (b)`)
	wantErr(t, d, `alter index r_b rename to r_c`, "use ALTER INDEX name ON table")
	exec(t, d, `alter index r_b on r2 rename to r2_b`)

	// DROP INDEX takes the qualified form too, and a missing index has
	// Postgres's wording.
	exec(t, d, `drop index public.r2_b`)
	wantErr(t, d, `drop index r2_b`, `index "r2_b" does not exist`)
	if tag := describeTag(t, d, `alter index r_a2 rename to r_a`); tag != "ALTER INDEX" {
		t.Errorf("command tag: %q", tag)
	}
}

// TestLimitQuotedCount: a quoted LIMIT or OFFSET count is an unknown
// literal coerced to bigint, as in Postgres. pgx's simple protocol sends
// every bound argument that way.
func TestLimitQuotedCount(t *testing.T) {
	d := openDB(t)
	seedUsers(t, d)
	got := exec(t, d, `select id from users order by id limit '2' offset ' 1 '`).Rows
	if !reflect.DeepEqual(got, [][]any{{int64(2)}, {int64(3)}}) {
		t.Fatalf("quoted counts: %v", got)
	}
	wantErr(t, d, `select id from users limit 'two'`, "invalid input syntax for type bigint")
	wantErr(t, d, `select id from users limit '-1'`, "LIMIT must not be negative")
	wantErr(t, d, `select id from users offset '-2'`, "OFFSET must not be negative")
	wantErr(t, d, `select id from users offset '1.5'`, "invalid input syntax for type bigint")
}

// TestAnyOverInt2vector: pg_index.indkey is an int2vector ("1 2") and
// `attnum = ANY(indkey)` must expand it, as GORM's GetIndexes does.
func TestAnyOverInt2vector(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table v (id int primary key, a int, b int)`)
	exec(t, d, `create index v_ba on v (b, a)`)
	got := exec(t, d, `select a.attname from pg_index i
		join pg_class ci on ci.oid = i.indexrelid
		join pg_attribute a on a.attrelid = i.indrelid
		where ci.relname = 'v_ba' and a.attnum = any(i.indkey) order by a.attnum`).Rows
	if !reflect.DeepEqual(got, [][]any{{"a"}, {"b"}}) {
		t.Fatalf("attnum = ANY(indkey): %v", got)
	}
	for in, ok := range map[string]bool{"1 3": true, "  2  ": true, "": false, "{1,2}": false, "1 x": false, "-1": false} {
		if _, got := parseVectorText(in); got != ok {
			t.Errorf("parseVectorText(%q) = %v, want %v", in, got, ok)
		}
	}
	// An array literal still parses as one.
	if got := exec(t, d, `select 2 = any('{1,2}')`).Rows; got[0][0] != true {
		t.Errorf("array literal: %v", got)
	}
}

// TestInfoSchemaIdentityFamily: information_schema.columns has
// Postgres's identity columns. GORM's ColumnTypes selects
// identity_increment by name, so its absence failed every AutoMigrate of
// an existing table. A serial-style column reports them as a Postgres
// serial does: NULL, with identity_cycle NO.
func TestInfoSchemaIdentityFamily(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table idf (id bigserial primary key, g int generated by default as identity, n text)`)
	got := exec(t, d, `select column_name, is_identity, identity_start, identity_increment,
			identity_maximum, identity_minimum, identity_cycle, is_generated, generation_expression
		from information_schema.columns where table_name = 'idf' order by ordinal_position`).Rows
	want := [][]any{
		{"id", "NO", nil, nil, nil, nil, "NO", "NEVER", nil},
		{"g", "NO", nil, nil, nil, nil, "NO", "NEVER", nil},
		{"n", "NO", nil, nil, nil, nil, "NO", "NEVER", nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("identity family:\n got %v\nwant %v", got, want)
	}
}

// TestAlterTypeIdentityUsingSkipsRewrite: GORM's AutoMigrate re-sends
// `ALTER COLUMN c TYPE smallint USING c::smallint` on every run (bytdb
// stores smallint as int8, so the type never reads back as smallint). A
// USING that only casts the column to the type it already has is an
// identity, and must not rewrite the table each time. The log's growth
// shows whether rows were rewritten.
func TestAlterTypeIdentityUsingSkipsRewrite(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table g (id int primary key, age smallint, name varchar(64), bal numeric(10,2))`)
	exec(t, d, `create index g_age on g (age)`)
	var b strings.Builder
	b.WriteString(`insert into g values `)
	for i := range 2000 {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "(%d, %d, 'n%d', %d.25)", i, i%100, i, i)
	}
	exec(t, d, b.String())

	growth := func(q string) int64 {
		t.Helper()
		before := d.e.Stats().LogBytes
		exec(t, d, q)
		return d.e.Stats().LogBytes - before
	}
	for _, q := range []string{
		`ALTER TABLE "g" ALTER COLUMN "age" TYPE smallint USING "age"::smallint`,
		`ALTER TABLE g ALTER COLUMN age TYPE bigint USING age`,
		`ALTER TABLE g ALTER COLUMN age TYPE int USING g.age::int4`,
		`ALTER TABLE "g" ALTER COLUMN "name" TYPE varchar(64) USING "name"::varchar(64)`,
		`ALTER TABLE g ALTER COLUMN bal TYPE numeric(10,2) USING bal::numeric(10,2)`,
		`ALTER TABLE g ALTER COLUMN bal TYPE numeric(10,2) USING bal::numeric`,
	} {
		if n := growth(q); n > 1024 {
			t.Errorf("%s: log grew %d bytes; an identity USING should not rewrite", q, n)
		}
	}
	// Anything else still rewrites: a real expression, a cast to a
	// different modifier, or a type change.
	for _, q := range []string{
		`ALTER TABLE g ALTER COLUMN age TYPE smallint USING age + 0`,
		`ALTER TABLE g ALTER COLUMN bal TYPE numeric(10,2) USING bal::numeric(10,1)`,
		`ALTER TABLE g ALTER COLUMN name TYPE varchar(128) USING name::varchar(128)`,
	} {
		if n := growth(q); n < 20000 {
			t.Errorf("%s: log grew only %d bytes; expected a rewrite", q, n)
		}
	}
	// The rewrite with the coarser cast really applied it.
	if got := exec(t, d, `select bal::text from g where id = 3`).Rows; got[0][0] != "3.30" {
		t.Errorf("numeric(10,1) cast rewrite: %v", got)
	}
	// The no-op forms left the data and the index intact.
	if got := exec(t, d, `select count(*) from g where age = 42`).Rows; got[0][0] != int64(20) {
		t.Errorf("index after no-op alters: %v", got)
	}
}
