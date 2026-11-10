-- Smoke coverage: things that already agree with Postgres. If this file
-- fails, the harness or the environment is broken, not a plan item.

-- @setup
create table t (id int primary key, name text, age int, score float8);
insert into t values (1, 'ada', 36, 1.5), (2, 'grace', 45, 2.25), (3, 'alan', 41, null), (4, null, null, 0.5);

-- @queries
select 1;
select id, name from t;
-- @ordered
select id from t order by id desc;
select count(*), sum(age), min(age), max(age) from t;
select name from t where age > 40;
select name from t where name like 'a%';
select id from t where score is null;
select coalesce(name, '?') from t;
select age / 2, age % 7 from t where age is not null;
select upper(name), length(name) from t where name is not null;
select 1 / 0;
-- @error
select nosuchcol from t;
select true and null, false and null, true or null, not null::bool;
select 'a' || 'b', 'x' || null;
select case when age > 40 then 'old' else 'young' end from t;
