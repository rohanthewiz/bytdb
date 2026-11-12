-- Duplicate relation names (N-054). Tables, views and sequences share
-- one namespace, so each kind collides with the others, and every
-- collision is Postgres's 42P07 duplicate_table. bytdb used to send
-- XX000 for all of them. An index name collides with another index on
-- the same table here: bytdb scopes index names per table, Postgres
-- per schema, and on one table the two agree.

-- @setup
create table t (id int primary key, a int);
create index t_a on t (a);
create view v as select id from t;
create sequence s;
create table u (id int primary key);

-- @queries
-- @error 42P07
create table t (id int primary key);
-- @error 42P07
create table v (id int primary key);
-- @error 42P07
create table s (id int primary key);
-- @error 42P07
create view t as select 1;
-- @error 42P07
create view v as select 2;
-- @error 42P07
create sequence t;
-- @error 42P07
create sequence s;
-- @error 42P07
create index t_a on t (id);
-- @error 42P07
alter table u rename to v;

-- Nothing changed: the failed statements left every relation as it was.
select id, a from t;
select count(*) from v;
