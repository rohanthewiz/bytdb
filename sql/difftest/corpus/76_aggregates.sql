-- Aggregates beyond the classic five (plan D-53): string_agg, array_agg,
-- bool_and / bool_or / every, FILTER (WHERE ...), ORDER BY inside the
-- call. bytdb's one array type is text[], so array_agg over a non-text
-- column compares by value only (@notype): {1,2,3} either way.

-- @setup
set timezone = 'UTC';
create table a (id int primary key, g text, s text, b bool, n int, x float8, ts timestamptz);
insert into a values
  (1, 'p', 'x',   true,  3,    1.5, '2024-01-01 00:00:00+00'),
  (2, 'p', 'y',   false, 1,    2.5, '2024-01-02 12:30:00+00'),
  (3, 'q', null,  true,  2,    null, null),
  (4, 'q', 'z z', null,  null, 4.0, '2024-01-03 00:00:00+00'),
  (5, 'r', 'a,b', true,  5,    0.5, '2024-01-04 00:00:00+00'),
  (6, 'r', '',    true,  4,    1.0, null);

-- @queries
-- @ordered
select g, string_agg(s, ',' order by id) from a group by g order by g;
-- @ordered
select g, string_agg(s, ' | ' order by n desc nulls last) from a group by g order by g;
select string_agg(s, '-' order by s desc) from a;
select string_agg(s, null order by id) from a;
select string_agg(s, ',') from a where false;
select string_agg(distinct g, '/' order by g) from a;
-- @notype
select array_agg(n order by id) from a;
-- @notype
select array_agg(n order by n nulls first) from a;
-- @notype
select array_agg(n order by n desc) from a where n > 1;
select array_agg(s order by id) from a;
select array_agg(distinct g order by g desc) from a;
-- @notype
select array_agg(b order by id) from a;
-- @notype
select array_agg(ts order by id) from a;
-- @notype
select array_agg(x order by x) from a;
select array_agg(s) from a where false;
-- @ordered
select g, bool_and(b), bool_or(b), every(b) from a group by g order by g;
select bool_and(b), bool_or(b) from a where false;
select bool_and(n > 0), bool_or(n > 4) from a;
-- @ordered
select g, count(*) filter (where b), count(*), count(n) filter (where n > 1) from a group by g order by g;
select count(*) filter (where n > 1), sum(n) filter (where g = 'p'), avg(x) filter (where b), max(s) filter (where s <> '') from a;
-- @notype
select array_agg(id order by id) filter (where b) from a;
select string_agg(s, ',' order by id) filter (where s <> '') from a;
-- @ordered
select g, count(*) filter (where b is not true) from a group by g having count(*) filter (where b) > 0 order by g;
select sum(n order by n), count(* ) from a;
-- @notype
-- @ordered
select g, array_agg(n order by n) filter (where n is not null), sum(n) from a group by g order by g;
-- @error
select string_agg(n, ',') from a;
-- @error
select bool_and(n) from a;
-- @error
select array_agg(distinct g order by n) from a;
-- @error
select count(*) filter (where n) from a;
-- @error
select count(*) filter (where count(*) > 1) from a;
-- @error
select string_agg(s, ',' order by id) over () from a;
