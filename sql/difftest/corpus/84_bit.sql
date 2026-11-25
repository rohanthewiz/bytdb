-- bit and bit varying (N-053): literals (B'' and X''), bit(n) exact
-- and varbit(n) maximum lengths, explicit casts that pad or truncate,
-- the bitwise operators on bit strings and on integers, comparison and
-- keys, the bit functions, the int casts, and errors.

-- @setup
create table b (id bigint primary key, f bit(4), v varbit(6), u varbit, one bit);
insert into b values (1, B'1010', B'1', X'1F', B'1'), (2, '0101', '110011', '', '0'), (3, X'F', 'x3', B'1010101010', B'0'), (4, null, null, null, null);
create table bk (k varbit primary key, n int);
insert into bk values (B'1', 1), (B'10', 2), (B'0', 3), (B'', 4), (B'11', 5), (B'0111', 6);

-- @queries
-- @ordered
select id, f, v, u, one from b order by id;
-- @ordered
select k, n from bk order by k;
-- @ordered
select k, n from bk order by k desc;
-- @ordered
select n from bk where k > B'1' order by n;
-- @ordered
select n from bk where k between B'0' and '1' order by n;

-- Literals and casts.
select B'0101', X'1F', X'a0', B'', '0101'::bit(4), 'x1F'::bit(8), 'b101'::varbit, B'101'::bit, B'101'::bit(5), B'10101'::bit(3), B'10101'::varbit(2), B'1'::varbit(4);
select 5::bit(4), (-1)::bit(4), 5::bit(40), (-5)::bit(40), B'0101'::int, B'11111111111111111111111111111111'::int, B'0101'::bigint, B'101'::text, '101'::text::bit(3);

-- Operators.
select B'1100' & B'1010', B'1100' | B'1010', B'1100' # B'1010', ~B'1100', B'1100' << 1, B'1100' >> 2, B'1100' << 5, B'1100' >> -1, B'11' || B'0';
select 5 & 3, 5 | 3, 5 # 3, ~5, 1 << 3, 16 >> 2, -16 >> 2, 6 & 3 | 8;
-- @ordered
select id, f & B'0110', f | B'0001', ~f, f << 1, v || u, f::varbit || v from b where id < 4 order by id;

-- Comparison.
select B'101' = B'101'::varbit, B'1' < B'10', B'10' < B'1', B'0111' < B'1', B'' < B'0';

-- Functions.
select length(B'10101'), bit_length(B'10101'), octet_length(B'10101'), octet_length(B'101010101'), bit_count(B'10101'), get_bit(B'1010', 0), get_bit(B'1010', 3), set_bit(B'1010', 1, 1);
select substring(B'101100' from 2 for 3), substring(B'101100' from 4), position(B'11' in B'0110'), position(B'111' in B'0110');
-- @ordered
select id, length(v), bit_count(u), octet_length(u) from b where id < 4 order by id;

-- Catalog.
-- @ordered
select a.attname::text, format_type(a.atttypid, a.atttypmod) from pg_attribute a join pg_class c on c.oid = a.attrelid where c.relname = 'b' and a.attnum > 0 order by a.attnum;

-- Errors.
-- @error 22P02
select B'102';
-- @error 22P02
select X'G';
-- @error 22P02
select '1 0'::varbit;
-- @error 22026
insert into b (id, f) values (9, B'101');
-- @error 22001
insert into b (id, v) values (9, B'1010101');
-- @error 22026
select B'10' & B'110';
-- @error 22026
select B'10' # B'110';
-- @error 2202E
select get_bit(B'1010', 4);
-- @error 22023
select set_bit(B'1010', 0, 2);
-- @error 22003
select (B'1' || B'00000000000000000000000000000000')::bit(33)::int;
-- @error 42846
select B'1'::varbit::int;
-- @error 42883
select max(f) from b;
-- @error 42883
select B'1' + B'1';
-- @error 23505
insert into bk values ('10', 9);
