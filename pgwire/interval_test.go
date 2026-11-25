package pgwire_test

// N-053 over the wire: interval columns, parameters and results through
// pgx — binary parameters and results (pgx's default for a known OID),
// text results, and the simple protocol — plus the error codes and the
// catalog.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestIntervalOverWire(t *testing.T) {
	ctx := context.Background()
	c := connect(t, startServer(t))
	mustExec(t, c, `create table iv (id int primary key, v interval, m interval minute)`)

	// Parameters of each shape pgx encodes for an interval placeholder:
	// pgtype.Interval and time.Duration go binary, a string goes as text.
	mustExec(t, c, `insert into iv values ($1, $2, $3)`, 1,
		pgtype.Interval{Months: 14, Days: -3, Microseconds: 3_723_000_001, Valid: true}, "90")
	mustExec(t, c, `insert into iv values ($1, $2, $3)`, 2, 36*time.Hour+500*time.Millisecond, "1 day 00:00:59")
	mustExec(t, c, `insert into iv values ($1, $2, $3)`, 3, "-1 days +02:00:00", nil)

	// m's '90' is a bound parameter: Postgres types it plain interval,
	// reads 90 seconds, then truncates to the column's minutes — unlike
	// a literal '90', which reads as 90 minutes (probed with psql \bind
	// on PG 16.9: 00:01:00 vs 01:30:00).
	want := map[int][2]string{
		1: {"1 year 2 mons -3 days +01:02:03.000001", "00:01:00"},
		2: {"36:00:00.5", "1 day"},
		3: {"-1 days +02:00:00", ""},
	}
	// The text rendering, in both result formats and both protocols:
	// binary results decode through pgtype.Interval, whose fields must
	// be the stored ones.
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeDescribeExec, pgx.QueryExecModeExec, pgx.QueryExecModeSimpleProtocol} {
		r, err := c.Query(ctx, `select id, v::text, m::text, v from iv order by id`, mode)
		if err != nil {
			t.Fatal(err)
		}
		for r.Next() {
			var id int
			var v string
			var m *string
			var raw pgtype.Interval
			if err := r.Scan(&id, &v, &m, &raw); err != nil {
				t.Fatal(err)
			}
			mt := ""
			if m != nil {
				mt = *m
			}
			if w := want[id]; v != w[0] || mt != w[1] {
				t.Errorf("mode %v row %d: (%s, %s), want %v", mode, id, v, mt, w)
			}
			if id == 1 && (raw.Months != 14 || raw.Days != -3 || raw.Microseconds != 3_723_000_001) {
				t.Errorf("mode %v: pgtype.Interval = %+v", mode, raw)
			}
		}
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
	}

	// timestamptz - timestamptz is an interval (OID 1186), and a
	// timestamptz parameter plus an interval parameter lands on the
	// calendar.
	var diff pgtype.Interval
	var at time.Time
	base := time.Date(2024, 1, 31, 10, 0, 0, 0, time.UTC)
	if err := c.QueryRow(ctx, `select $1::timestamptz - $2::timestamptz, $1::timestamptz + $3::interval`,
		base, base.Add(-36*time.Hour), "1 mon").Scan(&diff, &at); err != nil {
		t.Fatal(err)
	}
	if diff.Days != 1 || diff.Microseconds != 12*3_600_000_000 || diff.Months != 0 {
		t.Errorf("timestamp difference = %+v, want 1 day 12:00:00", diff)
	}
	if !at.Equal(time.Date(2024, 2, 29, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("Jan 31 + 1 mon = %v, want Feb 29", at)
	}

	// A RANGE frame offset over a timestamp key is described as an
	// interval, so a driver sends "1 day" (or a pgtype.Interval) for $1.
	mustExec(t, c, `create table ev (id int primary key, t timestamptz)`)
	mustExec(t, c, `insert into ev values (1, '2024-01-01 00:00+00'), (2, '2024-01-01 12:00+00'), (3, '2024-01-03 00:00+00')`)
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeDescribeExec, pgx.QueryExecModeExec, pgx.QueryExecModeSimpleProtocol} {
		for _, off := range []any{"1 day", pgtype.Interval{Days: 1, Valid: true}} {
			if mode == pgx.QueryExecModeSimpleProtocol {
				if _, isText := off.(string); !isText {
					continue // the simple protocol interpolates text only
				}
			}
			rows, err := c.Query(ctx, `select count(*) over (order by t range between $1 preceding and current row) from ev order by id`, mode, off)
			if err != nil {
				t.Fatalf("mode %v offset %v: %v", mode, off, err)
			}
			counts, err := pgx.CollectRows(rows, pgx.RowTo[int64])
			if err != nil || len(counts) != 3 || counts[0] != 1 || counts[1] != 2 || counts[2] != 1 {
				t.Errorf("mode %v offset %v: counts %v %v, want [1 2 1]", mode, off, counts, err)
			}
		}
	}

	// Errors carry Postgres's codes.
	for q, code := range map[string]string{
		`select interval '1 fortnight'`:         "22007",
		`select interval '1:60'`:                "22015",
		`select interval '178956970 years' * 2`: "22008",
		`select interval '1 day' + 1`:           "42883",
	} {
		_, err := c.Exec(ctx, q)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != code {
			t.Errorf("%s: %v, want SQLSTATE %s", q, err, code)
		}
	}

	// The catalog: the column type and modifier a client introspects.
	var typ string
	var oid uint32
	var typmod int32
	if err := c.QueryRow(ctx, `select format_type(a.atttypid, a.atttypmod), a.atttypid, a.atttypmod
		from pg_attribute a join pg_class c on c.oid = a.attrelid
		where c.relname = 'iv' and a.attname = 'm'`).Scan(&typ, &oid, &typmod); err != nil {
		t.Fatal(err)
	}
	if typ != "interval minute" || oid != 1186 || typmod != 0x0800FFFF {
		t.Errorf("catalog: %s oid %d typmod %#x", typ, oid, typmod)
	}
}
