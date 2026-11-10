-- IN-lists, = ANY(constant array) and OR-of-equalities on key columns
-- (plan D-38). They now run as key probes over sorted, de-duplicated
-- keys; the answers — NULL elements, NOT IN, empty arrays, order served
-- by the probed index — must stay exactly Postgres's.

-- @setup
create table o (id int primary key, uid int not null, tag text, ref uuid);
create index o_uid on o (uid, id);
insert into o values
  (1, 3, 'a', '00000000-0000-4000-8000-000000000001'),
  (2, 1, 'b', '00000000-0000-4000-8000-000000000002'),
  (3, 3, null, '00000000-0000-4000-8000-000000000003'),
  (4, 2, 'a', null),
  (5, 1, 'c', '00000000-0000-4000-8000-000000000005'),
  (6, 2, 'b', '00000000-0000-4000-8000-000000000006');
create table u (id int primary key, name text);
insert into u values (1, 'one'), (2, 'two'), (3, 'three');

-- @queries
select id from o where id in (5, 2, 2, 9);
select id from o where id in (5, null);
select id from o where id not in (5, null);
select id from o where id not in (1, 2, 3);
select id from o where id = any('{6,4}');
select id from o where id = any(array[3, 1]);
select id from o where id = any('{}');
select id from o where id = 4 or id = 1;
select id from o where id = 4 or uid = 1;
-- @ordered
select id, uid from o where uid in (3, 1) order by uid, id;
-- @ordered
select id, uid from o where uid in (3, 1) order by uid desc, id desc;
-- @ordered
select id from o where uid in (2, 3) order by id limit 2;
select id from o where ref in ('00000000-0000-4000-8000-000000000003', '00000000-0000-4000-8000-000000000006');
select id from o where tag in ('a', 'b', 'x', 'y', 'z');
select id, tag in ('a', 'q', 'r', 's', null) from o;
-- @ordered
select u.name, o.id from u join o on o.uid = u.id where u.id in (2, 3) order by o.id;
select count(*) from u join o on o.uid = u.id where o.id in (1, 2, 3);
