-- Static typing of literals and expressions (plan D-23 to D-26).

-- @setup
create table ev (id int primary key, due date, at timestamp, u uuid, n int, f float8);
insert into ev values (1, '2099-01-01', '2024-02-29 12:00:00', 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', 1, 1.0);
insert into ev values (2, '2024-02-29', '2019-06-01 00:00:00', 'b0eebc99-9c0b-4ef8-bb6d-6bb9bd380a12', 2, 2.5);
create table ck (id int primary key, day date check (day >= '2000-01-01'));

-- @queries
-- @todo D-23: date vs now() compared by Go kind
select id from ev where due > now();
-- @todo D-24: ts::date returns microseconds
select id from ev where at::date = '2024-02-29';
-- @todo D-24: ts::date returns microseconds
select at::date from ev;
-- @todo D-23: literal in CASE condition not typed
select case when at > '2020-01-01' then 'recent' else 'old' end from ev;
-- @error
-- @todo D-23: CHECK literal not typed, constraint inert
insert into ck values (1, '1999-01-01');
-- @todo D-23: literal inside IN (subquery) not typed
select id from ev where id in (select id from ev where due = '2024-02-29');
-- @todo D-23: literal inside a CTE body not typed
with x as (select id from ev where due = '2024-02-29') select id from x;
-- @todo D-23: literal against a derived-table column not typed
select id from (select id, due from ev) s where s.due = '2024-02-29';
-- @todo D-23: IN-list literal not typed as uuid
select id from ev where u in ('a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11');
-- @todo D-23: IN-list literal not typed as timestamp
select id from ev where at in ('2024-02-29 12:00:00');
-- @todo D-23: coalesce(ts, now()) vs literal
select id from ev where coalesce(at, now()) < '2025-01-01';
-- @todo D-23: scalar subquery vs literal
select id from ev where (select max(at) from ev) > '2020-01-01';
-- @todo D-23: comparison in the select list not typed
select at > '2024-01-01' from ev;
-- @todo D-23: date vs timestamp compares days with microseconds
select id from ev where due > at;
-- @todo D-24: ts::text renders microseconds
select at::text from ev;
-- @todo D-24: uuid::text renders raw bytes
select u::text from ev;
-- @todo D-24: text || date renders days
select 'x' || due from ev;
-- @todo D-24: float text rendering
select 1000000.0::float8::text;
-- @todo D-24: int::bool keeps the int
select 1::bool;
-- @todo D-24: bool::int unsupported
select true::int;
select coalesce(n, 0) + 1 from ev;
-- @todo D-25: nullif described as text
select nullif(n, 2) from ev;
-- @todo D-25: exists described as text
select exists (select 1 from ev);
-- @todo D-25: CASE whose first THEN is NULL described as text
select case when n = 1 then null else n end from ev;
-- @todo D-25: scalar subquery described as text
select (select max(n) from ev);
-- @todo D-26: 1 and 1.0 hash apart under UNION
select 1 union select 1.0;
-- @todo D-26: 1 and 1.0 hash apart under DISTINCT
select count(*) from (select distinct x from (select 1 as x union all select 1.0) s) s2;
-- @error
-- @todo D-23: uncoercible comparison yields NULL instead of an error
select 1 < 'a';
-- @error
-- @todo D-23: int = bool yields NULL instead of an error
select 1 = true;
-- @error
-- @todo D-23: chained comparison accepted
select 1 < 2 < 3;
-- @error
-- @todo D-23: timestamp + int accepted (Q1: reject)
select at + 1 from ev;
-- @todo D-23: date + int typed int8, not date
select due + 1 from ev;
-- @todo D-23: date - int typed int8, not date
select due - 1 from ev;
select length(f::text) from ev where id = 2;
