-- Numeric sqrt, exp, ln, log and power (N-052). Postgres has numeric
-- and double precision versions of each, picks one by the argument
-- types, and gives the numeric results its own scale rules. Numeric
-- columns compare as exact text, so digits and scale must agree.

-- @setup
create table m (id int primary key, n numeric, i int, f float8);
insert into m values
  (1, 16.0, 16, 16),
  (2, 2, 2, 2.5),
  (3, 0.0001, 1, 0.5),
  (4, 1.00001, 10, 100),
  (5, 123456789.123, 3, 1e-3),
  (6, null, null, null);

-- @queries
-- Version choice: numeric only for a numeric argument with no float one;
-- two-argument log is numeric-only, so integers go there too. (::text:
-- Postgres's pg_typeof returns regtype, bytdb's text.)
select pg_typeof(sqrt(2))::text, pg_typeof(sqrt(2.0))::text, pg_typeof(exp(1))::text, pg_typeof(ln(1.0))::text, pg_typeof(log(8))::text, pg_typeof(log(8.0))::text, pg_typeof(log(2, 8))::text, pg_typeof(power(2, 3))::text, pg_typeof(power(2, 3.0))::text, pg_typeof(power(2.0, 3::float8))::text, pg_typeof(cbrt(27.0))::text;
-- @ordered
select id, sqrt(n), exp(n / 1000), ln(n), log(n), log(2, n), power(n, 2), power(n, 0.5), power(n, i) from m order by id;
-- @ordered
select id, sqrt(i), ln(i), power(i, 2), log(i, 1000) from m where id < 6 order by id;
-- @notype
-- @ordered
select id, sqrt(f), exp(f / 100), ln(f), log(f), power(f, 2) from m where id < 6 order by id;

-- The scale rules at the textbook values.
select sqrt(16.0), sqrt(0::numeric), sqrt(0.0001), exp(1.0), exp(0::numeric), exp(-1.5), exp(100.5), ln(2.0), ln(1.0), ln(1.00001);
select log(100.0), log(2.0, 8.0), log(10, 2.5), log10(100.0), log(2, 8), power(2.0, 10), power(2::numeric, 2.0), power(1.5, 3), power(2.0, -2), power(2.0, 0.5), power(-2.0, 3), power(0.0, 0.5), power(0::numeric, 0);

-- Near 1 and near ties, where the series' working scales decide the
-- last digit.
select ln(1.0000000000000001), ln(0.9999999999999999), log(1.000000000000000000005), exp(0.0000000000001), exp(-0.00000000000005);
select power(1.0000000000000001, 13.75), power(0.999999999999965, 13.75), power(0.999999999999999999999999105, 17.7);

-- Integer exponents, large and negative; past int32 the sign follows parity.
select power(1.0000001, 100000000), power(0.9999, 70000), power(1.1, -300), power(0.99999999, -2147483648), power(-1.0000000001, 3000000001);

-- Untyped arguments take the chosen version's type.
select sqrt('16'), power(2.0, '3'), log('2', '8');

-- @error 2201F
select sqrt(-1.0);
-- @error 2201E
select ln(0.0);
-- @error 2201E
select ln(-1.0);
-- @error 2201E
select log(0.0, 10);
-- @error 2201F
select power(0.0, -1);
-- @error 2201F
select power(-2.0, 0.5);
-- @error 22012
select log(1.0, 10.0);
-- @error 22003
select exp(6000.0);
-- @error 22003
select power(10::numeric, 200000);
