-- numeric NaN and ±Infinity (N-051). Postgres 14+ has them; bytdb
-- refused them (0A000) until N-051. Both sides type these columns
-- numeric, so values compare as exact text.

-- @setup
create table s (id int primary key, x numeric, y numeric(5,2), f float8);
create index s_x on s (x);
insert into s values
  (1, 'NaN', 'NaN', 'NaN'),
  (2, 'Infinity', 1.5, 'Infinity'),
  (3, '-Infinity', 2, '-Infinity'),
  (4, 1.25, null, 1.25),
  (5, 'nan', null, null),
  (6, 0, 3, 0);
create table u (x numeric primary key, d numeric default 'NaN');
insert into u (x) values ('NaN'), ('+inf'), ('-infinity'), (5), (-5.50);

-- @queries
-- @ordered
select id, x, y, f from s order by id;
-- @ordered
select x, d from u order by x;

-- Input spellings, output, typmod.
select 'NaN'::numeric, ' nan '::numeric, 'Infinity'::numeric, '-inf'::numeric, '+INF'::numeric, 'NaN'::numeric(5,2);

-- Arithmetic.
select 'NaN'::numeric + 1, 'Infinity'::numeric + 1, 'Infinity'::numeric + '-Infinity'::numeric, 'Infinity'::numeric - 'Infinity'::numeric, 1 - 'Infinity'::numeric;
select 'Infinity'::numeric * 0, '-Infinity'::numeric * -2.5, 'Infinity'::numeric / 0.5, 1.5 / 'Infinity'::numeric, 'NaN'::numeric / 0;
select 'Infinity'::numeric % 2, 5.25 % 'Infinity'::numeric, 'NaN'::numeric % 0, mod(-3, '-Infinity'::numeric);
-- @ordered
select id, x + 1, x * 2, x / 4, x - x, -x from s order by id;

-- Functions.
select -('NaN'::numeric), abs('-Infinity'::numeric), sign('NaN'::numeric), sign('-Infinity'::numeric), sign('Infinity'::numeric);
select round('NaN'::numeric, 2), trunc('Infinity'::numeric), ceil('-Infinity'::numeric), floor('NaN'::numeric), round('Infinity'::numeric);
select scale('NaN'::numeric), min_scale('Infinity'::numeric), trim_scale('NaN'::numeric), trim_scale('-Infinity'::numeric);
select sqrt('NaN'::numeric), sqrt('Infinity'::numeric), exp('-Infinity'::numeric), exp('Infinity'::numeric), ln('Infinity'::numeric), log('Infinity'::numeric), log('NaN'::numeric);
select log(2, 'Infinity'::numeric), log('Infinity'::numeric, 2), log('Infinity'::numeric, 'Infinity'::numeric), log(1, 'Infinity'::numeric);
select power('NaN'::numeric, 0), power(1, 'NaN'::numeric), power('NaN'::numeric, 1), power(2, 'Infinity'::numeric), power(0.5, 'Infinity'::numeric), power(-1, 'Infinity'::numeric), power(-2, '-Infinity'::numeric);
select power('Infinity'::numeric, -2), power('Infinity'::numeric, 0.5), power('-Infinity'::numeric, 3), power('-Infinity'::numeric, 2), power('-Infinity'::numeric, -3), power(0, 'Infinity'::numeric);
select greatest(1, 'NaN'::numeric), least('-Infinity'::numeric, 1);

-- Casts.
select 'NaN'::numeric::float8, 'Infinity'::numeric::float8, '-Infinity'::numeric::float8, 'NaN'::float8::numeric, '-Infinity'::float8::numeric;
select 'NaN'::numeric::text, 'Infinity'::numeric::text, '-Infinity'::numeric::varchar;
-- @ordered
select id, f::numeric, x::float8 from s order by id;

-- Comparisons, ordering, grouping, DISTINCT, set operations.
select 'NaN'::numeric = 'NaN'::numeric, 'NaN'::numeric > 'Infinity'::numeric, '-Infinity'::numeric < -1e100, 'NaN'::numeric = 'NaN'::float8;
select 'NaN'::numeric in (1, 'NaN'), 'Infinity'::numeric between 1 and 'Infinity';
-- @ordered
select x from s order by x desc, id;
-- @ordered
select x, count(*) from s group by x order by x;
-- @ordered
select distinct x from s order by 1;
-- @ordered
select x from s union select 'NaN'::float8 order by 1;
select count(*) from s where x > 1e300;
select count(*) from s where x = 'NaN';
-- @ordered
select x from u where x >= 5 order by x;
-- @ordered
select x from u where x < 0 order by x desc;
-- @ordered
select a.id, b.id from s a join s b on a.x = b.x where a.id < b.id order by 1, 2;

-- Aggregates.
select sum(x), avg(x), min(x), max(x) from s;
select sum(x), avg(x) from s where x < 0 or x = 0;
select sum(x) from s where id in (2, 4);
select sum(y), avg(y), max(y) from s;
-- @ordered
select id, sum(x) over (order by id rows between 1 preceding and current row) from s order by id;

-- Errors.
-- @error 22P02
select '-NaN'::numeric;
-- @error 22003
select 'Infinity'::numeric(5,2);
-- @error 22003
insert into s values (9, 1, '-Infinity', null);
-- @error 22012
select 'Infinity'::numeric / 0;
-- @error 22012
select mod('-Infinity'::numeric, 0);
-- @error 0A000
select 'NaN'::numeric::int8;
-- @error 0A000
select 'Infinity'::numeric::int4;
-- @error 2201F
select sqrt('-Infinity'::numeric);
-- @error 2201E
select ln('-Infinity'::numeric);
-- @error 2201F
select power('-Infinity'::numeric, 0.5);
-- @error 2201F
select power(0, '-Infinity'::numeric);
-- @error 22008
select to_timestamp('NaN'::numeric);
-- @error 23505
insert into u (x) values ('NaN');
