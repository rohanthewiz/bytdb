-- Lexer and parser divergences (plan D-22).

-- @setup
create table i (id int primary key, v int);
insert into i values (1, 10), (2, null);

-- @queries
select 0x1F;
select 0o17;
select 0b101;
select 1_000;
-- @error
select 1a;
select E'\x41';
select E'\101';
select E'é';
select E'\U000000e9';
select -1::int;
select - - 1;
select 1+-+1;
select -9223372036854775808;
-- @todo D-23: integer || integer is accepted; Postgres has no such operator
select 2 || 3 + 4;
select 'a' || 3 + 4;
select 'x' || 1 || 2;
select id from i where v = null;
select 1 = any(null::int[]);
select 'a' || 1 || 'b';
select 3 - -2;
select -(2 + 3);
-- @todo D-53: ^ exponent operator
select 2 ^ 3;
select 0x7FFF_FFFF_FFFF_FFFF;
select 1_000.000_5;
select 0x_ff;
-- @error
select 1__000;
-- @error
select 0x;
-- @error
select 1_;
select E'café';
-- @error
select E'\xff';
select -0x10;
select 10 - -0b11;
