-- money (N-053): int64 cents with Postgres's en_US text ($1,234.56),
-- input rounding, keys, the operators (money × float rounds to cents,
-- money / money is float8), casts, sum/min/max, cash_words and errors.
-- The Postgres side runs with lc_monetary en_US.utf8, its docker
-- default; bytdb's money text is fixed to that form.

-- @setup
create table acct (id bigint primary key, bal money, fee money default 2.5);
insert into acct values (1, '$1,234.56', '0.10'), (2, '-7', '(1.005)'), (3, 5, 1.255), (4, null, null), (5, '92233720368547758.07', '-92233720368547758.08');
insert into acct (id, bal) values (6, '12.34');
insert into acct (id, bal) select 7, 3;
create table price (p money primary key, n int unique);
insert into price values ('$10.00', 1), ('-0.01', 2), ('0', 3), ('99.999', 4), ('1,000', 5);

-- @queries
-- @ordered
select id, bal, fee from acct order by id;
-- @ordered
select p, n from price order by p;
-- @ordered
select p, n from price order by p desc;

-- Input forms and output.
select '1.005'::money, '-1.005'::money, '1.0049'::money, '($1,000.00)'::money, '1.00-'::money, '  -  $ 1 '::money, ''::money, '.5'::money, '1,2,3'::money;
select money '12.34', '-92233720368547758.08'::money, 0::money, (-5)::money, 1.255::money, (-1.255)::numeric::money;

-- Comparison and keys.
select '1'::money = '1.00', '1.01'::money > '$1.00', '-1'::money < '0';
-- @ordered
select n from price where p > '$0.00' order by n;
-- @ordered
select n from price where p between '-1' and '10' order by n;
-- @ordered
select n from price where p = '1000' order by n;
select count(distinct bal) from acct;

-- Arithmetic.
select '5'::money + '1', '5'::money - '1.50', '5'::money * 2, 2 * '1.5'::money, '7'::money / 2, '-7'::money / 2, '0.07'::money / 2;
select '0.05'::money * 0.5, '0.03'::money * 0.5, 0.5 * '0.05'::money, '1'::money * 1.005, '10'::money / 3.0, '1'::money / 3, '5'::money * '2', '5'::money / '2';
select '1'::money / '3'::money, '5'::money / '$2.00';
-- @ordered
select id, bal + fee, bal - fee, bal * 3, bal / 4, bal * 1.5, bal / 0.5 from acct where id <> 5 order by id;

-- Aggregates.
select sum(bal), min(bal), max(bal), count(bal) from acct where id <> 5;
-- @ordered
select n % 2, sum(p), min(p), max(p) from price group by n % 2 order by 1;

-- Beside a window function (the select list is rewritten for it).
-- @ordered
select id, bal * 1.5, bal / '2', sum(bal) over (order by id) from acct where id < 5 order by id;

-- Casts and functions.
select '1.23'::money::numeric, '-92233720368547758.08'::money::numeric, '1'::money::numeric(10,1), '7.5'::money::text, 12.345::numeric::money;
select cash_words('123.45'::money), cash_words('1000000.99'::money), cash_words('-1.01'::money), cash_words('0'::money), cash_words('100105.15'::money);

-- Catalog.
-- @ordered
select a.attname::text, format_type(a.atttypid, a.atttypmod) from pg_attribute a join pg_class c on c.oid = a.attrelid where c.relname = 'acct' and a.attnum > 0 order by a.attnum;

-- Errors.
-- @error 22P02
select 'abc'::money;
-- @error 22P02
select '1.2.3'::money;
-- @error 22003
select '92233720368547758.08'::money;
-- @error 22003
select '92233720368547758.07'::money + '0.01';
-- @error 22003
select '92233720368547758.07'::money * 2;
-- @error 22003
select '1'::money * 'nan'::float8;
-- @error 22012
select '10'::money / 0;
-- @error 22012
select '10'::money / '0'::money;
-- @error 42883
select '1'::money + 1;
-- @error 42883
select '1'::money < 2;
-- @error 42883
select -'1'::money;
-- @error 42883
select avg(bal) from acct;
-- @error 42846
select '1'::money::float8;
-- @error 42846
select 1.5::float8::money;
-- @error 23505
insert into price values ('10', 9);
