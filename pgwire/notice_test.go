package pgwire_test

// Statement notices over the wire: the severity and SQLSTATE each kind
// arrives with, and one NoticeResponse per line.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestNoticeSeverities runs every kind of statement notice over pgx and
// checks each arrives with Postgres's severity and SQLSTATE: the
// IF [NOT] EXISTS skips as NOTICE, VACUUM's non-table skip as WARNING,
// and VACUUM VERBOSE's report as INFO, as a message of its own.
func TestNoticeSeverities(t *testing.T) {
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(startServer(t))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var notices []*pgconn.Notice
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		mu.Lock()
		notices = append(notices, n)
		mu.Unlock()
	}
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)

	mustExec(t, c, `create table t (id int primary key, a int)`)
	mustExec(t, c, `create view v as select id from t`)
	mustExec(t, c, `create index t_a on t (a)`)
	mustExec(t, c, `create sequence s`)
	mustExec(t, c, `insert into t values (1, 1), (2, 2)`)
	mustExec(t, c, `delete from t where id = 2`)

	for _, tc := range []struct {
		sql  string
		want []string // "SEVERITY CODE message", in order; a message ending in "…" is a prefix
	}{
		{`create table if not exists t (id int primary key)`,
			[]string{`NOTICE 42P07 relation "t" already exists, skipping`}},
		{`create index if not exists t_a on t (a)`,
			[]string{`NOTICE 42P07 relation "t_a" already exists, skipping`}},
		{`create sequence if not exists s`,
			[]string{`NOTICE 42P07 relation "s" already exists, skipping`}},
		{`drop sequence if exists ghost`,
			[]string{`NOTICE 00000 sequence "ghost" does not exist, skipping`}},
		{`alter table t drop constraint if exists ghost`,
			[]string{`NOTICE 00000 constraint "ghost" of relation "t" does not exist, skipping`}},
		{`alter table t add column if not exists a int`,
			[]string{`NOTICE 42701 column "a" of relation "t" already exists, skipping`}},
		{`drop table if exists ghost`,
			[]string{`NOTICE 00000 table "ghost" does not exist, skipping`}},
		{`drop index if exists ghost`,
			[]string{`NOTICE 00000 index "ghost" does not exist, skipping`}},
		{`drop view if exists ghost`,
			[]string{`NOTICE 00000 view "ghost" does not exist, skipping`}},
		{`vacuum v`,
			[]string{`WARNING 01000 skipping "v" --- cannot vacuum non-tables or special system tables`}},
		{`vacuum verbose v, t`, []string{
			`WARNING 01000 skipping "v" --- cannot vacuum non-tables or special system tables`,
			`INFO 00000 compacted the storage log: …`,
		}},
		{`vacuum t`, nil},
	} {
		mu.Lock()
		notices = nil
		mu.Unlock()
		mustExec(t, c, tc.sql)

		mu.Lock()
		got := notices
		mu.Unlock()
		if len(got) != len(tc.want) {
			t.Fatalf("%s: %d notices %v; want %d", tc.sql, len(got), got, len(tc.want))
		}
		for i, n := range got {
			line := n.Severity + " " + n.Code + " " + n.Message
			if want, ok := strings.CutSuffix(tc.want[i], "…"); ok {
				if !strings.HasPrefix(line, want) {
					t.Fatalf("%s: notice %d = %q; want prefix %q", tc.sql, i, line, want)
				}
			} else if line != tc.want[i] {
				t.Fatalf("%s: notice %d = %q; want %q", tc.sql, i, line, tc.want[i])
			}
			// The localization-proof severity must match the localized one.
			if n.SeverityUnlocalized != n.Severity {
				t.Fatalf("%s: V field %q; S field %q", tc.sql, n.SeverityUnlocalized, n.Severity)
			}
		}
	}
}
