package bytdb_test

// Deep-look D-56 part 2: argument normalization, Row getters,
// error-returning catalog lookups, and the Engine one-shots. An external
// test package, so views and sequences can be made through SQL.

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"
	bsql "github.com/rohanthewiz/bytdb/sql"
)

type userID int64
type code string
type uuidArr [16]byte

// upper is a driver.Valuer that stores its text upper-cased.
type upper string

func (u upper) Value() (driver.Value, error) { return string(u) + "!", nil }

type badValuer struct{}

func (badValuer) Value() (driver.Value, error) { return nil, errors.New("boom") }

func openTestEngine(t *testing.T) *bytdb.Engine {
	t.Helper()
	e, err := bytdb.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func TestNormalizeValue(t *testing.T) {
	n := int64(7)
	var nilp *int64
	for _, c := range []struct {
		in   any
		want any
	}{
		{userID(5), int64(5)},
		{code("x"), "x"},
		{&n, int64(7)},
		{nilp, nil},
		{uint64(9), int64(9)},
		{uint8(3), int64(3)},
		{float32(1.5), 1.5},
		{upper("hi"), "hi!"},
		{sql.NullString{String: "s", Valid: true}, "s"},
		{sql.NullInt64{}, nil},
		{[]string{"a", "b"}, []string{"a", "b"}},
	} {
		got, err := bytdb.NormalizeValue(c.in)
		if err != nil {
			t.Errorf("%T: %v", c.in, err)
			continue
		}
		if gs, ok := got.([]string); ok {
			if len(gs) != 2 || gs[0] != "a" {
				t.Errorf("%T: %v", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeValue(%T %v) = %v (%T), want %v", c.in, c.in, got, got, c.want)
		}
	}
	if b, err := bytdb.NormalizeValue(uuidArr{1, 2}); err != nil || len(b.([]byte)) != 16 {
		t.Errorf("named [16]byte: %v %v", b, err)
	}
	if _, err := bytdb.NormalizeValue(uint64(math.MaxUint64)); err == nil {
		t.Error("uint64 overflow accepted")
	}
	if _, err := bytdb.NormalizeValue(struct{}{}); !errors.Is(err, bytdb.ErrUnsupportedValue) {
		t.Errorf("struct: %v", err)
	}
	if _, err := bytdb.NormalizeValue(badValuer{}); err == nil || errors.Is(err, bytdb.ErrUnsupportedValue) {
		t.Errorf("failing Valuer: %v", err)
	}
}

func TestEngineArgumentShapes(t *testing.T) {
	e := openTestEngine(t)
	if _, err := e.CreateTable("t", []bytdb.Column{
		{Name: "id", Type: bytdb.TInt}, {Name: "x", Type: bytdb.TFloat},
		{Name: "ts", Type: bytdb.TTimestamp}, {Name: "s", Type: bytdb.TString},
		{Name: "tags", Type: bytdb.TTextArray}, {Name: "u", Type: bytdb.TUUID},
		{Name: "d", Type: bytdb.TDate},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	s := "ptr"
	// A named key, int32 into float, an int into a timestamp (micros), a
	// pointer, []string, a named 16-byte array, a time into a date.
	day := time.Date(2024, 2, 29, 15, 0, 0, 0, time.UTC)
	if err := e.Insert("t", userID(1), int32(3), 1_000_000, &s, []string{"a", "b c"}, uuidArr{9}, day); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("t", uint(2), upper("v"), nil, nil, nil, nil, nil); err == nil {
		t.Error("a Valuer's text accepted into a float column")
	}
	row, ok, err := e.Get("t", userID(1))
	if err != nil || !ok {
		t.Fatalf("Get by named key: %v %v", ok, err)
	}
	if x, _ := row.Float("x"); x != 3 {
		t.Errorf("x = %v", x)
	}
	if ts, ok := row.Time("ts"); !ok || !ts.Equal(time.Unix(1, 0)) {
		t.Errorf("ts = %v %v", ts, ok)
	}
	if d, ok := row.Time("d"); !ok || !d.Equal(time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("d = %v %v", d, ok)
	}
	if v, _ := row.String("s"); v != "ptr" {
		t.Errorf("s = %v", v)
	}
	if v, _ := row.String("tags"); v != `{a,"b c"}` {
		t.Errorf("tags = %v", v)
	}
	if b, ok := row.Bytes("u"); !ok || len(b) != 16 || b[0] != 9 {
		t.Errorf("u = %v", b)
	}
	if id, ok := row.Int("id"); !ok || id != 1 {
		t.Errorf("id = %v", id)
	}
	if _, ok := row.Int("nope"); ok {
		t.Error("Int of a missing column ok")
	}
	if m := row.Map(); m["s"] != "ptr" || len(m) != 7 {
		t.Errorf("Map = %v", m)
	}
	var zero bytdb.Row
	if zero.Map() != nil || zero.Col("x") != nil {
		t.Error("zero Row not empty")
	}
	if _, ok := zero.Time("ts"); ok {
		t.Error("zero Row Time ok")
	}
	if _, err := e.UpdateRow("t", map[string]any{"s": code("named")}, userID(1)); err != nil {
		t.Fatal(err)
	}
	row, _, _ = e.Get("t", int64(1))
	if v, _ := row.String("s"); v != "named" {
		t.Errorf("after UpdateRow s = %v", v)
	}
}

func TestEngineOneShots(t *testing.T) {
	e := openTestEngine(t)
	d := bsql.New(e)
	for _, q := range []string{
		`create table t (id int primary key, a text)`,
		`create view v as select id from t`,
		`create sequence s1 start 10`,
		`insert into t values (1, 'a'), (2, 'b'), (3, 'c')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := e.Count("t"); err != nil || n != 3 {
		t.Errorf("Count = %d %v", n, err)
	}
	if ok, err := e.Exists("t", int64(2)); err != nil || !ok {
		t.Errorf("Exists(2) = %v %v", ok, err)
	}
	if ok, err := e.Exists("t", int64(9)); err != nil || ok {
		t.Errorf("Exists(9) = %v %v", ok, err)
	}
	if _, err := e.Count("ghost"); !errors.Is(err, bytdb.ErrNoSuchTable) {
		t.Errorf("Count(ghost): %v", err)
	}
	if v, err := e.NextVal("s1"); err != nil || v != 10 {
		t.Errorf("NextVal = %d %v", v, err)
	}
	if err := e.SetVal("s1", 50, true); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.NextVal("s1"); v != 51 {
		t.Errorf("NextVal after SetVal = %d", v)
	}
	if err := e.Truncate("t", false); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.Count("t"); n != 0 {
		t.Errorf("Count after Truncate = %d", n)
	}

	// Lookups that report errors: absence is ErrNoSuchTable.
	if names, err := e.TableNames(); err != nil || len(names) != 1 || names[0] != "t" {
		t.Errorf("TableNames = %v %v", names, err)
	}
	if desc, err := e.LookupTable("t"); err != nil || desc.Name != "t" {
		t.Errorf("LookupTable = %v %v", desc, err)
	}
	if vd, err := e.LookupView("v"); err != nil || vd.Name != "v" {
		t.Errorf("LookupView = %v %v", vd, err)
	}
	if vs, err := e.ListViews(); err != nil || len(vs) != 1 {
		t.Errorf("ListViews = %v %v", vs, err)
	}
	if sd, err := e.LookupSequence("s1"); err != nil || sd.Name != "s1" {
		t.Errorf("LookupSequence = %v %v", sd, err)
	}
	if ss, err := e.ListSequences(); err != nil || len(ss) != 1 {
		t.Errorf("ListSequences = %v %v", ss, err)
	}
	for name, f := range map[string]func() error{
		"table":    func() error { _, err := e.LookupTable("nope"); return err },
		"view":     func() error { _, err := e.LookupView("nope"); return err },
		"sequence": func() error { _, err := e.LookupSequence("nope"); return err },
	} {
		if err := f(); !errors.Is(err, bytdb.ErrNoSuchTable) {
			t.Errorf("Lookup %s of an absent name: %v", name, err)
		}
	}
	// A closed engine is an error, not "does not exist".
	e.Close()
	if _, err := e.LookupTable("t"); err == nil || errors.Is(err, bytdb.ErrNoSuchTable) {
		t.Errorf("LookupTable on a closed engine: %v", err)
	}
	if _, err := e.TableNames(); err == nil {
		t.Error("TableNames on a closed engine: no error")
	}
}
