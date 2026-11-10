-- Date/time, boolean and uuid input forms beyond ISO 8601 (plan D-55),
-- each read the way Postgres reads it. The session time zone is UTC on
-- both sides, so zone-less text means UTC.

-- @setup
set timezone = 'UTC';
create table f (id int primary key, ts timestamptz, d date, b boolean, u uuid);
insert into f values (1, '2024-02-29 12:00', '2024-2-9', 'yes', '{a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11}');
insert into f values (2, '2024-02-29 12:00:00 UTC', '20240229', 'off', 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a12');
insert into f values (3, 'epoch', 'epoch', 't', null);
insert into f values (4, 'infinity', 'infinity', 'n', null);
insert into f values (5, '-infinity', '-infinity', 'on', null);

-- @queries
-- @ordered
select id, ts, d, b, u from f order by id;
-- @ordered
select id from f order by ts, id;
-- @ordered
select id from f order by d desc, id;
select count(*) from f where ts > '2024-02-29 11:59:59' and ts < 'infinity';
select 'y'::boolean, 'fa'::boolean, 'OFF'::boolean, '1'::boolean;
-- @error
select 'maybe'::boolean;
select '2024-02-29 12:00+02'::timestamptz;
