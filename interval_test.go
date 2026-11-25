package bytdb_test

// N-053: the interval column type at the engine level — every accepted
// Go input shape, the type modifier, fields surviving the span-only key
// encoding (primary key and unique index), span order in scans, ALTER
// COLUMN TYPE, DEFAULT backfill, and a reopen. The interval package has
// the Postgres goldens; the SQL and wire layers have their own tests.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/interval"
)

func iv(s string) bytdb.Interval {
	v, err := bytdb.ParseInterval(s)
	if err != nil {
		panic(err)
	}
	return v
}

func ivStr(t *testing.T, r bytdb.Row, col string) string {
	t.Helper()
	v, ok := r.Interval(col)
	if !ok {
		t.Fatalf("column %s is %T (%v), not an Interval", col, r.Col(col), r.Col(col))
	}
	return v.String()
}

func TestIntervalColumnCoercionAndTypmod(t *testing.T) {
	e := openTestEngine(t)
	minute, err := interval.Typmod("minute", -1)
	if err != nil {
		t.Fatal(err)
	}
	prec1, err := interval.Typmod("day to second", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable("t", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt},
		{Name: "raw", Type: bytdb.TInterval},
		{Name: "mins", Type: bytdb.TInterval, IntervalTypmod: minute},
		{Name: "tenths", Type: bytdb.TInterval, IntervalTypmod: prec1},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	// raw keeps what it was given; mins reads a bare number as minutes
	// and drops the seconds; tenths rounds the clock half away from zero
	// to 0.1 s.
	cases := []struct {
		in                any
		raw, mins, tenths string
	}{
		{iv("1 mon 2 days 03:04:05.06"), "1 mon 2 days 03:04:05.06", "1 mon 2 days 03:04:00", "1 mon 2 days 03:04:05.1"},
		{90 * time.Minute, "01:30:00", "01:30:00", "01:30:00"},
		{1500*time.Millisecond + 999, "00:00:01.5", "00:00:00", "00:00:01.5"}, // sub-µs truncated
		{"5", "00:00:05", "00:05:00", "00:00:05"},
		{"-00:00:00.25", "-00:00:00.25", "00:00:00", "-00:00:00.3"},
		{[]byte("P1DT2H"), "1 day 02:00:00", "1 day 02:00:00", "1 day 02:00:00"},
	}
	for i, c := range cases {
		if err := e.Insert("t", i, c.in, c.in, c.in); err != nil {
			t.Fatalf("insert %v: %v", c.in, err)
		}
		r, ok, err := e.Get("t", i)
		if err != nil || !ok {
			t.Fatalf("get %d: %v %v", i, ok, err)
		}
		if got := ivStr(t, r, "raw"); got != c.raw {
			t.Errorf("%v: raw = %q, want %q", c.in, got, c.raw)
		}
		if got := ivStr(t, r, "mins"); got != c.mins {
			t.Errorf("%v: mins = %q, want %q", c.in, got, c.mins)
		}
		if got := ivStr(t, r, "tenths"); got != c.tenths {
			t.Errorf("%v: tenths = %q, want %q", c.in, got, c.tenths)
		}
	}
	// An integer could be any unit, and bad text is Postgres's 22007.
	if err := e.Insert("t", 100, int64(5), nil, nil); err == nil {
		t.Error("an int64 was accepted into an interval column")
	}
	err = e.Insert("t", 101, "1 fortnight", nil, nil)
	if err == nil || !strings.Contains(err.Error(), `invalid input syntax for type interval: "1 fortnight"`) {
		t.Errorf("bad text: %v", err)
	}
	// A modifier is only for interval columns.
	if _, err := e.CreateTable("bad", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt, IntervalTypmod: minute},
	}, "id"); err == nil {
		t.Error("an interval modifier on an int column was accepted")
	}
}

func TestIntervalKeysKeepFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "iv.db")
	e, err := bytdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable("span", []bytdb.Column{
		{Name: "k", Type: bytdb.TInterval},
		{Name: "u", Type: bytdb.TInterval},
		{Name: "note", Type: bytdb.TString},
	}, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndex("span", "span_u", true, "u"); err != nil {
		t.Fatal(err)
	}
	for i, k := range []string{"1 mon", "-1 day", "36:00:00", "1 year", "00:00:00.000001", "29 days 23:00:00"} {
		u := iv(k)
		u.Micros += int64(i + 1) // distinct unique-index values
		if err := e.Insert("span", iv(k), u, k); err != nil {
			t.Fatalf("insert %s: %v", k, err)
		}
	}
	// '30 days' and '720 hours' equal '1 mon' as values, so as keys.
	if err := e.Insert("span", iv("30 days"), iv("5 years"), "dup"); !errors.Is(err, bytdb.ErrDuplicateKey) {
		t.Fatalf("30 days after 1 mon: %v", err)
	}
	if err := e.Insert("span", iv("2 days"), iv("1 day 12:00:00.000003"), "dup"); !errors.Is(err, bytdb.ErrUniqueViolation) {
		t.Fatalf("unique 1 day 12:00:00.000003 after 36:00:00.000003: %v", err)
	}
	check := func(e *bytdb.Engine) {
		t.Helper()
		var got []string
		for r, err := range e.Scan("span") {
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, ivStr(t, r, "k"))
		}
		// Span order; each key reads back with the fields it was written
		// with, not its justified form.
		want := "-1 days|00:00:00.000001|36:00:00|29 days 23:00:00|1 mon|1 year"
		if strings.Join(got, "|") != want {
			t.Errorf("scan = %v, want %s", got, want)
		}
		// A point lookup by an equal span finds the row and reports the
		// stored fields.
		r, ok, err := e.Get("span", iv("720 hours"))
		if err != nil || !ok || ivStr(t, r, "k") != "1 mon" {
			t.Errorf("Get(720 hours) = %v %v %v", r.Vals, ok, err)
		}
		// An index range scan resolves entries through their decoded
		// primary keys (key form) back to rows with the stored fields.
		var keys []string
		for r, err := range e.ScanIndex("span", "span_u", []any{iv("1 day")}, []any{iv("1 mon")}) {
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, ivStr(t, r, "k"))
		}
		if strings.Join(keys, "|") != "36:00:00|29 days 23:00:00" {
			t.Errorf("index range = %v", keys)
		}
	}
	check(e)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if e, err = bytdb.Open(path); err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	check(e)
}

func TestIntervalAlterAndDefault(t *testing.T) {
	e := openTestEngine(t)
	if _, err := e.CreateTable("a", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt},
		{Name: "d", Type: bytdb.TInterval, Default: "'1 day 02:03:04.5'"},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("a", 1, iv("-1 mon +3 days 00:00:01")); err != nil {
		t.Fatal(err)
	}
	// A new column's DEFAULT backfills existing rows.
	if err := e.AddColumn("a", bytdb.Column{Name: "e", Type: bytdb.TInterval, Default: "'P2W'"}); err != nil {
		t.Fatal(err)
	}
	r, _, _ := e.Get("a", 1)
	if got := ivStr(t, r, "e"); got != "14 days" {
		t.Errorf("backfilled default = %q", got)
	}
	// Narrowing to interval hour truncates every row and the default.
	hour, _ := interval.Typmod("hour", -1)
	if err := e.AlterColumnType("a", "d", bytdb.ColumnTypeChange{Type: bytdb.TInterval, IntervalTypmod: hour}); err != nil {
		t.Fatal(err)
	}
	r, _, _ = e.Get("a", 1)
	if got := ivStr(t, r, "d"); got != "-1 mons +3 days" {
		t.Errorf("after interval hour: %q", got)
	}
	// interval -> text is an automatic cast; the default comes along.
	if err := e.AlterColumnType("a", "d", bytdb.ColumnTypeChange{Type: bytdb.TString}); err != nil {
		t.Fatal(err)
	}
	r, _, _ = e.Get("a", 1)
	if s, _ := r.String("d"); s != "-1 mons +3 days" {
		t.Errorf("as text: %q", s)
	}
	// text -> interval needs USING, as in Postgres.
	if err := e.AlterColumnType("a", "d", bytdb.ColumnTypeChange{Type: bytdb.TInterval}); err == nil {
		t.Error("text -> interval without USING was accepted")
	}
}
