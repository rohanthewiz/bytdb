package sql

// Deep-look D-51 part 2: the catalog tables tools read (pg_tables,
// pg_indexes, pg_views, pg_settings, pg_sequences, the information_schema
// views), the catalog rows that misled (relhasindex, varchar typmods,
// serial defaults), and regclass in both directions.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func seedCatalogD51(t *testing.T, d *DB) {
	t.Helper()
	for _, q := range []string{
		`create table t (id serial primary key, name varchar(20), x float8, ts timestamp)`,
		`create table "MixedT" (id serial primary key)`,
		`create table c (id int primary key, tid int references t(id) on delete cascade)`,
		`create unique index c_tid on c (tid)`,
		`create view v as select id, name from t`,
		`create sequence s1`,
	} {
		exec(t, d, q)
	}
}

// rowsText renders a result's rows compactly for comparison.
func rowsText(r *Result) string {
	var b strings.Builder
	for i, row := range r.Rows {
		if i > 0 {
			b.WriteString(" | ")
		}
		for j, v := range row {
			if j > 0 {
				b.WriteByte(' ')
			}
			fmt.Fprint(&b, v)
		}
	}
	return b.String()
}

func TestCatalogTables(t *testing.T) {
	d := openDB(t)
	seedCatalogD51(t, d)
	for q, want := range map[string]string{
		`select tablename, hasindexes from pg_tables where schemaname = 'public' order by tablename`: "MixedT true | c true | t true",
		`select tablename, indexname, indexdef from pg_indexes where tablename in ('c', 'MixedT') order by indexname`: `MixedT MixedT_pkey CREATE UNIQUE INDEX "MixedT_pkey" ON public."MixedT" USING btree (id) | ` +
			`c c_pkey CREATE UNIQUE INDEX c_pkey ON public.c USING btree (id) | c c_tid CREATE UNIQUE INDEX c_tid ON public.c USING btree (tid)`,
		`select viewname, definition from pg_views`:                                                                           "v select id, name from t",
		`select sequencename, data_type, start_value, last_value from pg_sequences`:                                           "s1 bigint 1 <nil>",
		`select count(*) from pg_description`:                                                                                 "0",
		`select schema_name from information_schema.schemata order by schema_name`:                                            "information_schema | pg_catalog | public",
		`select table_name, view_definition, is_updatable from information_schema.views`:                                      "v select id, name from t NO",
		`select constraint_name, unique_constraint_name, delete_rule from information_schema.referential_constraints`:         "c_tid_fkey t_pkey CASCADE",
		`select table_name, column_name from information_schema.constraint_column_usage where constraint_name = 'c_tid_fkey'`: "t id",
		`select relname, relnatts, relhasindex from pg_class where relname in ('t', 't_pkey', 's1', 'v') order by relname`:    "s1 3 false | t 4 true | t_pkey 1 false | v 2 false",
	} {
		if got := rowsText(exec(t, d, q)); got != want {
			t.Errorf("%s\n got %s\nwant %s", q, got, want)
		}
	}
	// last_value appears once the sequence has been drawn.
	exec(t, d, `select nextval('s1')`)
	if got := rowsText(exec(t, d, `select last_value from pg_sequences`)); got != "1" {
		t.Errorf("last_value after nextval = %s", got)
	}
}

