package sqlfrontaudit

import (
	"strings"
	"testing"
)

func TestStmtProbes(t *testing.T) {
	d := openDB(t)
	must(t, d, `create table t (id int primary key, i int, s text, d date, ts timestamp)`)
	must(t, d, `insert into t values (1, 10, 'a', '2024-01-01', '2024-01-01 00:00:00'), (2, 20, 'b', null, null), (3, null, 'c', null, null)`)
	must(t, d, `create table u (id int primary key, tid int, v text)`)
	must(t, d, `insert into u values (1, 1, 'x'), (2, 1, 'y')`)
	probe(t, d,
		// --- lexing
		`select 'it''s', E'a\nb', e'\x41é\101', 'a\nb', U&'d\0061t'`,
		`select $$dollar$$`, `select $tag$it's$tag$`,
		`select 'a' 'b'`, "select 'a'\n'b'",
		`select 1 /* c /* nested */ still */ + 1 -- tail`,
		`select 1 /* unterminated`,
		`select 'unterminated`,
		`select 1.e2, .5, 1., 1e, 0x10, 1_000, 1e+, 12abc`,
		`select 1a`, `select 1.2.3`, `select 0x1F`, `select 1_000`,
		"select 1\f+\v1", "select 1", "select 1\x00",
		`select "id", "ID" from t`, `select ID, Id, "id" from t limit 1`,
		`create table "Mixed Case" ("Col A" int primary key, "select" text)`,
		`insert into "Mixed Case" values (1, 'x')`,
		`select "Col A", "select" from "Mixed Case"`, `select col a from "Mixed Case"`, `select * from mixed case`,
		`select "Mixed Case"."Col A" from "Mixed Case"`, `select m."Col A" from "Mixed Case" m`,
		`select "a""b" from (select 1 as "a""b") q`,
		`select 1 as "select", 2 as from_, 3 "as"`,
		`select 1 as année`, `select 1 as "année"`, `create table tablé (id int primary key)`,
		`select t.id from public.t`, `select public.t.id from public.t`, `select id from "public"."t"`, `select id from other.t`,
		`select 1 as desc`, `select 1 as order`, `select i value, s name, d date, ts timestamp from t limit 1`,
		`select id user, id type, id key, id end from t limit 1`,
		// --- statement forms
		`select * from t where i is true`, `select * from t where i is not distinct from null`,
		`select id from t order by i nulls first`, `select id from t order by i desc nulls last`,
		`select id from t order by i`, `select id from t order by i desc`,
		`select id from t limit all`, `select id from t fetch first 1 rows only`, `select id from t offset 1 rows`,
		`select id from t for update`, `select id from t for update skip locked`, `select id from t limit 1 for share`,
		`select id from t except select id from u`, `select id from t intersect select id from u`,
		`(select id from t) union (select id from u)`, `select id from t union all select id from u order by 1 limit 2`,
		`values (1, 'a'), (2, 'b')`, `table t`,
		`select t.id from t join u using (id)`, `select * from t natural join u`, `select t.id from t right join u on true`,
		`select t.id from t join u on u.tid = t.id where u.v = 'x'`,
		`select t.id from t, u where u.tid = t.id and u.v = 'y'`,
		`select x.id from t as x(id2) limit 1`,
		`insert into u select id + 10, id, s from t`, `insert into u (id, tid, v) select 99, 1, 'z'`,
		`insert into u values (50, 1, 'q') on conflict do nothing`,
		`insert into u as uu values (51, 1, 'q')`,
		`update t set i = default where id = 3`, `update t set (i, s) = (1, 'z') where id = 3`,
		`update t as x set i = 5 where x.id = 3`, `update t set i = u.id from u where u.tid = t.id`,
		`update only t set i = 5 where id = 3`, `update t set t.i = 5 where id = 3`,
		`delete from t as x where x.id = 99`, `delete from t using u where u.tid = t.id and u.id = 99`, `delete from only t where id = 99`,
		`with x as (select 1 as a) insert into u select 60, a, 'w' from x`,
		`with x as (select 1 as a) update t set i = 1 where id in (select a from x)`,
		`create temp table tmp (id int primary key)`, `create temporary table tmp (id int)`, `create unlogged table ul (id int primary key)`,
		`create table nopk (a int, b text)`, `insert into nopk values (1, 'x'), (1, 'x')`, `select * from nopk`,
		`create table ctas as select * from t`, `select * into t2 from t`,
		`create table dflt (id int primary key, a text default 'x'::text, b int default (0), c uuid default gen_random_uuid(), d timestamptz default now(), e bool default false not null, f int default -1, g text default '', h timestamp default current_timestamp, i jsonb default '{}'::jsonb, j text[] default '{}', k int default 1 + 1)`,
		`create table dflt2 (id int primary key, c uuid default gen_random_uuid())`,
		`create table dflt3 (id int primary key, a text default 'x'::text)`,
		`create table dflt4 (id int primary key, b int default (0))`,
		`create table dflt5 (id int primary key, n int default nextval('s'))`,
		`create table dflt6 (id uuid primary key default gen_random_uuid(), a varchar(10) default 'x'::character varying)`,
		`create table types1 (id bigserial primary key, a numeric(10,2), b decimal, c char(3), d time, e interval, f int[], g smallint, h real, i double precision, j timestamptz(6), k bigint not null default 0)`,
		`create table types2 (id integer generated always as identity primary key)`,
		`create table types3 (id int primary key, a numeric)`, `create table types4 (id int primary key, a int[])`, `create table types5 (id int primary key, a inet)`, `create table types6 (id int primary key, a char(3))`, `create table types7 (id int primary key, a citext)`,
		`create table types8 (id int primary key, a "text", b pg_catalog.int4, c public.foo)`,
		`create table fkt (id int primary key, tid int references t (id) on delete set null)`,
		`create table fkt2 (id int primary key, tid int references t on update cascade)`,
		`create table fkt3 (id int primary key, tid int references t deferrable initially deferred)`,
		`create index concurrently if not exists t_i on t (i)`, `create index t_i2 on t using btree (i)`, `create index on t (s)`, `create index t_low on t (lower(s))`, `create index t_part on t (i) where i > 0`, `create index t_inc on t (i) include (s)`,
		`create index t_i3 on t (i asc, s desc nulls last)`, `drop index concurrently t_i3`, `drop index if exists nope`, `drop index t_i2 cascade`,
		`create schema if not exists public`, `create schema app`, `create extension if not exists "uuid-ossp"`, `create extension pgcrypto`,
		`comment on table t is 'x'`, `comment on column t.i is 'x'`, `analyze`, `analyze t`, `discard all`, `deallocate all`, `lock table t in exclusive mode`,
		`grant all on t to public`, `create type mood as enum ('a','b')`, `create or replace function f() returns int as $$ select 1 $$ language sql`, `do $$ begin end $$`,
		`alter table t add column n int`, `alter table t add n2 int`, `alter table t add column if not exists n int, add column n3 int`,
		`alter table if exists t add column n4 int`, `alter table only t add column n5 int`, `alter table t alter column n type bigint`, `alter table t alter n set default 5`,
		`alter table t rename n to nn`, `alter table t rename column nn to n`, `alter table t drop column if exists zz`, `alter table t drop n2`, `alter table t drop column n cascade`,
		`alter table t add constraint t_i_pos check (i > 0) not valid`, `alter table t validate constraint t_i_pos`,
		`alter table t add constraint t_s_uniq unique (s)`, `alter table u add constraint u_tid_fk foreign key (tid) references t (id)`,
		`alter table t alter column s set not null`, `alter table t alter column s drop not null`,
		`alter table t enable row level security`, `alter table t set (fillfactor = 90)`, `alter table t rename to t_new`, `alter table t_new rename to t`,
		`alter index t_i rename to t_i_new`, `alter table t set schema public`,
		`drop table if exists a, b`, `drop table u cascade`, `drop table if exists nope cascade`, `drop view if exists vv cascade`,
		`truncate t restart identity cascade`, `truncate only t`,
		`explain analyze select 1`, `explain (format json) select 1`, `explain select * from t where id = 1`,
		`prepare p as select 1`, `execute p`, `copy t from stdin`, `listen ch`, `notify ch`, `set role postgres`, `reset role`, `set names 'utf8'`, `set client_encoding = 'UTF8'`, `set app.user_id = '5'`,
		`set session authorization default`, `set transaction read only`, `set session characteristics as transaction read only`, `set search_path = public`, `set search_path to app, public`, `set local lock_timeout = '5s'`,
		`show search_path`, `show lock_timeout`, `show application_name`, `show max_connections`, `show server_version_num`, `show lc_collate`, `show extra_float_digits`, `show "TimeZone"`, `show TimeZone`, `show is_superuser`, `show session_authorization`, `show default_transaction_read_only`, `show idle_in_transaction_session_timeout`, `show bytea_output`, `show intervalstyle`,
		`select current_setting('server_version_num')`, `select set_config('search_path', 'public', false)`, `select pg_backend_pid()`, `select current_user`, `select session_user`, `select user`, `select current_schema`, `select current_catalog`, `select current_database()`, `select current_schemas(true)`, `select pg_is_in_recovery()`, `select txid_current()`, `select pg_advisory_lock(1)`, `select pg_try_advisory_lock(1)`, `select pg_advisory_unlock(1)`, `select pg_advisory_xact_lock(1)`, `select has_table_privilege('t', 'select')`, `select to_regclass('t')`, `select to_regclass('public.t')`, `select 't'::regclass`, `select 'nope'::regclass`, `select quote_ident('a b')`, `select pg_postmaster_start_time()`, `select inet_server_addr()`, `select pg_typeof(1)`, `select exists(select 1)`, `select not exists(select 1 from t where id = 99)`,
	)
	// scalar function inventory: which common ones exist?
	fns := []string{
		`abs(-1)`, `round(1.5)`, `round(1.234, 2)`, `ceil(1.2)`, `ceiling(1.2)`, `floor(1.8)`, `trunc(1.8)`, `mod(7, 2)`, `power(2, 3)`, `sqrt(4)`, `sign(-2)`, `greatest(1, 2)`, `least(1, 2)`, `random()`, `exp(1)`, `ln(1)`, `log(10)`, `pi()`, `div(7,2)`,
		`substring('hello', 2, 3)`, `substring('hello' from 2 for 3)`, `substr('hello', 2)`, `left('hello', 2)`, `right('hello', 2)`, `trim('  a ')`, `trim(both 'x' from 'xax')`, `ltrim(' a')`, `rtrim('a ')`, `btrim(' a ')`, `replace('abc', 'b', 'x')`, `concat('a', 1, null)`, `concat_ws(',', 'a', 'b')`, `position('b' in 'abc')`, `strpos('abc', 'b')`, `split_part('a,b', ',', 2)`, `repeat('a', 3)`, `reverse('abc')`, `lpad('a', 3, '0')`, `rpad('a', 3)`, `initcap('a b')`, `starts_with('abc', 'a')`, `md5('a')`, `ascii('a')`, `chr(97)`, `octet_length('é')`, `bit_length('a')`, `format('%s-%s', 'a', 1)`, `translate('abc', 'a', 'x')`, `regexp_replace('abc', 'b', 'x')`, `regexp_match('abc', 'b')`, `to_hex(255)`, `encode('a'::bytea, 'hex')`, `quote_literal('a')`, `string_to_array('a,b', ',')`, `array_position(array['a'], 'a')`, `cardinality(array[1,2])`, `unnest(array[1,2])`, `array_agg(1)`, `string_agg('a', ',')`, `bool_and(true)`, `bool_or(true)`, `every(true)`, `generate_series(1, 3)`,
		`extract(epoch from now())`, `date_part('year', now())`, `date_trunc('month', now())`, `to_char(now(), 'YYYY-MM-DD')`, `to_timestamp(0)`, `to_date('2024-01-01', 'YYYY-MM-DD')`, `make_date(2024, 1, 1)`, `age(now(), now())`, `timezone('UTC', now())`, `now() at time zone 'UTC'`, `current_time`, `localtime`, `timeofday()`, `isfinite(now())`, `justify_days(null)`,
		`jsonb_build_object('a', 1)`, `json_build_object('a', 1)`, `jsonb_typeof('1'::jsonb)`, `jsonb_array_length('[1]'::jsonb)`, `jsonb_extract_path_text('{"a":1}'::jsonb, 'a')`, `to_json(1)`, `row_to_json(null)`, `jsonb_pretty('{}'::jsonb)`, `jsonb_strip_nulls('{}'::jsonb)`,
		`uuid_generate_v4()`, `gen_random_uuid()`, `uuidv7()`, `ifnull(null, 1)`, `nvl(null, 1)`, `isnull(null)`, `num_nulls(1, null)`, `count(*)`, `count(distinct 1)`, `sum(1)`, `avg(1)`, `min(1)`, `max(1)`, `stddev(1)`, `variance(1)`,
	}
	var have, miss []string
	for _, f := range fns {
		res, err := func() (r any, e error) {
			defer func() {
				if p := recover(); p != nil {
					t.Logf("PANIC in %s: %v", f, p)
				}
			}()
			return d.Exec("select " + f)
		}()
		_ = res
		if err != nil {
			miss = append(miss, f)
		} else {
			have = append(have, f)
		}
	}
	t.Logf("FUNCTIONS PRESENT: %s", strings.Join(have, " | "))
	t.Logf("FUNCTIONS MISSING: %s", strings.Join(miss, " | "))
}
