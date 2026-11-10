package bytdb

// Deep-look D-55: the date/time, boolean and uuid input forms Postgres
// accepts beyond ISO 8601.

import (
	"testing"
	"time"
)

func TestParseTimestampForms(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	for in, want := range map[string]time.Time{
		"2024-02-29 12:00":                  time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC),
		"2024-02-29T12:00":                  time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC),
		"2024-02-29 12:00+02":               time.Date(2024, 2, 29, 10, 0, 0, 0, time.UTC),
		"2024-02-29 12:00:00 UTC":           time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC),
		"2024-02-29 12:00:00 Z":             time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC),
		"2024-02-29 12:00 America/New_York": time.Date(2024, 2, 29, 12, 0, 0, 0, ny),
		"2024-2-9":                          time.Date(2024, 2, 9, 0, 0, 0, 0, time.UTC),
		"20240229":                          time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC),
		"epoch":                             time.Unix(0, 0),
		"EPOCH":                             time.Unix(0, 0),
	} {
		got, err := ParseTimestamp(in)
		if err != nil || got != want.UnixMicro() {
			t.Errorf("ParseTimestamp(%q) = %d, %v; want %d", in, got, err, want.UnixMicro())
		}
	}
	if v, _ := ParseTimestamp("infinity"); v != TimestampInfinity || FormatTimestamp(v) != "infinity" {
		t.Errorf("infinity = %d (%s)", v, FormatTimestamp(v))
	}
	if v, _ := ParseTimestamp("-infinity"); v != TimestampNegInfinity || FormatTimestamp(v) != "-infinity" {
		t.Errorf("-infinity = %d (%s)", v, FormatTimestamp(v))
	}
	now := time.Now().UnixMicro()
	if v, _ := ParseTimestamp("now"); v < now-int64(time.Minute/time.Microsecond) || v > now+int64(time.Minute/time.Microsecond) {
		t.Errorf("now = %d, want about %d", v, now)
	}
	today, _ := ParseDate("today")
	tomorrow, _ := ParseDate("tomorrow")
	yesterday, _ := ParseDate("yesterday")
	if tomorrow != today+1 || yesterday != today-1 || today != time.Now().UTC().Unix()/86400 {
		t.Errorf("today %d tomorrow %d yesterday %d", today, tomorrow, yesterday)
	}
	if d, _ := ParseDate("infinity"); d != DateInfinity || FormatDate(d) != "infinity" {
		t.Errorf("date infinity = %d", d)
	}
	if d, err := ParseDate("2024-2-9"); err != nil || FormatDate(d) != "2024-02-09" {
		t.Errorf("2024-2-9 = %d, %v", d, err)
	}
	for _, bad := range []string{"2024-02-30", "noon", "2024-02-29 25:00", "2024-02-29 12:00 Mars/Base"} {
		if _, err := ParseTimestamp(bad); err == nil {
			t.Errorf("ParseTimestamp(%q) accepted", bad)
		}
	}
}

func TestParseBoolForms(t *testing.T) {
	for in, want := range map[string]bool{
		"true": true, "TRUE": true, "t": true, "tr": true, " yes ": true, "y": true, "on": true, "1": true,
		"false": false, "f": false, "fal": false, "no": false, "n": false, "off": false, "0": false,
	} {
		if got, err := ParseBool(in); err != nil || got != want {
			t.Errorf("ParseBool(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "o", "maybe", "2", "truee", "yess"} {
		if _, err := ParseBool(bad); err == nil {
			t.Errorf("ParseBool(%q) accepted", bad)
		}
	}
}

func TestParseUUIDBraces(t *testing.T) {
	a, err1 := ParseUUID("{a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11}")
	b, err2 := ParseUUID("a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11")
	if err1 != nil || err2 != nil || string(a) != string(b) {
		t.Fatalf("braced uuid: %x %v / %x %v", a, err1, b, err2)
	}
}
