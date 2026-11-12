-- The scalar function library (plan D-53). Since N-048 bytdb has a real
-- numeric type, and a decimal literal is numeric, so round/trunc over
-- one compare digit for digit (difftest compares numeric columns as
-- exact text). The numeric file is 77_numeric.sql.

-- @setup
set timezone = 'UTC';
create table f (id int primary key, i int, x float8, s text, ts timestamptz, d date, j jsonb, b bytea);
insert into f values
  (1, -7, -2.5, '  Hello World  ', '2024-02-29 13:45:30.123456+00', '2024-02-29', '{"a": [1, 2, 3]}', '\x0102ff'),
  (2, 12, 3.7, 'élan,vital,é', '1999-12-31 23:59:59+00', '1970-01-01', '[1, "x", null]', '\x'),
  (3, null, null, null, null, null, null, null);

-- @queries
-- @ordered
select id, abs(i), abs(x), sign(i), sign(x), mod(i, 5), ceil(x), floor(x) from f order by id;
select round(2.5), round(-2.5), round(2.345, 2), trunc(-2.7), trunc(1.239, 2);
select power(2, 10), sqrt(16), cbrt(27), exp(0), ln(1), log(100), log(2, 8), pi() > 3.14;
select greatest(1, 5, 3), least(4, null, 2), greatest(null::int, null);
-- @error
select sqrt(-1);
-- @error
select mod(1, 0);
-- @ordered
select id, substring(s, 3, 5), substr(s, 3), substring(s from 3 for 5), substring(s for 4), left(s, 4), right(s, 3), left(s, -10), right(s, -10) from f order by id;
-- @ordered
select id, trim(s), ltrim(s), rtrim(s), btrim(s), trim(both ' H' from s), trim(leading ' ' from s), trim(trailing from s) from f order by id;
-- @ordered
select id, replace(s, 'l', 'L'), strpos(s, 'o'), position('o' in s), split_part(s, ',', 2), split_part(s, ',', -1), reverse(s) from f order by id;
-- @ordered
select id, lpad(s, 20, '*'), rpad(s, 5), lpad('ab', 1), repeat('ab', 3), initcap('hello wORLD-foo bar') from f order by id;
-- @ordered
select id, starts_with(s, '  He'), md5(s), ascii(s), octet_length(s), length(s), length(b), octet_length(b) from f order by id;
select chr(65), chr(233), concat('a', null, 1, true, 2.5), concat_ws('-', 'a', null, 'b', 3), concat_ws(null, 'a');
-- @ordered
select id, extract(year from ts), extract(month from ts), extract(day from ts), extract(hour from ts), extract(dow from ts), extract(doy from ts), extract(quarter from ts) from f order by id;
-- @notype
-- @ordered
select id, extract(epoch from ts), date_part('second', ts), extract(week from d), extract(isodow from d) from f order by id;
-- @ordered
select id, date_trunc('month', ts), date_trunc('hour', ts), date_trunc('year', d), date_trunc('week', ts), date_trunc('quarter', ts) from f order by id;
-- @ordered
select id, to_char(ts, 'YYYY-MM-DD HH24:MI:SS'), to_char(ts, 'Mon DD, YYYY HH12:MI AM'), to_char(d, 'DDD Q "q"') from f order by id;
select to_timestamp(0), to_timestamp(1709214330), make_date(2024, 2, 29);
-- @error
select make_date(2023, 2, 29);
-- @ordered
-- @bytdb: TIMESTAMP and TIMESTAMPTZ are one type, so AT TIME ZONE's wall time comes back as timestamptz rendered with +00
select id, ts at time zone 'UTC', ts at time zone 'America/New_York' from f order by id;
-- @ordered
select id, jsonb_typeof(j), jsonb_typeof(j -> 'a'), jsonb_array_length(j -> 'a') from f where id <> 2 order by id;
select jsonb_array_length('[1,2]'::jsonb), jsonb_typeof('"s"'::jsonb), jsonb_typeof('null'::jsonb);
-- @error
select no_such_function(1);
-- @error
select upper(1);
-- @error
select length(123);
