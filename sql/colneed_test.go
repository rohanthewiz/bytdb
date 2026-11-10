package sql

// Deep-look D-39: SELECT scans decode only the columns the statement
// names. A column the name walk missed would read as NULL, so every
// query here runs with the masks on and off and must agree.

import (
	"fmt"
	"testing"
)

func TestColumnMasksDoNotChangeAnswers(t *testing.T) {
	d := openDB(t)
	for _, q := range []string{
		`create table o (id int primary key, cust int not null, amount float, status text, note text, doc jsonb)`,
		`create index o_cust on o (cust)`,
		`create index o_status on o (status desc, amount)`,
		`create table c (id int primary key, name text, region int)`,
		`create view vo as select id, note, status from o`,
	} {
		exec(t, d, q)
	}
	for i := 1; i <= 40; i++ {
		exec(t, d, fmt.Sprintf(`insert into o values (%d, %d, %d.25, 's%d', 'note %d', '{"k": %d}')`, i, 1+i%5, i, i%3, i, i%4))
	}
	for i := 1; i <= 5; i++ {
		exec(t, d, fmt.Sprintf(`insert into c values (%d, 'c%d', %d)`, i, i, i%2))
	}
	queries := []string{
		`select count(*), sum(amount) from o`,
		`select count(note) from o`,
		`select * from o order by id limit 3`,
		`select o.* from o join c on c.id = o.cust where c.region = 1 order by o.id`,
		`select id from o where status = 's1' order by amount desc`,
		`select id, status from o where cust in (2, 3) order by id`,
		`select id from o order by note limit 4`,
		`select status, count(*), max(amount) from o group by status order by status`,
		`select status, sum(amount) from o group by 1 having count(note) > 2 order by 1`,
		`select id, row_number() over (partition by status order by amount desc) from o order by id`,
		`select id, sum(amount) over (order by note) from o order by id`,
		`select id from o where exists (select 1 from c where c.id = o.cust and c.name = 'c2') order by id`,
		`select id, (select name from c where c.id = o.cust) from o order by id limit 5`,
		`select id from o where amount > (select avg(amount) from o o2 where o2.cust = o.cust) order by id`,
		`select note from vo where id = 7`,
		`select count(*) from vo where status = 's2'`,
		`with x as (select * from o where cust = 1) select id, note from x order by id`,
		`with x as (select id, note from o) select count(*) from x where note like 'note 1%'`,
		`select cust from o where id < 5 union select region from c order by 1`,
		`select doc->>'k', count(*) from o group by 1 order by 1`,
		`select c.name, sum(o.amount) from c join o on o.cust = c.id group by c.name order by c.name`,
		`select c.name, o.note from c left join o on o.cust = c.id and o.id < 3 order by c.name, o.note`,
		`select distinct status from o order by status`,
		`select id from o where note = 'note 9' or status = 's0' order by id`,
		`select id from o order by status desc, amount limit 5`,
		`select max(note), min(doc::text) from o`,
	}
	run := func(masks bool) []string {
		useColumnMasks = masks
		defer func() { useColumnMasks = true }()
		out := make([]string, len(queries))
		for i, q := range queries {
			out[i] = fmt.Sprint(exec(t, d, q).Rows)
		}
		return out
	}
	with, without := run(true), run(false)
	for i, q := range queries {
		if with[i] != without[i] {
			t.Errorf("%s\n with masks    %s\n without masks %s", q, with[i], without[i])
		}
	}
	// And the masks do engage: an aggregate over one column decodes
	// nothing else.
	fp, err := prepareFrom(d.lookup(d.e.TableShared), []FromItem{{Table: "o"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := Parse(`select sum(amount) from o`)
	if err != nil {
		t.Fatal(err)
	}
	fp.readColumnsOf(st.(*Select))
	if need := fp.steps[0].needCols(); fmt.Sprint(need) != "[false false true false false false]" {
		t.Fatalf("mask for sum(amount) = %v", need)
	}
}
