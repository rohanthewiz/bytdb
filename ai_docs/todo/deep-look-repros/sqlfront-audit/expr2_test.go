package sqlfrontaudit

import "testing"

func TestExprProbes2(t *testing.T) {
	d := openDB(t)
	must(t, d, `create table t (id int primary key, i int, f float, s text, b bool, ts timestamp, d date, u uuid, j jsonb, by bytea)`)
	must(t, d, `insert into t values (1, 10, 1.5, 'héllo', true, '2024-02-29 12:00:00', '2024-02-29', 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '{"a":1}', '\x0102')`)
	must(t, d, `insert into t values (2, null, null, null, null, null, null, null, null, null)`)
	must(t, d, `create index t_ts on t (ts)`)
	probe(t, d,
		`select coalesce(1, 1/0)`,
		`select case when true then 1 else 1/0 end`,
		`select 7 / 2, -7 / 2, 7 % -2, -7 % 2, 7.0 / 2`,
		`select 5.5 % 2`, `select 1.0 / 0`,
		`select -1::int`, `select -1::text`, `select - 1 :: bigint`, `select -i::int from t`, `select (-1)::int`,
		`select 1 - -1`, `select 1 - - 1`, `select 1+-+1`, `select - - 1`, `select -(-1)`, `select +1`,
		`select '12'::int, ' 12 '::int`, `select '1.5'::int`, `select 1.5::int, 2.5::int, 3.5::int, (-2.5)::int`,
		`select 'nan'::float, 'inf'::float`,
		`select true::int`, `select 1::bool, 0::bool`, `select 2::bool`, `select 'yes'::bool`, `select 't'::bool, 'TRUE'::bool`, `select 'on'::bool`,
		`select '2024-02-29'::date`, `select '2023-02-29'::date`, `select '2024-13-01'::date`, `select '2024-2-9'::date`, `select '20240229'::date`, `select '02/29/2024'::date`,
		`select '0001-01-01'::date`, `select '9999-12-31'::date`, `select '294277-01-01'::timestamp`, `select '300000-01-01'::timestamp`, `select '0000-01-01'::date`, `select '10000-01-01'::date`,
		`select 'epoch'::timestamp`, `select 'infinity'::timestamp`, `select 'today'::date`, `select 'now'::timestamp`,
		`select '2024-02-29 12:00'::timestamp`, `select '2024-02-29'::timestamp`, `select '2024-02-29 12:00:00 UTC'::timestamp`, `select '2024-02-29 12:00:00 America/New_York'::timestamp`, `select '2024-02-29 12:00:00-05'::timestamp`, `select '2024-02-29 24:00:00'::timestamp`, `select '2024-02-29 12:00:60'::timestamp`,
		`select 'A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11'::uuid`, `select '{a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11}'::uuid`, `select 'a0eebc999c0b4ef8bb6d6bb9bd380a11'::uuid`,
		`select 1::numeric(10,2)`, `select 'x'::varchar(10)`, `select 'x'::character varying`, `select now()::timestamp without time zone`, `select now()::timestamp(3)`, `select 1::pg_catalog.int4`,
		`select '0x10'::int`, `select '1_000'::int`, `select '+5'::int`, `select '0x1p-2'::float`, `select '1_0'::float`,
		// date/time semantics
		`select id from t where ts::date = '2024-02-29'`,
		`select id from t where ts >= '2024-02-29' and ts < '2024-03-01'`,
		`select id from t where d = '2024-02-29'`,
		`select id from t where ts in ('2024-02-29 12:00:00')`,
		`select id from t where d in ('2024-02-29', '2024-03-01')`,
		`select id from t where u in ('a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11')`,
		`select id from t where u = 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'`,
		`select id from t where u = any(array['a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'])`,
		`select id from t where b in ('true')`,
		`select id from t where by in ('\x0102')`,
		`select id from t where j in ('{"a": 1}')`,
		`select id from t where coalesce(ts, '2000-01-01') > '2024-01-01'`,
		`select id from t where (case when id = 1 then d end) = '2024-02-29'`,
		`select id from t where i in ('10')`,
		`select id from t where f in ('1.5')`,
		`select d + 1, d - 1, ts + 1, d - d, ts - ts from t where id = 1`,
		`select d > ts, d = ts, ts > d from t where id = 1`,
		`select d < '2024-03-01 00:00:00', ts > '2024-02-29' from t where id = 1`,
		`select now() > '2020-01-01', current_date > '2020-01-01', now() - now()`,
		`select current_timestamp, current_date, localtimestamp, now()`,
		`select current_time`, `select current_user, session_user, current_schema, current_database(), current_schema(), version()`,
		`select now() + interval '1 day'`, `select extract(year from now())`, `select date_trunc('day', now())`, `select to_char(now(), 'YYYY')`, `select age(now())`,
		`select i + f, i * 1.0, i / 3, i / 3.0, s || i, b and true, not b from t`,
		`select i + s from t where id = 1`, `select s + 1 from t where id = 1`, `select b + 1 from t where id = 1`, `select 'a' + 'b'`, `select '1' + 1`, `select '1' + '2'`,
		`select 1 = '1', '1' = 1, 1 = 'x'`, `select 1 < 'a'`, `select 'a' < 1`, `select true = 'true', true = 1, 't' = true`, `select 1 = true`,
		`select 1.0 = 1, 1 = 1.0, 'a' = 'a ', 'a' < 'B', 'B' < 'a'`,
		`select j -> 'a', j ->> 'a', j || '{"b":2}', j - 'a' from t where id = 1`,
		`select id from t where s = 'héllo' collate "C"`,
		`select 1 < 2 < 3`, `select 1 = 1 = true`, `select 1 between 0 and 2 and true`, `select not 1 = 2`, `select not true and false, not (true and false)`,
		`select 1 + 2 = 3 and 2 * 2 = 4 or false`, `select 2 || 3 + 4`, `select 'a' || 'b' = 'ab'`, `select 1 in (1) = true`, `select 1 + 1 is null`,
		`select 5 not between 1 and 3, 5 between symmetric 9 and 1, 2 between 3 and 1`,
		`select 1 is null is null`, `select 1 <> all(array[2,3]), 1 = any('{1,2}'), 1 = any(array[]::int[])`, `select null = any(array[1]), 1 = any(array[null]), 1 <> all(array[null])`,
		`select 'x' = any(array['x','y']), 2 > all(array[1,1]), 1 = any(array[1.0])`,
		`select array[1,2,3], array['a','b'], array[]`, `select (array[1,2])[1]`,
	)
}
