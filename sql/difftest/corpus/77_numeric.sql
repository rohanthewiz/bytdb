-- The numeric / decimal type (N-048). Both sides type these columns
-- numeric (OID 1700), so difftest compares them as exact text: digits
-- and display scale must agree, not just the value.

-- @setup
create table n (id int primary key, a numeric(10,2), b numeric, c numeric(5,-1), f float8, i int);
insert into n values
  (1, 12.345, 1.0, 12345, 1.5, 7),
  (2, -0.005, 1.00, 4, 0.1, -3),
  (3, 99999999.994, 123456789012345678901234567890.123456789, null, null, null),
  (4, null, 0.000, 5, -2.25, 10),
  (5, 0, -42, -55, 3, 0);
create table k (p numeric primary key, q numeric(6,3));
create unique index k_q on k (q);
insert into k values (1.50, 1), (2, 2.5), (-0.001, null);

-- @queries
-- @ordered
select id, a, b, c from n order by id;
-- @ordered
select p, q from k order by p;

-- Literals: a point or an exponent makes a numeric, with its written scale.
select 0.1 + 0.2, 1.50e1, 1e3, 1_000.000_5, .5, 5., 12345678901234567890;
select 1.10 * 2.00, 1.5 - 2.25, -1.50, 7.5 % 2, -7.5 % 2;

-- Division picks enough digits for 16 significant ones.
select 1/3.0, 10.0/4, 2/3::numeric, 100::numeric/3, 0.001::numeric/3, 99999::numeric/7, 0.05/0.0003, 1.0/7e10;

-- Casts in and out: numeric -> int rounds half away from zero, float8
-- -> numeric keeps 15 significant digits, typmods round.
select 2.5::int, (-2.5)::int, 1.4999::int, 12.5::float8, 0.1::float8::numeric, 1e20::float8::numeric;
select '12.345'::numeric(5,2), 0.005::numeric(3,2), 123.5::numeric(3,0), 12345::numeric(2,-3), '  -1.50  '::numeric;
select (1/3.0)::float8, 2.50::text, (-0.0)::numeric, '1e-3'::numeric, '-0.00'::numeric;

-- Arithmetic across the promotion ladder: int -> numeric -> float8.
-- @ordered
select id, a + b, a * i, i + a, -a, a / 3, f + a from n where id in (1, 2, 5) order by id;

-- Comparisons ignore scale; numeric meets float8 as float8.
select id from n where b = 1;
select id from n where a > 10 order by id;
select id from n where a between -1 and 1 order by id;
select id from n where b in (1.000, -42) order by id;
select 1.0 = 1.00, 1.5::numeric = 1.5::float8, 0.1::numeric = 0.1::float8, 2 < 2.5, 2.5 < 3::float8;
select p from k where p = 1.5;
select p from k where p > 0 order by p;

-- Aggregates: sum keeps numeric, avg over integers and numerics is numeric.
select sum(a), avg(a), min(a), max(a), count(distinct b), sum(b) from n;
select sum(i), avg(i), avg(distinct i), sum(c), avg(c) from n;
select b, count(*) from n where b is not null group by b order by b;
select sum(a)::numeric(12,1), round(avg(a), 2) from n;

-- Functions with numeric implementations.
-- @ordered
select id, round(a, 1), round(a), trunc(b, 2), abs(a), ceil(a), floor(a), sign(a), mod(a, 3) from n where id in (1, 2, 5) order by id;
select round(2.345, 2), round(-2.5), round(1234.5, -2), trunc(-2.789, 1), ceil(2.10), floor(-1.5), sign(-2.50), abs(-2.50);
select round(5, 1), mod(-7, 2.5), mod(7.5, 2), scale(1.500), scale(5), min_scale(1.500), trim_scale(1.500);
select greatest(1, 2.5, 2), least(4, 1.25, 3), coalesce(null::numeric, 0.50);
select case when id = 1 then 1 else 1.5 end from n order by id;

-- Writes: UPDATE and INSERT ... SELECT go through the typmod.
-- @setup
update n set a = a * 1.1 where id = 1;
insert into n (id, a, b) select 10 + id, b, a from n where id in (1, 2);
-- @queries
-- @ordered
select id, a, b from n where id in (1, 11, 12) order by id;

-- Errors.
-- @error 22003
select 123.456::numeric(4,2);
-- @error 22003
insert into n (id, a) values (20, 123456789);
-- @error 22012
select 1 / 0.0;
-- @error 22012
select mod(1.5, 0);
-- @error 22P02
select 'abc'::numeric;
-- @error 23505
insert into k values (1.5000, 9);
-- @error 23505
insert into k values (9, 2.500);

-- The catalog a client introspects.
select format_type(a.atttypid, a.atttypmod) from pg_attribute a join pg_class t on t.oid = a.attrelid
  where t.relname = 'n' and a.attname in ('a', 'b', 'c') order by a.attnum;
select column_name, data_type, numeric_precision, numeric_precision_radix, numeric_scale
  from information_schema.columns where table_name = 'n' and column_name in ('a', 'b') order by ordinal_position;
