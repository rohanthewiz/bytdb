-- citext (N-053): case-insensitive text. Values read back as written;
-- comparison, ordering, grouping, keys and pattern matching ignore case;
-- citext meeting text is text (case-sensitive), and functions take it as
-- text. bytdb's collation is C, so the corpus avoids text whose order
-- depends on the locale (punctuation, digits against letters).

-- @setup
create extension if not exists citext;
create table u (id bigint primary key, email citext unique, name citext, plain text);
insert into u values (1, 'Ada@Example.com', 'Ada', 'Ada'), (2, 'grace@example.COM', 'grace', 'grace'), (3, 'ALAN@example.com', 'ALAN', 'ALAN'), (4, null, 'barbara', 'Barbara'), (5, 'edsger@example.com', 'Edsger', 'edsger');
create table k (c citext primary key, n int);
insert into k values ('Apple', 1), ('banana', 2), ('CHERRY', 3), ('date', 4);

-- @queries
-- @ordered
select id, email, name, plain from u order by id;
-- @ordered
select c, n from k order by c;
-- @ordered
select c, n from k order by c desc;

-- Equality and order ignore case; the stored case comes back.
-- @ordered
select id from u where email = 'ada@EXAMPLE.com' order by id;
-- @ordered
select id, name from u where name > 'b' order by name;
-- @ordered
select n from k where c = 'APPLE' order by n;
-- @ordered
select n from k where c between 'B' and 'cz' order by n;
-- @ordered
select n from k where c in ('apple', 'DATE', 'fig') order by n;
select 'ABC'::citext = 'abc'::citext, 'a'::citext < 'B'::citext, 'B'::citext < 'c'::citext, 'abc'::citext <> 'ABD'::citext;

-- citext meeting text is text = text: case matters.
-- @ordered
select id from u where name = plain order by id;
select 'A'::citext = 'a'::text, 'A'::text = 'a'::citext;

-- Grouping, DISTINCT and set operations fold case.
select count(distinct name) from (values ('a'::citext), ('A'), ('b')) v(name);
-- @ordered
select lower(name::text), count(*) from (values ('x'::citext), ('X'), ('y')) v(name) group by name order by 1;
-- @ordered
select c from (select 'Q'::citext as c union select 'q'::citext) s order by c;

-- A later UNION arm's untyped literal takes the first arm's type (the
-- fix that made the VALUES lists above fold), for any type.
-- @ordered
select d, d + 1 from (values (date '2024-01-31'), ('2024-02-28')) v(d) order by d;

-- Pattern matching ignores case on a citext operand.
-- @ordered
select id from u where email like 'a%' order by id;
-- @ordered
select id from u where name ~ '^[ae]' order by id;
-- @ordered
select id from u where name not like '%A%' order by id;

-- Functions take citext as text and return text.
-- @ordered
select id, upper(name), lower(name), length(name), name || '!', concat(name, '?') from u order by id;
-- @ordered
select id, coalesce(email, 'none'), coalesce(email, 'NONE') = 'none' from u order by id;
select min(c), max(c) from k;
select string_agg(name, ',' order by id) from u;

-- Casts.
select 'MiXeD'::citext, 'MiXeD'::citext::text, citext 'Abc', 'x'::text::citext = 'X';

-- Errors: a unique citext refuses a value equal ignoring case.
-- @error 23505
insert into u values (9, 'GRACE@EXAMPLE.COM', 'g', 'g');
-- @error 23505
insert into k values ('BANANA', 9);
