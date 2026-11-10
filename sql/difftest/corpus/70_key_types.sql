-- Key lookups and ranges on uuid, timestamp and date columns (plan
-- D-37). These used to be full scans; now they push into point gets and
-- bounded index scans, which must return exactly what Postgres does,
-- including at NULLs, the epoch boundary, and through cast literals.

-- @setup
create table ev (id uuid primary key, at timestamptz, day date, n int);
create index ev_at on ev (at);
create index ev_day on ev (day);
insert into ev values
  ('00000000-0000-4000-8000-000000000001', '1969-12-31 23:59:59+00', '1969-12-31', 1),
  ('00000000-0000-4000-8000-000000000002', '1970-01-01 00:00:00+00', '1970-01-01', 2),
  ('7fffffff-0000-4000-8000-000000000003', '2024-01-01 00:00:00+00', '2024-01-01', 3),
  ('80000000-0000-4000-8000-000000000004', '2024-01-01 00:00:01+00', '2024-01-02', 4),
  ('ffffffff-ffff-4fff-bfff-ffffffffffff', null, null, 5);
create table child (id int primary key, pid uuid references ev (id));
insert into child values (1, '7fffffff-0000-4000-8000-000000000003'), (2, '00000000-0000-4000-8000-000000000001');

-- @queries
select n from ev where id = '7fffffff-0000-4000-8000-000000000003';
select n from ev where id = '80000000-0000-4000-8000-000000000004'::uuid;
select n from ev where '00000000-0000-4000-8000-000000000002'::uuid = id;
-- @ordered
select n from ev where id > '7fffffff-0000-4000-8000-000000000003' order by n;
-- @ordered
select n from ev where id <= '7fffffff-0000-4000-8000-000000000003' order by id desc;
select n from ev where id = '00000000-0000-4000-8000-0000000000ff';
-- @ordered
select n from ev where at >= '2024-01-01' order by n;
-- @ordered
select n from ev where at < '1970-01-01 00:00:00+00'::timestamptz order by n;
-- @ordered
select n from ev where at > '1969-12-31 23:59:59+00' and at <= '2024-01-01 00:00:00+00' order by n;
select n from ev where day = '2024-01-02';
-- @ordered
select n from ev where day between '1969-12-31'::date and '1970-01-01'::date order by n;
-- @ordered
select n from ev where day < '2024-01-01 12:00:00+00'::timestamptz order by n;
-- @ordered
select c.id, e.n from child c join ev e on e.id = c.pid order by c.id;
-- @error
insert into child values (3, '00000000-0000-4000-8000-0000000000ff');