func TestCatalogColumnsAndTypmods(t *testing.T) {
	d := openDB(t)
	seedCatalogD51(t, d)
	q := `select column_name, data_type, udt_name, character_maximum_length, numeric_precision,
		numeric_scale, datetime_precision, is_identity, column_default
		from information_schema.columns where table_name = 't' order by ordinal_position`
	want := "id bigint int8 <nil> 64 0 <nil> NO nextval('t_id_seq'::regclass) | " +
		"name character varying varchar 20 <nil> <nil> <nil> NO <nil> | " +
		"x double precision float8 <nil> 53 <nil> <nil> NO <nil> | " +
		"ts timestamp with time zone timestamptz <nil> <nil> <nil> 6 NO <nil>"
	if got := rowsText(exec(t, d, q)); got != want {
		t.Errorf("columns\n got %s\nwant %s", got, want)
	}
	// A mixed-case table's serial default quotes the sequence name, as
	// regclass output does.
	if got := rowsText(exec(t, d, `select column_default from information_schema.columns where table_name = 'MixedT'`)); got != `nextval('"MixedT_id_seq"'::regclass)` {
		t.Errorf("MixedT default %s", got)
	}
	// varchar(n) reads as varchar with typmod n+4 in pg_attribute.
	if got := rowsText(exec(t, d, `select atttypid, atttypmod, format_type(atttypid, atttypmod) from pg_attribute
		where attrelid = 't'::regclass and attname = 'name'`)); got != "1043 24 character varying(20)" {
		t.Errorf("varchar attribute %s", got)
	}
}

func TestPgSettingsSession(t *testing.T) {
	d := openDB(t)
	s := d.NewSession()
	defer s.Close()
	sexec(t, s, `set app.tenant = 'acme'`)
	sexec(t, s, `set statement_timeout = '3s'`)
	got := rowsText(sexec(t, s, `select name, setting, source from pg_settings
		where name in ('app.tenant', 'statement_timeout', 'lc_collate') order by name`))
	if want := "app.tenant acme session | lc_collate C default | statement_timeout 3000ms default"; got != want {
		t.Errorf("pg_settings\n got %s\nwant %s", got, want)
	}
}

func TestRegclass(t *testing.T) {
	d := openDB(t)
	seedCatalogD51(t, d)
	// Name -> oid for every relation kind, quoted and qualified names.
	for _, name := range []string{`t`, `public.t`, `"MixedT"`, `public."MixedT"`, `c_tid`, `t_pkey`, `v`, `s1`, `pg_class`, `pg_catalog.pg_class`, `information_schema.columns`} {
		r := exec(t, d, fmt.Sprintf(`select '%s'::regclass::oid`, name))
		if _, ok := r.Rows[0][0].(int64); !ok {
			t.Errorf("%s::regclass = %v", name, r.Rows[0][0])
		}
	}
	if r := exec(t, d, `select 'pg_class'::regclass::oid`); r.Rows[0][0] != int64(1259) {
		t.Errorf("pg_class oid = %v, want Postgres's 1259", r.Rows[0][0])
	}
	for _, q := range []string{`select 'MixedT'::regclass`, `select 'nope'::regclass`} {
		if _, err := d.Exec(q); err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("%s: err %v", q, err)
		}
	}
	// Oid -> name at the top level and through ::text; comparisons stay
	// on oids.
	for q, want := range map[string]string{
		`select 't'::regclass, '"MixedT"'::regclass, 'c_tid'::regclass, 1259::regclass, 424242::regclass`: `t "MixedT" c_tid pg_class 424242`,
		`select oid::regclass from pg_class where relname = 'MixedT'`:                                     `"MixedT"`,
		`select conrelid::regclass::text, confrelid::regclass from pg_constraint where contype = 'f'`:     "c t",
		`select count(*) from pg_attribute where attrelid = 't'::regclass and attnum > 0`:                 "4",
		`select relname from pg_class where oid = 'v'::regclass`:                                          "v",
		`select to_regclass('"MixedT"') = '"MixedT"'::regclass`:                                           "true",
	} {
		if got := rowsText(exec(t, d, q)); got != want {
			t.Errorf("%s\n got %s\nwant %s", q, got, want)
		}
	}
	// The column keeps its name and Describe agrees with execution on
	// the type (text), so a prepared statement's RowDescription matches
	// its rows.
	r := exec(t, d, `select oid::regclass from pg_class where relname = 't'`)
	if r.Cols[0] != "oid" || r.Types[0] != bytdb.TString {
		t.Errorf("cols %v types %v", r.Cols, r.Types)
	}
	info := describe(t, d, `select oid::regclass, oid from pg_class`)
	if info.Types[0] != bytdb.TString || info.Types[1] != bytdb.TInt {
		t.Errorf("Describe types %v", info.Types)
	}
}
