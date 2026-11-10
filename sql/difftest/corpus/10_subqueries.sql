-- Subqueries (plan D-09, D-27, D-28, D-21).

-- @setup
create table o (id int primary key, cust int, amt int);
insert into o values (1, 1, 10), (2, 1, 50), (3, 2, 30), (4, 2, 5), (5, 3, 99);

-- @queries
-- @todo D-27: subquery ORDER BY/LIMIT ignored; result typed text
select (select amt from o order by amt desc limit 1);
-- @todo D-27: subquery LIMIT ignored
select id from o where id in (select id from o order by amt desc limit 2);
-- @todo D-25: exists() described as text
select exists (select 1 from o limit 0);
-- @todo D-27: subquery OFFSET ignored
select (select amt from o order by amt limit 1 offset 1);
-- @todo D-25: coalesce over a scalar subquery described as text
select coalesce((select sum(amt) from o where cust = 9), 0);
-- @todo D-27: aggregate inside a subquery's expression
select (select coalesce(sum(amt), 0) from o where cust = 9);
-- @todo D-27: count(DISTINCT) inside a scalar subquery
select (select count(distinct cust) from o);
-- @todo D-27: expression over an aggregate inside a scalar subquery
select (select count(*) + 1 from o);
-- @todo D-25: exists() described as text (its value is right since D-21)
select exists (select count(*) from o where false);
-- latest row per customer, correlated
-- @todo D-27: correlated subquery ORDER BY/LIMIT; result typed text
select o1.cust, (select o2.amt from o o2 where o2.cust = o1.cust order by o2.id desc limit 1) from o o1 where o1.id in (1, 3, 5);
-- @todo D-27: GROUP BY/HAVING inside IN (subquery)
select id from o where cust in (select cust from o group by cust having count(*) > 1);
-- @todo D-27: aggregate inside IN (subquery)
select id from o where amt in (select max(amt) from o);
-- @todo D-27: UNION inside IN (subquery)
select id from o where id in (select 1 union select 2);
-- @todo D-27: GROUP BY inside EXISTS
select exists (select cust from o group by cust);
select id from o where amt > (select avg(amt) from o);
select count(*) from o where id not in (select cust from o);
