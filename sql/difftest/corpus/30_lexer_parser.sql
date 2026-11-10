-- Lexer and parser divergences (plan D-22).

-- @setup
create table i (id int primary key, v int);
insert into i values (1, 10), (2, null);

-- @queries
-- @todo D-22: hex integer literal
select 0x1F;
-- @todo D-22: octal integer literal
select 0o17;
-- @todo D-22: binary integer literal
select 0b101;
-- @todo D-22: underscore digit separators
select 1_000;
-- @error
-- @todo D-22: trailing junk after a number
select 1a;
-- @todo D-22: E-string \\x escape
select E'\x41';
-- @todo D-22: E-string octal escape
select E'\101';
select E'é';
-- @todo D-22: E-string \\U escape
select E'\U000000e9';
-- @todo D-22: folded negative literal with a cast
select -1::int;
-- @todo D-22: repeated unary sign
select - - 1;
-- @todo D-22: unary sign after a binary operator
select 1+-+1;
select -9223372036854775808;
select 2 || 3 + 4;
-- @todo D-22: = NULL is an error instead of no rows
select id from i where v = null;
-- @todo D-22: = ANY(NULL) is false instead of NULL
select 1 = any(null::int[]);
select 'a' || 1 || 'b';
select 3 - -2;
select -(2 + 3);
-- @todo D-53: ^ exponent operator
select 2 ^ 3;
