package sqlfrontaudit

import "testing"

func TestSubqueryLiteralCoercion(t *testing.T) {
	d := setup(t)
	must(t, d, `create index t_d on t (d)`)
	must(t, d, `create view v as select id from t where d = '2024-02-29'`)
	must(t, d, `create view v2 as select id, d, ts, u from t`)
	// Every query below returns one row in Postgres.
	for _, q := range []string{
		`select id from t where exists (select 1 from t t2 where t2.ts > '2024-01-01')`,
		`select id from t where id in (select id from t where d = '2024-02-29')`,
		`select id from t where id = (select id from t where u = 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11')`,
		`with q as (select id from t where d = '2024-02-29') select id from q`,
		`select id from (select id from t where ts >= '2024-02-29' and ts < '2024-03-01') q`,
		`select id from (select id, d from t) q where d = '2024-02-29'`,
		`with q as (select id, ts, u from t) select id from q where u = 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'`,
		`select id from v`,
		`select id from v2 where d = '2024-02-29'`,
		`select id from v2 where u = 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'`,
		`select t.id from t join t t2 on t2.id = t.id and t2.d = '2024-02-29'`,
		`select t.id from t left join t t2 on t2.id = t.id where t2.d = '2024-02-29' or t2.d = '2020-01-01'`,
		`select id from t where d = '2024-02-29' or ts = '2020-01-01 00:00:00'`,
		`select id from t where not (d <> '2024-02-29')`,
		`select id from t where d between '2024-02-01' and '2024-03-01'`,
		`select id from t where d = '2024-02-29'::date`,
		`select id from t where (d = '2024-02-29') = true`,
		`select id from t where (d = '2024-02-29') is not null`,
		`select id from t where case when d = '2024-02-29' then true else false end`,
		`select id from t group by id, d having d = '2024-02-29'`,
		`select id from t group by id having max(d) = '2024-02-29'`,
		`select id from t where u <> '00000000-0000-0000-0000-000000000000' and u is not null`,
		`select id from t where '2024-02-29' = d`,
		`select id from t where d = any(array['2024-02-29'])`,
		`select count(*) from t where d >= '2024-01-01' group by d having count(*) >= 1`,
	} {
		want(t, d, "[[1]]", q)
	}
	want(t, d, "[[1]]", `update t set i = 11 where id in (select id from t where d = '2024-02-29') returning id`)
	want(t, d, "[[1]]", `delete from t where exists (select 1 from t t2 where t2.u = 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11') returning id`)
}
