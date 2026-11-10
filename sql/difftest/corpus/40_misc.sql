-- Smaller SQL correctness items (plan D-21).

-- @setup
create table s (id serial primary key check (id > 5), v int);
create table d (id int primary key, x int);
insert into d values (1, 1), (2, 2);

-- @queries
-- @todo D-25: coalesce described as text (it now short-circuits: D-21)
select coalesce(1, 1/0);
-- @error
insert into s (v) values (1);
-- @error
select x.id from (select d.id, d2.id from d, d d2) x;
select count(distinct x) from d;
