-- Everyday SQL that used to fail to parse (plan D-52): casts and typed
-- literals, IS forms, NULLS FIRST/LAST, GROUP BY alias, LIMIT/FETCH
-- spellings, FOR UPDATE, JOIN USING, INTERSECT/EXCEPT, parenthesized
-- operands, VALUES, row comparisons, dollar quotes, string continuation,
-- schema-qualified columns, non-ASCII identifiers.

-- @setup
create table t (id int primary key, a int, b text);
create table u (id int primary key, a int, c text);
insert into t values (1, 1, 'x'), (2, 1, 'y'), (3, 2, null), (4, null, 'z'), (5, null, null);
insert into u values (1, 1, 'p'), (2, 3, 'q'), (3, null, 'r');
create table ñandú (id int primary key, peso int);
insert into ñandú values (1, 30);

-- @queries
select cast(a as text), cast('5' as int) + 1 from t where id = 1;
select date '2024-01-02', timestamp '2024-01-02 03:04:05' = '2024-01-02 03:04:05'::timestamp;
-- @notype
select id, a is distinct from 1, a is not distinct from null from t order by id;
-- @ordered
select id, (a > 1) is true, (a > 1) is not false, (a > 1) is unknown from t order by id;
-- @ordered
select id, a from t order by a nulls first, id;
-- @ordered
select id, a from t order by a desc nulls last, id;
-- @ordered
select a as x, count(*) from t group by x order by x;
-- @ordered
select id from t order by id limit all;
-- @ordered
select id from t order by id fetch first 2 rows only;
-- @ordered
select id from t order by id offset 1 rows fetch next 1 row only;
-- @ordered
select id from t where id < 3 order by id for update;
-- @ordered
select id, a, c from t join u using (id) order by id;
-- @ordered
select id, b, c from t left join u using (id) order by id;
-- @ordered
select a from t intersect select a from u order by 1;
-- @ordered
select a from t intersect all select a from u order by 1;
-- @ordered
select a from t except select a from u order by 1;
-- @ordered
select a from t except all select a from u order by 1;
-- @ordered
select a from u union select a from t intersect select a from u order by 1;
select count(*) from ((select a from t order by a limit 1) union all (select a from u order by a desc limit 1)) s;
-- @ordered
values (1, 'a'), (2, 'b') order by 1 desc;
select * from (values (1, 'x'), (2, 'y')) as v(n, s) where n > 1;
-- @ordered
select id from t where (id, a) in ((1, 1), (3, 2), (4, null)) order by id;
-- @ordered
select id from t where (id, a) = (2, 1) or (id, a) <> (1, 1) order by id;
select $$it's$$, $tag$a $$ b$tag$;
select 'foo'
  'bar';
select peso from ñandú;
