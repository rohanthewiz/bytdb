-- gen.sql: Postgres 16's answers for money_golden_test.go. Run by
-- gen.sh in a session whose lc_monetary is en_US.utf8 (the docker
-- image's default). One tab-separated case per line:
--
--   op <TAB> arg1 <TAB> arg2 <TAB> expected
--
-- Money operands are given as cents; results print as Postgres prints
-- them (money as cash_out text), or "ERROR <sqlstate>".

set client_min_messages = warning;
set lc_monetary = 'en_US.utf8';

create or replace function pg_temp.try(q text) returns text language plpgsql as $$
declare r text;
begin
  execute q into r;
  return coalesce(r, 'NULL');
exception when others then
  return 'ERROR ' || sqlstate;
end $$;

-- cents -> money, via int8 (exact, any value).
create or replace function pg_temp.m(c bigint) returns money language sql as $$
  select (c::numeric * 0.01)::money -- exact; numeric division would round 19-digit values
$$;

create temp table inputs (s text);
insert into inputs values
  (''), (' '), ('0'), ('1'), ('-1'), ('+1'), ('1.5'), ('1.50'), ('1.005'), ('1.015'),
  ('1.0049'), ('1.0050001'), ('-1.005'), ('-0.005'), ('0.001'), ('.5'), ('5.'), ('.'),
  ('$'), ('$1'), ('$ 1'), ('-$1'), ('$-1'), ('  -  $ 1 '), ('(1)'), ('($1,000.00)'),
  ('(5'), ('5)'), ('1-'), ('1.00-'), ('5 $'), ('1,234.56'), ('$1,234.567'), ('1,2,3'),
  ('1.2,3'), ('1.2.3'), ('--5'), ('+-5'), ('1e3'), ('abc'), ('1 dollars'), ('1 USD'),
  ('92233720368547758.07'), ('92233720368547758.08'), ('-92233720368547758.08'),
  ('-92233720368547758.09'), ('92233720368547758.065'), ('12345678901234567890'),
  ('0.0099'), ('999.995'), ('-999.995'), ('1,000,000'), ('$1,000,000.00'), ('1.2a');

create temp table cents (c bigint);
insert into cents values (0), (1), (-1), (5), (99), (100), (-100), (150), (700), (-700),
  (123456), (-123456), (100000000), (9223372036854775807), (-9223372036854775808),
  (3), (7), (-7), (1000000099), (10500), (2110), (100100), (101500);

create temp table ints (n bigint);
insert into ints values (0), (1), (-1), (2), (3), (-2), (100), (9223372036854775807);

create temp table floats (f text);
insert into floats values ('0'), ('1'), ('-1'), ('0.5'), ('1.5'), ('2'), ('3'), ('0.1'),
  ('1e-300'), ('1e300'), ('nan'), ('infinity'), ('-0.5'), ('0.333333333333');

select 'in', s, '', pg_temp.try(format('select %L::money::text', s)) from inputs;
select 'out', c::text, '', pg_temp.m(c)::text from cents;
select 'words', c::text, '', cash_words(pg_temp.m(c)) from cents;
select 'tonum', c::text, '', pg_temp.m(c)::numeric::text from cents;
select op, a.c::text, b.c::text, pg_temp.try(format('select (pg_temp.m(%s) %s pg_temp.m(%s))::text', a.c, sym, b.c))
  from cents a, cents b, (values ('add', '+'), ('sub', '-'), ('div', '/')) o(op, sym);
select op, c::text, n::text, pg_temp.try(format('select (pg_temp.m(%s) %s %s::bigint)::text', c, sym, n))
  from cents, ints, (values ('muli', '*'), ('divi', '/')) o(op, sym);
select op, c::text, f, pg_temp.try(format('select (pg_temp.m(%s) %s %L::float8)::text', c, sym, f))
  from cents, floats, (values ('mulf', '*'), ('divf', '/')) o(op, sym);
select 'fromnum', s, '', pg_temp.try(format('select (%s)::numeric::money::text', s)) from (values
  ('0'), ('1.005'), ('-1.005'), ('1.004999'), ('92233720368547758.07'), ('92233720368547758.08'),
  ('-92233720368547758.08'), ('-92233720368547758.085'), ('0.995'), ('12.3456')) t(s);
select 'fromint', n::text, '', pg_temp.try(format('select (%s)::bigint::money::text', n)) from (values
  (0), (5), (-5), (92233720368547758), (92233720368547759), (-92233720368547758), (-92233720368547759)) t(n);
