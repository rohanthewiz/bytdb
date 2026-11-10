-- DML forms that used to fail to parse (plan D-52): INSERT ... SELECT
-- (including the first-N-columns rule), UPDATE ... FROM, DELETE ...
-- USING, target aliases, SET col = DEFAULT, ON CONFLICT ... SET col =
-- DEFAULT. Each statement's effect is checked by the queries after it.

-- @setup
create table t (id int primary key, a int default 7, b text default 'bd');
create table u (id int primary key, a int, c text);
insert into t (id, a, b) values (1, 1, 'x'), (2, null, 'y'), (3, 3, null);
insert into u values (1, 10, 'p'), (3, 30, 'q'), (4, 40, 'r');
insert into t select id + 10, a, c from u where a > 15;
insert into t (id, b) select id + 20, c from u where c <> 'p';
insert into t select id + 30 from u where id = 1;
insert into t (select id + 40, a, 'paren' from u where id = 4);
update t set a = u.a from u where u.id = t.id;
update t as x set b = 'aliased' where x.id = 2;
update t set b = default where id = 3;
update t set a = default, b = u.c from u where u.id + 10 = t.id;
delete from t using u where u.id + 20 = t.id and u.c = 'q';
delete from t as x where x.id = 44;
insert into t values (1, 99, 'z') on conflict (id) do update set a = default;

-- @queries
-- @ordered
select id, a, b from t order by id;
select count(*) from t;
