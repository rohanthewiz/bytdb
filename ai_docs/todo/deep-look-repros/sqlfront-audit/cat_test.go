package sqlfrontaudit

import "testing"

func TestCatalogProbes(t *testing.T) {
	d := openDB(t)
	must(t, d, `create table t (id int primary key, i int not null default 5, s varchar(20), sm smallint, r real, ts timestamp, a text[])`)
	must(t, d, `create table "Odd Name" (id serial primary key, x text)`)
	must(t, d, `create view vw as select id, s from t`)
	must(t, d, `alter table t add column dropped int`)
	must(t, d, `alter table t drop column dropped`)
	must(t, d, `alter table t add column late int`)
	probe(t, d,
		`select relname, relkind, relhasindex, relnatts from pg_class where relname = 't'`,
		`select relname, relkind, relhasindex from pg_class order by relname`,
		`select attname, attnum, atttypid, attnotnull, atthasdef, attisdropped, atttypmod from pg_attribute where attrelid = 't'::regclass order by attnum`,
		`select column_name, ordinal_position, data_type, udt_name, is_nullable, column_default, character_maximum_length, numeric_precision from information_schema.columns where table_name = 't' order by ordinal_position`,
		`select table_schema, table_name, table_type from information_schema.tables order by table_name`,
		`select tablename from pg_tables where schemaname = 'public'`,
		`select indexname, indexdef from pg_indexes where tablename = 't'`,
		`select viewname from pg_views`,
		`select schema_name from information_schema.schemata`,
		`select table_name from information_schema.views`,
		`select * from information_schema.referential_constraints`,
		`select * from information_schema.constraint_column_usage`,
		`select * from pg_description`, `select * from pg_enum`, `select * from pg_proc limit 1`, `select * from pg_depend limit 1`, `select name, setting from pg_settings limit 1`, `select * from pg_extension`, `select * from pg_user`, `select * from pg_range`, `select * from pg_trigger`, `select * from pg_tablespace`, `select * from pg_locks`, `select * from pg_stat_user_tables`, `select * from pg_matviews`, `select * from pg_sequences`, `select * from information_schema.routines`, `select * from information_schema.triggers`, `select * from information_schema.check_constraints`, `select * from information_schema.statistics`,
		`select typname, oid, typtype, typlen from pg_type order by oid`,
		`select format_type(a.atttypid, a.atttypmod) from pg_attribute a where a.attrelid = 't'::regclass and a.attnum > 0 order by a.attnum`,
		`select n.nspname, c.relname from pg_catalog.pg_class c left join pg_catalog.pg_namespace n on n.oid = c.relnamespace where c.relkind in ('r','p') and n.nspname not in ('pg_catalog','information_schema') order by 1, 2`,
		`select count(1) from information_schema.tables where table_schema = 'public' and table_name = 't' limit 1`,
		`select exists (select 1 from pg_tables where (current_schema() is null or schemaname = current_schema()) and tablename = 'goose_db_version')`,
		`select 'public.t'::regclass, '"Odd Name"'::regclass, 'Odd Name'::regclass, 'T'::regclass, 'pg_class'::regclass`,
		`select 't'::regclass::text, 100::regclass`,
		`select c.oid::regclass from pg_class c where relname = 't'`,
		`select pg_get_indexdef(indexrelid) from pg_index where indrelid = '"Odd Name"'::regclass`,
		`select adbin, pg_get_expr(adbin, adrelid) from pg_attrdef`,
		`select column_default from information_schema.columns where table_name = 'Odd Name' and column_name = 'id'`,
		`select pg_get_serial_sequence('"Odd Name"', 'id')`,
		`select last_value from pg_sequences`,
		`select * from information_schema.sequences`,
		`select conname, contype, conkey from pg_constraint order by conname`,
		`select current_database(), current_schema(), version()`,
		`select datname from pg_database`, `select rolname, rolsuper from pg_roles`,
	)
}
