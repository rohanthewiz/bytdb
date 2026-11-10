-- Smaller SQL correctness items (plan D-21).

-- @setup
create table s (id serial primary key check (id > 5), v int);
create table d (id int primary key, x int);
insert into d values (1, 1), (2, 2);

-- @queries
-- @todo D-21: coalesce evaluates every argument
select coalesce(1, 1/0);
-- @error
-- @todo D-21: CHECK never sees the drawn identity value
insert into s (v) values (1);
-- @error
-- @todo D-21: duplicate derived-table column resolves silently
select x.id from (select d.id, d2.id from d, d d2) x;
select count(distinct x) from d;
