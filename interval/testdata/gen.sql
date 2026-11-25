-- gen.sql: Postgres 16's answers for the interval golden tests
-- (golden_test.go). Run by gen.sh against a PG 16 server in a UTC
-- session; it prints one tab-separated case per line:
--
--   op <TAB> arg1 <TAB> arg2 <TAB> expected
--
-- expected is the result's text, or "ERROR <sqlstate>". Timestamps are
-- given and returned as microseconds since 1970 (bytdb's form), or the
-- words infinity / -infinity.

set client_min_messages = warning;
set timezone = 'UTC';
set intervalstyle = 'postgres';

create or replace function pg_temp.try(q text) returns text language plpgsql as $$
declare r text;
begin
  execute q into r;
  return coalesce(r, 'NULL');
exception when others then
  return 'ERROR ' || sqlstate;
end $$;

-- A timestamp as bytdb's microsecond count.
create or replace function pg_temp.us(t timestamptz) returns text language sql as $$
  select case when isfinite(t) then (extract(epoch from t) * 1000000)::bigint::text else t::text end
$$;

create temp table inputs (s text);
insert into inputs values
  ('0'), ('1'), ('-1'), ('1.5'), ('.5'), ('1.'), ('5'), ('+5'), ('-.5'),
  ('1 day'), ('1 days'), ('-1 day'), ('2 days'), ('1 d'), ('1day'),
  ('1 hour'), ('1 h'), ('1 hr'), ('2 hrs'), ('90 minutes'), ('1 min'), ('1 m'),
  ('1 second'), ('1 s'), ('1 sec'), ('30 secs'), ('1 ms'), ('2 msec'),
  ('1 millisecond'), ('2 milliseconds'), ('2 millisecondsxx'), ('3 us'),
  ('3 usec'), ('1 microsecond'), ('1 microseconds'), ('1 week'), ('1 w'),
  ('2 weeks'), ('1 month'), ('1 mon'), ('2 mons'), ('1 months'), ('1 year'),
  ('1 y'), ('1 yr'), ('2 yrs'), ('1 years'), ('1 decade'), ('1 dec'),
  ('2 decades'), ('1 century'), ('1 c'), ('2 centuries'), ('1 millennium'),
  ('1 mil'), ('2 millennia'), ('1 quarter'), ('1 qtr'), ('1 timezone'),
  ('1 year 2 mons 3 days 04:05:06.789'), ('1 year 2 months 3 days 4 hours 5 minutes 6 seconds'),
  ('-1 day +02:00:00'), ('1 day -02:00:00'), ('-1 days -02:00:00'),
  ('@ 1 day'), ('@ 1 day ago'), ('1 day ago'), ('1 day ago ago'),
  ('1 ago'), ('1 ago day'), ('ago'), ('day'), ('1 day day'), ('1 days 1 days'),
  ('01:02'), ('01:02:03'), ('1:2:3.456'), ('1:2:3.4567891'), ('100:00:00'),
  ('-01:30'), ('+01:30'), ('-1:30'), ('1:30.5'), ('1:60'), ('1:59:60'),
  ('1:59:61'), ('1:2:3:4'), ('25:00'), ('1 01:00'), ('3 4:05:06'),
  ('1 2 hours'), ('1 hour 01:00'), ('01:00 1.5 days'), ('01:00 1 day'),
  ('1-2'), ('-1-2'), ('1-12'), ('1-'), ('1-x'), ('1-2 3'), ('1-2 3 4:05:06'),
  ('1/2'), ('1.2.3'), ('1.5 mon'), ('1.5 months'), ('1.5 years'), ('0.1 years'),
  ('0.01 years'), ('1.5 weeks'), ('-1.5 weeks'), ('1.5 days'), ('0.5 day'),
  ('1.5 hours'), ('0.25 minutes'), ('1.5 seconds'), ('0.000001 seconds'),
  ('0.0000005 seconds'), ('0.0000015 seconds'), ('1.5 decades'), ('0.5 millennium'),
  ('1.123456789 days'), ('-1.123456789 days'), ('1.999999 months'),
  ('2147483647 days'), ('2147483648 days'), ('-2147483648 days'),
  ('2147483647 months'), ('2147483648 months'), ('178956970 years'),
  ('178956971 years'), ('-178956970 years 8 months'), ('9223372036854775807 us'),
  ('9223372036854775808 us'), ('2562047788 hours'), ('2562047789 hours'),
  ('153722867280 minutes'), ('99999999999999999999'), ('1e3'), ('1 epoch'),
  ('now'), ('infinity'), ('-infinity'), ('today'), ('1 day at'), ('1 dow'),
  ('5 mm'), (''), ('   '), (' 1 day '), ('1 DAY'), ('1 Day 2 HOURS'),
  ('1,day'), ('1;day'), ('1 day, 2 hours'), ('1 day 2 hours ago'),
  ('-1 day 2 hours ago'), ('1 mon -1 day'), ('-1 mon +1 day -00:00:01'),
  ('1 day -00:00:01'), ('-1 day -00:00:01'), ('- 1 day'), ('+ 1 day'),
  ('-day'), ('+'), ('-'), ('1 ×'), ('P1D'), ('P1Y2M3DT4H5M6S'), (' P1D'),
  ('p1d'), ('P1.5Y'), ('P1.5M'), ('P1.5W'), ('P1.5D'), ('PT1.5H'), ('PT1.5M'),
  ('PT1.5S'), ('P0001-02-03T04:05:06'), ('P00010203T040506'), ('P0001-02-03'),
  ('P0001-02'), ('P0001'), ('PT04:05:06'), ('PT040506'), ('PT04:05'),
  ('P1Y-2M'), ('P-1Y2M'), ('P1YT'), ('PT'), ('P'), ('P1'), ('P1X'),
  ('PT1D'), ('P1DT1H1M1.5S'), ('P1e2D'), ('P-1.5D'), ('P.5D'),
  ('P1000000000000000D'), ('P10000000000000000D'), ('Pinf'), ('P-infD'),
  ('P0001-02-03T04:05:06.5'), ('P1Y2M3W4DT5H6M7S'), ('P20200101'),
  ('P2020-01-01T00:00:00'), ('PT1H30M'), ('P1Y2M3DT4H5M6.789S'),
  ('1 year 2 months 3 weeks 4 days'), ('3 weeks 4 days'), ('1 week 2 days'),
  ('1.5 weeks 2 days'), ('10000000 hours'), ('-10000000 hours'),
  ('1 century 2 decades 3 years'), ('7 days 24:00:00'), ('48:00:00'),
  ('2 hours 30'), ('30 2 hours'), ('1 hour 30 minutes 15 seconds 500 ms'),
  ('1.5 seconds 500 ms'), ('1 second 500 ms'), ('500 ms 250 us');

create temp table typmods (m text);
insert into typmods values (''), ('year'), ('month'), ('day'), ('hour'),
  ('minute'), ('second'), ('year to month'), ('day to hour'),
  ('day to minute'), ('day to second'), ('hour to minute'),
  ('hour to second'), ('minute to second'), ('(0)'), ('(2)'), ('(6)'),
  ('second(3)'), ('day to second(1)'), ('minute to second(0)');

create temp table tminputs (s text);
insert into tminputs values ('5'), ('1.5'), ('-1.5'), ('1-2'), ('1 2'),
  ('1:30'), ('1:30.7'), ('1 day 02:03:04.5678'), ('-1 day -02:03:04.5678'),
  ('1 year 2 months 3 days 04:05:06.789'), ('25:59:59.999999'),
  ('3 4:05'), ('0.5'), ('99'), ('1 mon'), ('00:00:00.5'), ('-00:00:00.5'),
  ('00:00:00.15'), ('00:00:00.25'), ('-00:00:00.25'), ('1 week');

create temp table ivs (v text);
insert into ivs values ('0'), ('1 day'), ('-1 day'), ('1 mon'), ('30 days'),
  ('720 hours'), ('1 year'), ('1 mon 1 day 01:00:00'), ('-1 mon +1 day'),
  ('1 day -01:00:00'), ('36:00:00'), ('-36:00:00'), ('1 mon -1 day'),
  ('00:00:00.000001'), ('-00:00:00.000001'), ('1 year 2 mons 3 days 04:05:06.789'),
  ('-1 year -2 mons -3 days -04:05:06.789'), ('29 days 23:59:59.999999'),
  ('59 days'), ('-59 days 12:00:00'), ('2 mons -1 day 23:00:00'),
  ('178956970 years 7 mons'), ('-178956970 years -8 mons'),
  ('2147483647 days'), ('-2147483648 days'), ('2562047788:00:54.775807'),
  ('-2562047788:00:54.775808'), ('1 mon 15 days'), ('15 days 12:00:00'),
  ('3 days 01:02:03'), ('1 year 6 mons');

create temp table factors (f text);
insert into factors values ('0'), ('1'), ('-1'), ('2'), ('0.5'), ('1.5'),
  ('-1.5'), ('3'), ('7'), ('0.1'), ('1e-7'), ('1e9'), ('2147483647'),
  ('0.333333333333'), ('1.0000001'), ('nan'), ('infinity'), ('-infinity'),
  ('86400'), ('0.04166666666666666');

create temp table tss (t text);
insert into tss values ('2024-01-31 00:00:00'), ('2024-02-29 12:34:56.789'),
  ('2023-01-31 23:59:59.999999'), ('1970-01-01'), ('1969-12-31 23:00:00'),
  ('2000-03-31 10:00'), ('1900-02-28'), ('2100-12-31 23:59:59'),
  ('0001-01-01'), ('0001-01-01 BC'), ('4713-11-25 BC'), ('4714-11-24 BC'),
  ('infinity'), ('-infinity'), ('294000-01-01');

create temp table units (u text);
insert into units values ('microseconds'), ('microsecond'), ('us'),
  ('milliseconds'), ('ms'), ('second'), ('seconds'), ('s'), ('minute'),
  ('min'), ('hour'), ('hours'), ('h'), ('day'), ('days'), ('week'),
  ('month'), ('mon'), ('quarter'), ('year'), ('years'), ('decade'),
  ('century'), ('millennium'), ('epoch'), ('dow'), ('doy'), ('isodow'),
  ('timezone'), ('julian'), ('bogus'), ('DAY'), ('Hours');

-- Input, output and type modifiers.
select 'in', s, '', pg_temp.try(format('select %L::interval::text', s)) from inputs;
select 'in', s, m, pg_temp.try(format('select %L::interval %s::text', s, m))
  from tminputs, typmods where m <> '';

-- Binary interval ops and comparison.
select op, a.v, b.v, pg_temp.try(format('select (%L::interval %s %L::interval)::text', a.v, sym, b.v))
  from ivs a, ivs b, (values ('add', '+'), ('sub', '-')) o(op, sym);
select 'cmp', a.v, b.v, pg_temp.try(format('select interval_cmp(%L::interval, %L::interval)::text', a.v, b.v))
  from ivs a, ivs b;

-- Scaling.
select op, v, f, pg_temp.try(format('select (%L::interval %s %L::float8)::text', v, sym, f))
  from ivs, factors, (values ('mul', '*'), ('div', '/')) o(op, sym);

-- Unary.
select op, v, '', pg_temp.try(format('select (%s(%L::interval))::text', fn, v))
  from ivs, (values ('neg', '-'), ('jh', 'justify_hours'), ('jd', 'justify_days'),
             ('ji', 'justify_interval')) o(op, fn);

-- Timestamps meet intervals.
select 'tsadd', pg_temp.us(t::timestamptz), v,
  pg_temp.try(format('select pg_temp.us(%L::timestamptz + %L::interval)', t, v)) from tss, ivs;
select 'tssub', pg_temp.us(t::timestamptz), v,
  pg_temp.try(format('select pg_temp.us(%L::timestamptz - %L::interval)', t, v)) from tss, ivs;
select 'tsmi', pg_temp.us(a.t::timestamptz), pg_temp.us(b.t::timestamptz),
  pg_temp.try(format('select (%L::timestamptz - %L::timestamptz)::text', a.t, b.t)) from tss a, tss b;
select 'age', pg_temp.us(a.t::timestamptz), pg_temp.us(b.t::timestamptz),
  pg_temp.try(format('select age(%L::timestamptz, %L::timestamptz)::text', a.t, b.t)) from tss a, tss b;

-- extract / date_part / date_trunc.
select 'extract', u, v, pg_temp.try(format('select extract(%s from %L::interval)::text', quote_ident(u), v))
  from units, ivs;
select 'part', u, v, pg_temp.try(format('select date_part(%L, %L::interval)::text', u, v)) from units, ivs;
select 'trunc', u, v, pg_temp.try(format('select date_trunc(%L, %L::interval)::text', u, v)) from units, ivs;

-- make_interval(years, months, weeks, days, hours, mins, secs).
select 'make', a, '', pg_temp.try(format('select make_interval(%s)::text', a)) from (values
  ('0,0,0,0,0,0,0'), ('1,2,3,4,5,6,7.5'), ('-1,-2,-3,-4,-5,-6,-7.5'),
  ('0,0,0,0,0,0,1.0000005'), ('0,0,0,0,0,0,1.0000015'), ('178956970,7,0,0,0,0,0'),
  ('178956971,0,0,0,0,0,0'), ('0,0,306783378,1,0,0,0'), ('0,0,0,0,2147483647,0,0'),
  ('0,0,0,0,0,0,1e300'), ('0,0,0,0,0,0,''nan'''), ('0,0,0,0,0,0,''infinity'''),
  ('0,0,0,0,0,0,9223372036854.775807'), ('0,0,0,0,0,0,-0.0000005')) t(a);
