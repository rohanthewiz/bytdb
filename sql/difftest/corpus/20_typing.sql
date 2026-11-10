-- Static typing of literals and expressions (plan D-23 to D-26).

-- @setup
create table ev (id int primary key, due date, at timestamp, u uuid, n int, f float8);
insert into ev values (1, '2099-01-01', '2024-02-29 12:00:00', 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', 1, 1.0);
insert into ev values (2, '2024-02-29', '2019-06-01 00:00:00', 'b0eebc99-9c0b-4ef8-bb6d-6bb9bd380a12', 2, 2.5);
create table ck (id int primary key, day date check (day >= '2000-01-01'));

-- @queries
select id from ev where due > now();
select id from ev where at::date = '2024-02-29';
select at::date from ev;
select case when at > '2020-01-01' then 'recent' else 'old' end from ev;
-- @error
insert into ck values (1, '1999-01-01');
select id from ev where id in (select id from ev where due = '2024-02-29');
with x as (select id from ev where due = '2024-02-29') select id from x;
select id from (select id, due from ev) s where s.due = '2024-02-29';
select id from ev where u in ('a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11');
select id from ev where at in ('2024-02-29 12:00:00');
select id from ev where coalesce(at, now()) < '2025-01-01';
select id from ev where (select max(at) from ev) > '2020-01-01';
select at > '2024-01-01' from ev;
select id from ev where due > at;
-- @bytdb: TIMESTAMP and TIMESTAMPTZ are one type (docs/gotchas.md), so ::text carries the +00 offset
select at::text from ev;
select u::text from ev;
select 'x' || due from ev;
select 1000000.0::float8::text;
select 1::bool;
select true::int;
select coalesce(n, 0) + 1 from ev;
select nullif(n, 2) from ev;
select exists (select 1 from ev);
select case when n = 1 then null else n end from ev;
select (select max(n) from ev);
select 1 union select 1.0;
select count(*) from (select distinct x from (select 1 as x union all select 1.0) s) s2;
-- @error
select 1 < 'a';
-- @error
select 1 = true;
-- @error
select 1 < 2 < 3;
-- @error
select at + 1 from ev;
select due + 1 from ev;
select due - 1 from ev;
select length(f::text) from ev where id = 2;
-- Further shapes the typed bind pass covers.
select id from ev where due between '2024-01-01' and '2024-12-31';
select id from ev where u = any(array['a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11']::uuid[]);
select id from ev where due in ('2024-02-29', '2099-01-01');
select case n when 1 then 'one' else 'many' end from ev;
select coalesce(nullif(n, 1), 0) + 1 from ev;
select id from ev where at::date > due - 30;
select due - due, due + 7 from ev;
select '2024-03-01'::date - due from ev;
select n + '5' from ev;
-- @error 22P02
select n + 'x' from ev;
-- @error 42883
select id from ev where n = true;
-- @error 42883
select 'a' || 1 = 1;
select 1.5::text, '-0'::float8::text, 1e300::float8::text, 0.00001::float8::text;
select true::text, 1::bool, 0::bool, false::int;
select at::date::text from ev;
