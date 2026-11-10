-- One statement, one snapshot, none of its own writes (plan D-29, D-31).

-- @setup
create table g (id int primary key, grp int, x int);
insert into g values (1, 1, 10), (2, 1, 20), (3, 1, 30);
create table node (id int primary key, parent int references node(id));
create table p (id int primary key);
create table c (id int primary key, pid int references p(id));
insert into p values (1), (2);
insert into c values (1, 1);

-- @queries
-- @ordered
-- @todo D-29: UPDATE subquery sees the statement's own writes
update g set x = (select max(g2.x) from g g2 where g2.grp = g.grp) + 1 returning id, x;
-- @todo D-29: DELETE RETURNING subquery sees the statement's own deletes
delete from g where id = 3 returning (select count(*) from g);
-- @todo D-31: child FK checked per row, not at end of statement
insert into node values (1, 2), (2, null);
-- @todo D-31: depends on the insert above
update node set id = id + 10, parent = parent + 10;
-- @todo D-31: depends on the insert above
select id, parent from node;
update p set id = case when id = 1 then 2 else 1 end;
select id from p;
