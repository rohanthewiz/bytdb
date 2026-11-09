package sqlfrontaudit

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

func sx(t *testing.T, s *bsql.Session, q string, args ...any) {
	t.Helper()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("[%c] %-60s => PANIC %v", s.Status(), q, r)
			}
		}()
		res, err := s.Exec(q, args...)
		extra := ""
		if res != nil && (res.Notice != "" || res.Tag != "") {
			extra = " notice=" + res.Notice + " tag=" + res.Tag
		}
		t.Logf("%-62s => %s%s  [status now %c]", q, show(res, err), extra, s.Status())
	}()
}

func TestSessionProbes(t *testing.T) {
	d := openDB(t)
	must(t, d, `create table t (id int primary key, v text)`)
	must(t, d, `create sequence sq`)
	s := d.NewSession()
	defer s.Close()

	t.Log("--- syntax error inside a block: does the block abort?")
	sx(t, s, `begin`)
	sx(t, s, `insert into t values (1, 'a')`)
	sx(t, s, `insrt into t values (2, 'b')`)
	sx(t, s, `insert into t values (3, 'c')`)
	sx(t, s, `commit`)
	sx(t, s, `select id from t order by id`)
	must(t, d, `delete from t`)

	t.Log("--- runtime error inside a block")
	sx(t, s, `begin`)
	sx(t, s, `insert into t values (1, 'a')`)
	sx(t, s, `select 1/0`)
	sx(t, s, `select 1`)
	sx(t, s, `set statement_timeout = 5`)
	sx(t, s, `show server_version`)
	sx(t, s, `begin`)
	sx(t, s, `savepoint x`)
	sx(t, s, `commit`)
	sx(t, s, `select count(*) from t`)

	t.Log("--- savepoints")
	sx(t, s, `begin`)
	sx(t, s, `insert into t values (1, 'a')`)
	sx(t, s, `savepoint a`)
	sx(t, s, `insert into t values (2, 'b')`)
	sx(t, s, `savepoint a`)
	sx(t, s, `insert into t values (3, 'c')`)
	sx(t, s, `rollback to a`)
	sx(t, s, `select id from t order by id`)
	sx(t, s, `release a`)
	sx(t, s, `rollback to a`)
	sx(t, s, `select id from t order by id`)
	sx(t, s, `rollback to savepoint nope`)
	sx(t, s, `rollback to a`)
	sx(t, s, `select id from t order by id`)
	sx(t, s, `commit`)
	sx(t, s, `select id from t order by id`)
	must(t, d, `delete from t`)

	t.Log("--- sequences across rollback, currval")
	sx(t, s, `begin`)
	sx(t, s, `select nextval('sq')`)
	sx(t, s, `rollback`)
	sx(t, s, `select currval('sq')`)
	sx(t, s, `select nextval('sq')`)
	sx(t, s, `select coalesce(1, nextval('sq'))`)
	sx(t, s, `select nextval('sq')`)

	t.Log("--- read only block")
	sx(t, s, `begin read only`)
	sx(t, s, `select nextval('sq')`)
	sx(t, s, `rollback`)
	sx(t, s, `begin read only`)
	sx(t, s, `insert into t values (9, 'z')`)
	sx(t, s, `rollback`)
	sx(t, s, `begin transaction isolation level serializable, read write, not deferrable`)
	sx(t, s, `set transaction isolation level read committed`)
	sx(t, s, `show transaction isolation level`)
	sx(t, s, `create table zz (id int primary key)`)
	sx(t, s, `rollback`)

	t.Log("--- SET/SHOW")
	sx(t, s, `set statement_timeout = '1500ms'`)
	sx(t, s, `show statement_timeout`)
	sx(t, s, `set statement_timeout to '1 s'`)
	sx(t, s, `show statement_timeout`)
	sx(t, s, `set statement_timeout = '0.4ms'`)
	sx(t, s, `show statement_timeout`)
	sx(t, s, `select 1`)
	sx(t, s, `set statement_timeout = 99999999999999999999`)
	sx(t, s, `set statement_timeout = 1e30`)
	sx(t, s, `show statement_timeout`)
	sx(t, s, `select 1`)
	sx(t, s, `set statement_timeout = '-'`)
	sx(t, s, `reset statement_timeout`)
	sx(t, s, `set application_name = 'app'`)
	sx(t, s, `show application_name`)
	sx(t, s, `set search_path = app`)
	sx(t, s, `select count(*) from t`)
	sx(t, s, `show search_path`)
	sx(t, s, `set time zone 'America/New_York'`)
	sx(t, s, `show timezone`)
	sx(t, s, `select '2024-01-01 12:00:00'::timestamp`)
	sx(t, s, `set datestyle = 'German'`)
	sx(t, s, `set bogus_param = 1`)
	sx(t, s, `show bogus_param`)
	sx(t, s, `set local statement_timeout = 1`)
	sx(t, s, `show statement_timeout`)
	sx(t, s, `reset all`)
	sx(t, s, `show all`)

	t.Log("--- timeout inside a block leaves what?")
	must(t, d, `create table big (id int primary key, v int)`)
	for i := 0; i < 20; i++ {
		q := `insert into big values `
		for j := 0; j < 1000; j++ {
			if j > 0 {
				q += ","
			}
			q += "(" + itoa(i*1000+j) + ", 1)"
		}
		must(t, d, q)
	}
	sx(t, s, `set statement_timeout = 1`)
	sx(t, s, `begin`)
	sx(t, s, `update big set v = v + 1`)
	sx(t, s, `select 1`)
	sx(t, s, `rollback`)
	sx(t, s, `update big set v = v + 1 where id in (select b1.id from big b1, big b2 where b2.v > b1.v + 100)`)
	sx(t, s, `reset statement_timeout`)
	sx(t, s, `select sum(v), count(*) from big`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	_, err := s.ExecCtx(ctx, `update big set v = v + 1`)
	cancel()
	t.Logf("ExecCtx 1ms update => %v  [status %c]", err, s.Status())
	sx(t, s, `select sum(v), count(*) from big`)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

// Non-idempotent statement replay under OCC autocommit retry.
func TestOCCRetryProbe(t *testing.T) {
	d := openDB(t, bytdb.WithConcurrentWrites())
	must(t, d, `create table c (id int primary key, n int)`)
	must(t, d, `insert into c values (1, 0)`)
	must(t, d, `create sequence sq`)
	done := make(chan struct{})
	var errs atomic.Int64
	const workers, per = 8, 300
	for w := 0; w < workers; w++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < per; i++ {
				if _, err := d.Exec(`update c set n = n + 1 where id = 1`); err != nil {
					errs.Add(1)
				}
			}
		}()
	}
	for w := 0; w < workers; w++ {
		<-done
	}
	res := must(t, d, `select n from c`)
	t.Logf("n = %v, surfaced conflict errors = %d, n+errs = %d (want %d)", res.Rows[0][0], errs.Load(), res.Rows[0][0].(int64)+errs.Load(), workers*per)
}
