-- Upsert that changes a referenced key (plan D-10).

-- @setup
create table p (id int primary key, name text);
create table ch (id int primary key, pid int references p(id));
insert into p values (1, 'a');
insert into ch values (1, 1);

-- @queries
-- @error 23503
-- @todo D-10: upsert changing a referenced key skips the parent-side FK check
insert into p values (1, 'x') on conflict (id) do update set id = 100;
-- @todo D-10: depends on the upsert above
select id from p;
