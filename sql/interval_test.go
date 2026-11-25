package sql

// N-053: interval through the SQL layer, for what the difftest corpus
// (corpus/81_interval.sql, checked against Postgres 16) does not reach:
// type modifiers applied to INSERT, UPDATE and DEFAULT text, ALTER
// COLUMN TYPE with and without USING, Go Interval parameters, EXPLAIN's
// rendering of an interval bound, result typing, and named-argument
// errors.

import (
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
)

// ivText renders one result cell: an Interval as its Postgres text.
func ivText(t *testing.T, v any) string {
	t.Helper()
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bytdb.Interval:
		return x.String()
	}
	t.Fatalf("cell is %T (%v), not an interval", v, v)
	return ""
}

func TestIntervalTypmodWrites(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table m (id int primary key, mins interval minute default '5', ym interval year to month, p interval(1))`)
	// A bare number reads in the modifier's last unit, from VALUES, from
	// UPDATE SET, and from the DEFAULT text alike.
	exec(t, d, `insert into m (id, ym, p) values (1, '7', '00:00:01.25')`)
	exec(t, d, `insert into m values (2, '90', '1-6', '-00:00:00.25')`)
	exec(t, d, `update m set mins = '15', ym = '30' where id = 1`)
	exec(t, d, `alter table m add column later interval hour default '2'`)
	res := exec(t, d, `select mins, ym, p, later from m order by id`)
	var got []string
	for _, row := range res.Rows {
		var cells []string
		for _, v := range row {
			cells = append(cells, ivText(t, v))
		}
		got = append(got, strings.Join(cells, ", "))
	}
	want := "00:15:00, 2 years 6 mons, 00:00:01.3, 02:00:00 | 01:30:00, 1 year 6 mons, -00:00:00.3, 02:00:00"
	if strings.Join(got, " | ") != want {
		t.Errorf("rows = %q\nwant   %q", strings.Join(got, " | "), want)
	}
	for i, typ := range res.Types {
		if typ != bytdb.TInterval {
			t.Errorf("column %d typed %q", i, typ)
		}
	}
	// A literal that does not read is the statement's error, at bind.
	if _, err := d.Exec(`insert into m (id, mins) values (3, 'often')`); err == nil ||
		!strings.Contains(err.Error(), `invalid input syntax for type interval: "often"`) {
		t.Errorf("bad text: %v", err)
	}
}

func TestIntervalAlterUsing(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table a (id int primary key, secs int, txt text)`)
	exec(t, d, `insert into a values (1, 90, '1 day 02:30'), (2, -5, '3 weeks')`)
	// text -> interval needs USING; with it, the text parses.
	if _, err := d.Exec(`alter table a alter column txt type interval`); err == nil {
		t.Error("text -> interval without USING was accepted")
	}
	exec(t, d, `alter table a alter column txt type interval using txt::interval`)
	exec(t, d, `alter table a alter column secs type interval using make_interval(secs => secs)`)
	// Narrowing the modifier with an interval cast in USING.
	exec(t, d, `alter table a alter column txt type interval day using txt::interval day`)
	res := exec(t, d, `select secs, txt from a order by id`)
	got := ivText(t, res.Rows[0][0]) + ", " + ivText(t, res.Rows[0][1]) + " | " +
		ivText(t, res.Rows[1][0]) + ", " + ivText(t, res.Rows[1][1])
	if want := "00:01:30, 1 day | -00:00:05, 21 days"; got != want {
		t.Errorf("rows = %q, want %q", got, want)
	}
	res = exec(t, d, `select format_type(atttypid, atttypmod) from pg_attribute a join pg_class c on c.oid = a.attrelid where c.relname = 'a' and attname = 'txt'`)
	if got := res.Rows[0][0]; got != "interval day" {
		t.Errorf("format_type = %v", got)
	}
}

