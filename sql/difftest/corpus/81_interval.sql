-- interval (N-053): columns and type modifiers, input and output,
-- comparison by span ('1 mon' = '30 days'), keys, arithmetic with
-- timestamps and dates, aggregates, functions, casts and errors.
-- Timestamps are timestamptz (bytdb's one timestamp type) in a UTC
-- session, so both sides render them alike.

-- @setup
create table iv (id bigint primary key, v interval, h interval hour, s interval day to second(1), y interval year to month);
insert into iv values
  (1, '1 year 2 mons 3 days 04:05:06.789', '1 day 02:30:00', '1 day 02:03:04.56', '1 year 2 mons 3 days'),
  (2, '-1 day +02:00:00', '-90 minutes', '-00:00:00.25', '-1 years -2 mons'),
  (3, '1 mon', '5', '5', '5'),
  (4, '30 days', null, null, null),
  (5, '36:00:00', '@ 3 hours ago', 'P1DT2H', 'P1Y2M'),
  (6, null, null, null, null),
  (7, '00:00:00.000001', '0', '0', '0');
create table ik (k interval primary key, n int unique);
insert into ik values ('1 mon', 1), ('-1 day', 2), ('36:00:00', 3), ('1 year', 4), ('00:00:00.000001', 5), ('29 days 23:00:00', 6);
create index ik_n on ik (n);
create table ts (id int primary key, t timestamptz, d date);
insert into ts values
  (1, '2024-01-31 00:00:00+00', '2024-01-31'),
  (2, '2024-02-29 12:34:56.789+00', '2024-02-29'),
  (3, '2023-12-31 23:59:59.999999+00', '2023-12-31'),
  (4, '1969-12-31 23:00:00+00', '1969-12-31'),
  (5, 'infinity', 'infinity');

-- @queries
-- @ordered
select id, v, h, s, y from iv order by id;
-- @ordered
select k, n from ik order by k;

-- Input forms and output.
select interval '1 day', interval '-1 day', interval '1.5 hours', interval '90 minutes', interval '1-2', interval '-1-2', interval '3 4:05:06';
select '@ 1 day 2 hours ago'::interval, 'P1Y2M3DT4H5M6.789S'::interval, 'P0001-02-03T04:05:06'::interval, 'PT1.5H'::interval, '1.5 mon'::interval;
select '1 year 2 months 3 weeks 4 days'::interval, '2 millisecondsxx'::interval, '1 day 2 hours ago'::interval, '0.5 day'::interval, '1:30.5'::interval;
select interval '0', interval '00:00:00', interval '1 mon -1 day', interval '-1 mon +1 day -00:00:01', interval '100:00:00';
-- Type modifiers.
select interval '5' minute, interval '1 day 02:30:45.678' hour, interval '1 day 02:30:45.678' day to minute, interval '1 day 02:30:45.678' second(1), interval(2) '00:00:01.555';
select '1:30'::interval minute to second, '1 year 7 mons 3 days'::interval year, '1 year 7 mons 3 days'::interval month, '-00:00:00.25'::interval(1), '00:00:00.15'::interval(1);

-- Comparison is by span: a month is 30 days, a day 24 hours.
select interval '1 mon' = interval '30 days', interval '1 day' = interval '24 hours', interval '1 mon' < interval '31 days', interval '1 year' > interval '360 days';
-- @ordered
select id from iv where v = '30 days' order by id;
-- @ordered
select id from iv where v > '1 day' order by id;
-- @ordered
select id, v from iv where v is not null order by v, id;
-- @ordered
select id, v from iv where v is not null order by v desc, id;
select count(distinct v) from iv;
-- @ordered
select n from ik where k = '720 hours' order by n;
-- @ordered
select n from ik where k between '1 day' and '1 mon' order by n;
-- @ordered
select n from ik where k in ('30 days', '1 day 12:00:00', '-24 hours') order by n;

-- Arithmetic.
select interval '1 day' + interval '1 hour', interval '1 day' - interval '1 hour', interval '1 mon' + interval '-1 day', -interval '1 day 02:00', -(interval '-1 mon');
select interval '1 day' * 2, interval '1 day' * 1.5, 1.5 * interval '1 mon', interval '1 mon' / 7, interval '1 day' / 0.5, interval '1 day' * '2', interval '1 day' + '1 hour';
-- @ordered
select id, v + interval '1 day', v * 2, v / 4, -v from iv where v is not null order by id;

-- Timestamps and dates meet intervals on the calendar.
-- @ordered
select id, t + interval '1 mon', t - interval '1 mon', t + interval '1 day 01:00', t - interval '36 hours', interval '1 year' + t from ts order by id;
-- date ± interval is a timestamp without time zone in Postgres; bytdb has
-- one timestamp type (timestamptz), so the results are cast to it here.
-- @ordered
select id, (d + interval '1 mon')::timestamptz, (d - interval '1 hour')::timestamptz, (interval '1 day' + d)::timestamptz from ts order by id;
select timestamptz '2024-01-31 10:00+00' + interval '1 mon 1 day', timestamptz '2024-03-31 00:00+00' - interval '1 mon', timestamptz '2023-02-28 00:00+00' + interval '1 year';
select timestamptz '2024-03-01 00:00+00' - timestamptz '2024-01-01 12:00+00', timestamptz '2024-01-01 00:00+00' - timestamptz '2024-03-01 00:00+00', timestamptz '2024-01-01 00:00+00' - '2023-12-31 22:30+00';
-- @ordered
select a.id, b.id, a.t - b.t from ts a, ts b where a.id < 5 and b.id < 5 order by a.id, b.id;
select timestamptz '2024-01-01 00:00+00' + '36 hours';

-- Aggregates. Equal spans tie in min and max; the later value wins in
-- Postgres, and these rows are in id order on both sides.
select sum(v), avg(v), min(v), max(v) from iv;
select sum(v), avg(v) from iv where id in (1, 2, 5);
-- @ordered
select n % 2, sum(k), avg(k), min(k), max(k) from ik group by n % 2 order by 1;
-- @ordered
select v, count(*) from iv where id in (2, 5, 7) group by v order by v;

-- Functions.
select justify_hours(interval '36:00:00'), justify_hours(interval '-1 day +36:00'), justify_days(interval '35 days'), justify_days(interval '-1 mon +35 days'), justify_interval(interval '1 mon -1 hour');
select make_interval(1, 2, 3, 4, 5, 6, 7.5), make_interval(days => 10), make_interval(hours => 1, secs => 1.5), make_interval(), make_interval(0, 0, 0, 0, 0, 0, 0.0000005);
select date_part('hour', interval '1 day 02:30'), date_part('epoch', interval '1 year 1 day'), date_part('month', interval '14 mons'), date_part('second', interval '00:00:01.25');
select extract(day from interval '40 days'), extract(minute from interval '01:30:00'), extract(year from interval '27 mons'), extract(epoch from interval '1 mon');
select date_trunc('hour', interval '1 day 02:30:45'), date_trunc('day', interval '1 mon 2 days 03:04'), date_trunc('year', interval '27 mons 3 days'), date_trunc('quarter', interval '5 mons 1 day');
select age(timestamptz '2024-03-31 00:00+00', timestamptz '2024-02-29 00:00+00'), age(timestamptz '2024-01-01 00:00+00', timestamptz '2024-03-15 12:00+00'), age(timestamptz '2000-02-29 00:00+00', timestamptz '1999-03-31 23:59:59.5+00');
select isfinite(interval '1 day');
-- @ordered
select id, justify_interval(v), date_part('day', v), date_trunc('hour', v) from iv where v is not null order by id;

-- Window frames measured in intervals: RANGE offsets over a timestamp
-- or interval ORDER BY key.
-- @ordered
select id, count(*) over (order by t range between interval '1 mon' preceding and current row), sum(id) over (order by t range between '12 hours' preceding and '1 day' following) from ts order by id;
-- @ordered
select id, count(*) over (order by t desc range between interval '40 days' preceding and interval '1 hour' following) from ts order by id;
-- @ordered
select id, count(*) over (order by v range between interval '1 day' preceding and interval '1 day' following) from iv where v is not null order by id;
-- @error 22013
select count(*) over (order by t range between interval '-1 day' preceding and current row) from ts;

-- @ordered
select id, t - timestamptz '2024-01-01 00:00+00', count(*) over () from ts where id < 5 order by id;

-- Casts.
select interval '1 day 02:00'::text, '1 day'::interval::text, cast('2 weeks' as interval), ('1 day'::interval)::interval hour;
select interval '1 day' || ' later';

-- Catalog.
-- @ordered
select a.attname::text, format_type(a.atttypid, a.atttypmod) from pg_attribute a join pg_class c on c.oid = a.attrelid where c.relname = 'iv' and a.attnum > 0 order by a.attnum;
-- @ordered
select column_name::text, data_type::text, datetime_precision, interval_type::text, interval_precision from information_schema.columns where table_name = 'iv' order by ordinal_position;

-- Errors.
-- @error 22007
select interval '1 fortnight';
-- @error 22007
select interval 'P1D2';
-- @error 22015
select interval '1:60';
-- @error 22015
select interval '1-12';
-- @error 22008
select interval '178956970 years' * 2;
-- @error 22012
select interval '1 day' / 0;
-- @error 42883
select interval '1 day' + 1;
-- @error 42883
select interval '1 day' * interval '1 day';
-- @error 22008
select timestamptz 'infinity' - timestamptz '2024-01-01 00:00+00';
-- @error 0A000
select date_part('dow', interval '1 day');
-- @error 22023
select date_trunc('epoch', interval '1 day');
-- @error 23505
insert into ik values ('720 hours', 99);
-- @error 22007
insert into iv (id, v) values (99, 'soon');