func TestIntervalParamsAndTyping(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table ev (id int primary key, at timestamptz, dur interval)`)
	at := time.Date(2024, 1, 31, 10, 0, 0, 0, time.UTC)
	// A Go Interval binds directly, as does interval text.
	if _, err := d.Exec(`insert into ev values ($1, $2, $3)`, 1, at, bytdb.Interval{Months: 1, Days: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`insert into ev values ($1, $2, $3)`, 2, at, "36 hours"); err != nil {
		t.Fatal(err)
	}
	res, err := d.Exec(`select id, at + dur, dur * 2, at - $1::timestamptz from ev where dur > $2 order by id`,
		at.Add(-time.Hour), bytdb.Interval{Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := []bytdb.ColType{bytdb.TInt, bytdb.TTimestamp, bytdb.TInterval, bytdb.TInterval}
	for i, typ := range res.Types {
		if typ != want[i] {
			t.Errorf("column %d typed %q, want %q", i, typ, want[i])
		}
	}
	if len(res.Rows) != 2 {
		t.Fatalf("rows: %v", res.Rows)
	}
	// 2024-01-31 10:00 + 1 mon 1 day is Feb 29 (clamped) + 1 day.
	if got := bytdb.FormatTimestamp(res.Rows[0][1].(int64)); got != "2024-03-01 10:00:00+00" {
		t.Errorf("at + dur = %s", got)
	}
	if got := ivText(t, res.Rows[1][2]); got != "72:00:00" {
		t.Errorf("dur * 2 = %s", got)
	}
	if got := ivText(t, res.Rows[0][3]); got != "01:00:00" {
		t.Errorf("at - param = %s", got)
	}
}

func TestIntervalExplainAndErrors(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table k (k interval primary key, n int)`)
	res := exec(t, d, `explain select n from k where k = '720 hours'`)
	var plan strings.Builder
	for _, row := range res.Rows {
		plan.WriteString(row[0].(string) + "\n")
	}
	if !strings.Contains(plan.String(), "'720:00:00'::interval") {
		t.Errorf("explain does not render the interval bound:\n%s", plan.String())
	}
	for q, want := range map[string]string{
		`select make_interval(fortnights => 1)`:        `function make_interval has no parameter named "fortnights"`,
		`select make_interval(days => 1, days => 2)`:   `argument name "days" used more than once`,
		`select make_interval(days => 1, 2)`:           `positional argument cannot follow named argument`,
		`select make_interval(1, 2, 3, 4, 5, 6, 7, 8)`: `function make_interval takes at most 7 arguments`,
		`select now() + 1`:                             `operator does not exist`,
		`select interval '1 day' - now()`:              `operator does not exist: interval - timestamp with time zone`,
		`select sum(txt) from (select 'x' as txt) s`:   ``, // an error of its own; only checked to fail
	} {
		_, err := d.Exec(q)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", q, err, want)
		}
	}
}

func TestIntervalStyleSetting(t *testing.T) {
	d := openDB(t)
	exec(t, d, `set intervalstyle = postgres`)
	exec(t, d, `set intervalstyle to 'Postgres'`)
	if _, err := d.Exec(`set intervalstyle = iso_8601`); err == nil ||
		!strings.Contains(err.Error(), `invalid value for parameter "intervalstyle"`) {
		t.Errorf("iso_8601: %v", err)
	}
	exec(t, d, `reset intervalstyle`)
}

// TestIntervalInWindowedSelect: a window function in the select list
// sends the items through rewriteWindows, which used to rebuild every
// arithmetic node without its typed operator kind — so ts - ts beside
// count(*) over () came back as bigint microseconds.
func TestIntervalInWindowedSelect(t *testing.T) {
	d := openDB(t)
	exec(t, d, `create table w (id int primary key, ts timestamptz)`)
	exec(t, d, `insert into w values (1, '2024-01-02 00:00+00')`)
	res := exec(t, d, `select ts - '2024-01-01 00:00+00'::timestamptz, count(*) over () from w`)
	if got := ivText(t, res.Rows[0][0]); got != "1 day" {
		t.Errorf("windowed ts - ts = %s", got)
	}
}
