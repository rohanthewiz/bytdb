package bytdb

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestAddColumnBackfill covers the core promise: a DEFAULT on a
// non-empty table lands in the rows that already exist, so the column
// reads the same for old and new rows.
func TestAddColumnBackfill(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"}, []any{2, "grace", 45, "g@x"})

	if err := e.AddColumn("people", Column{Name: "city", Type: TString, Default: "'nyc'"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []any{1, 2} {
		row, ok, err := e.Get("people", id)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if row.Col("city") != "nyc" {
			t.Fatalf("backfilled column for %v = %v; want nyc", id, row.Col("city"))
		}
	}

	// The backfill wrote only the new column: everything else survives.
	row, _, _ := e.Get("people", 1)
	if row.Col("name") != "ada" || row.Col("age") != int64(36) || row.Col("email") != "a@x" {
		t.Fatalf("backfill disturbed existing columns: %v", row.Vals)
	}

	// An index built afterwards sees the backfilled values, which is
	// the observable proof the rows were rewritten rather than merely
	// reading a descriptor-level fallback.
	if _, err := e.CreateIndex("people", "by-city", false, "city"); err != nil {
		t.Fatal(err)
	}
	got := names(t, e.ScanIndex("people", "by-city", nil, nil))
	if want := []string{"ada", "grace"}; !slices.Equal(got, want) {
		t.Fatalf("index over backfilled column = %v; want %v", got, want)
	}

	// NOT NULL is now satisfiable on a non-empty table when a default
	// supplies the value.
	if err := e.AddColumn("people", Column{Name: "active", Type: TBool, NotNull: true, Default: "true"}); err != nil {
		t.Fatal(err)
	}
	row, _, _ = e.Get("people", 2)
	if row.Col("active") != true {
		t.Fatalf("NOT NULL DEFAULT backfill = %v; want true", row.Col("active"))
	}
}

// TestAddColumnBackfillTypes runs the literal decoder over the forms
// the SQL layer stores, including the evaluated clock markers.
func TestAddColumnBackfillTypes(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"})

	before := time.Now().UTC()
	for _, c := range []struct {
		col  Column
		want any
	}{
		{Column{Name: "n", Type: TInt, Default: "-7"}, int64(-7)},
		{Column{Name: "f", Type: TFloat, Default: "1.5"}, 1.5},
		{Column{Name: "b", Type: TBool, Default: "false"}, false},
		{Column{Name: "s", Type: TString, Default: "'it''s'"}, "it's"},
		{Column{Name: "tags", Type: TTextArray, Default: "'{a,b}'"}, "{a,b}"},
		{Column{Name: "doc", Type: TJSONB, Default: `'{"a": 1}'`}, `{"a":1}`},
		// DEFAULT NULL is what stored rows already read: no rewrite,
		// no error.
		{Column{Name: "nul", Type: TInt, Default: "null"}, nil},
	} {
		if err := e.AddColumn("people", c.col); err != nil {
			t.Fatalf("add %s: %v", c.col.Name, err)
		}
		row, _, _ := e.Get("people", 1)
		if got := row.Col(c.col.Name); got != c.want {
			t.Fatalf("backfilled %s = %#v; want %#v", c.col.Name, got, c.want)
		}
	}

	// now() resolves once, at DDL time, to an instant inside the
	// statement's window.
	if err := e.AddColumn("people", Column{Name: "seen", Type: TTimestamp, Default: "now()"}); err != nil {
		t.Fatal(err)
	}
	row, _, _ := e.Get("people", 1)
	seen, ok := row.Col("seen").(int64)
	if !ok || seen < before.UnixMicro() || seen > time.Now().UTC().UnixMicro() {
		t.Fatalf("now() backfill = %v; want an instant within the test", row.Col("seen"))
	}

	// current_date truncates to the UTC day on a date column.
	if err := e.AddColumn("people", Column{Name: "day", Type: TDate, Default: "current_date"}); err != nil {
		t.Fatal(err)
	}
	row, _, _ = e.Get("people", 1)
	wantDay := time.Date(before.Year(), before.Month(), before.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400
	if row.Col("day") != wantDay {
		t.Fatalf("current_date backfill = %v; want %v", row.Col("day"), wantDay)
	}
}

// TestAddColumnBackfillRejections: a default the engine cannot
// evaluate must fail the statement outright rather than half-apply.
func TestAddColumnBackfillRejections(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"})

	for _, c := range []struct{ name, def string }{
		{"expr", "1 + 2"},
		{"call", "gen_random_uuid()"},
		{"concat", "'a' || 'b'"},
	} {
		err := e.AddColumn("people", Column{Name: c.name, Type: TString, Default: c.def})
		if err == nil {
			t.Fatalf("default %q accepted", c.def)
		}
		// Either "not a literal ..." or the malformed-string wording,
		// depending on where the text stops looking like a constant.
		if !strings.Contains(err.Error(), "literal") {
			t.Fatalf("default %q: %v", c.def, err)
		}
		// Nothing was published: the failed add left no column behind.
		if e.Table("people").ColIndex(c.name) >= 0 {
			t.Fatalf("column %q published despite the error", c.name)
		}
	}

	// A default of the wrong type fails the same way.
	if err := e.AddColumn("people", Column{Name: "n", Type: TInt, Default: "'abc'"}); err == nil {
		t.Fatal("type-incompatible default accepted")
	}

	// NOT NULL with no usable value still needs an empty table.
	err := e.AddColumn("people", Column{Name: "req", Type: TInt, NotNull: true})
	if err == nil || !strings.Contains(err.Error(), "contains null values") {
		t.Fatalf("NOT NULL without default on a non-empty table: %v", err)
	}
	err = e.AddColumn("people", Column{Name: "req", Type: TInt, NotNull: true, Default: "null"})
	if err == nil || !strings.Contains(err.Error(), "contains null values") {
		t.Fatalf("NOT NULL DEFAULT NULL on a non-empty table: %v", err)
	}
}

// TestSetDropColumnDefault covers the descriptor-only alterations:
// they change what future inserts get, never what rows already hold.
func TestSetDropColumnDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"})

	if err := e.SetColumnDefault("people", "email", "'none@x'"); err != nil {
		t.Fatal(err)
	}
	if got := e.Table("people").Columns[3].Default; got != "'none@x'" {
		t.Fatalf("stored default = %q", got)
	}
	// Existing rows keep their values — SET DEFAULT is not a backfill.
	row, _, _ := e.Get("people", 1)
	if row.Col("email") != "a@x" {
		t.Fatalf("SET DEFAULT rewrote a row: %v", row.Col("email"))
	}

	// It survives a reopen, like every other descriptor change.
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = openEngine(t, path)
	defer e.Close()
	if got := e.Table("people").Columns[3].Default; got != "'none@x'" {
		t.Fatalf("default after reopen = %q", got)
	}

	// Replacing and dropping.
	if err := e.SetColumnDefault("people", "email", "'other@x'"); err != nil {
		t.Fatal(err)
	}
	if err := e.DropColumnDefault("people", "email"); err != nil {
		t.Fatal(err)
	}
	if got := e.Table("people").Columns[3].Default; got != "" {
		t.Fatalf("default after drop = %q", got)
	}
	// DROP DEFAULT is idempotent, as in Postgres.
	if err := e.DropColumnDefault("people", "email"); err != nil {
		t.Fatal(err)
	}
	// SetColumnDefault("") is the drop.
	if err := e.SetColumnDefault("people", "email", ""); err != nil {
		t.Fatal(err)
	}

	// Rejections: unknown table/column, and a literal the column
	// cannot hold (caught at DDL time, not at the first insert).
	if err := e.SetColumnDefault("ghosts", "email", "'x'"); err == nil {
		t.Fatal("unknown table accepted")
	}
	if err := e.SetColumnDefault("people", "nope", "'x'"); err == nil {
		t.Fatal("unknown column accepted")
	}
	if err := e.DropColumnDefault("people", "nope"); err == nil {
		t.Fatal("unknown column accepted by drop")
	}
	if err := e.SetColumnDefault("people", "age", "'abc'"); err == nil {
		t.Fatal("type-incompatible default accepted")
	}
}

// TestSetDropColumnNotNull covers the validating flag flip: the cheap
// half of the "add a required column" migration on a table too large
// to rewrite in one transaction.
func TestSetDropColumnNotNull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	peopleTable(t, e)
	insertPeople(t, e, []any{1, "ada", 36, "a@x"}, []any{2, "grace", 45, nil})

	// A column with a NULL in it is refused, with Postgres's wording,
	// and nothing is published.
	err := e.SetColumnNotNull("people", "email")
	if err == nil || !strings.Contains(err.Error(), "contains null values") {
		t.Fatalf("SET NOT NULL over a NULL: %v", err)
	}
	if e.Table("people").Columns[3].NotNull {
		t.Fatal("flag published despite the NULL")
	}

	// Fill the gap and it succeeds; the flag then rejects new NULLs.
	if _, err := e.Update("people", []any{2}, map[string]any{"email": "g@x"}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetColumnNotNull("people", "email"); err != nil {
		t.Fatal(err)
	}
	if !e.Table("people").Columns[3].NotNull {
		t.Fatal("NOT NULL not published")
	}
	if err := e.Insert("people", 3, "hopper", 50, nil); err == nil {
		t.Fatal("NULL accepted after SET NOT NULL")
	}
	if _, err := e.Update("people", []any{1}, map[string]any{"email": nil}); err == nil {
		t.Fatal("update to NULL accepted after SET NOT NULL")
	}

	// Idempotent, and it survives a reopen.
	if err := e.SetColumnNotNull("people", "email"); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = openEngine(t, path)
	defer e.Close()
	if !e.Table("people").Columns[3].NotNull {
		t.Fatal("NOT NULL lost across reopen")
	}

	// A key column needs no scan and is already NOT NULL by
	// construction; setting it is a no-op rather than an error.
	if err := e.SetColumnNotNull("people", "id"); err != nil {
		t.Fatal(err)
	}

	// DROP NOT NULL is the descriptor flip back, idempotent, and
	// refused on a primary key.
	if err := e.DropColumnNotNull("people", "email"); err != nil {
		t.Fatal(err)
	}
	if e.Table("people").Columns[3].NotNull {
		t.Fatal("NOT NULL not cleared")
	}
	if err := e.Insert("people", 4, "katherine", 60, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.DropColumnNotNull("people", "email"); err != nil {
		t.Fatal(err)
	}
	if err := e.DropColumnNotNull("people", "id"); err == nil ||
		!strings.Contains(err.Error(), "primary key") {
		t.Fatalf("DROP NOT NULL on a key column: %v", err)
	}

	// Unknown table/column on both.
	if err := e.SetColumnNotNull("ghosts", "email"); err == nil {
		t.Fatal("unknown table accepted")
	}
	if err := e.SetColumnNotNull("people", "nope"); err == nil {
		t.Fatal("unknown column accepted")
	}
	if err := e.DropColumnNotNull("people", "nope"); err == nil {
		t.Fatal("unknown column accepted by drop")
	}
}

// TestSetNotNullIdentity: identity columns are NOT NULL by definition,
// so the flag cannot be dropped from one.
func TestSetNotNullIdentity(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	if _, err := e.CreateTable("s", []Column{
		{Name: "id", Type: TInt}, {Name: "n", Type: TInt, Identity: true},
	}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.DropColumnNotNull("s", "n"); err == nil ||
		!strings.Contains(err.Error(), "identity") {
		t.Fatalf("DROP NOT NULL on an identity column: %v", err)
	}
}

// TestAddColumnBackfillLimit covers the guard that refuses a one-shot
// ADD COLUMN ... DEFAULT on a table too large to rewrite in one
// transaction. The three cases that matter are the boundary (a table
// exactly at the cap still runs), one row past it (refused, and
// nothing changed), and the disabled guard (0 means no cap).
func TestAddColumnBackfillLimit(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	peopleTable(t, e)
	for i := 1; i <= 4; i++ {
		insertPeople(t, e, []any{i, "p", 1, "e"})
	}

	// Exactly at the cap: allowed. The guard is "more than limit", not
	// "limit or more" — pinning that here keeps a future off-by-one
	// from silently tightening the rule.
	e.SetBackfillLimit(4)
	if err := e.AddColumn("people", Column{Name: "city", Type: TString, Default: "'nyc'"}); err != nil {
		t.Fatalf("backfill at exactly the limit should run: %v", err)
	}
	if row, _, _ := e.Get("people", 1); row.Col("city") != "nyc" {
		t.Fatalf("city = %v; want nyc", row.Col("city"))
	}

	// One row over: refused, and the refusal is atomic — no descriptor
	// change, so the column does not exist even partially.
	e.SetBackfillLimit(3)
	err := e.AddColumn("people", Column{Name: "zip", Type: TString, Default: "'10001'"})
	if err == nil {
		t.Fatal("backfill over the limit should be refused")
	}
	if !strings.Contains(err.Error(), "backfill limit") {
		t.Fatalf("error should name the limit: %v", err)
	}
	if !strings.Contains(err.Error(), "SET NOT NULL") {
		t.Fatalf("error should name the batched alternative: %v", err)
	}
	if desc := e.Table("people"); desc.ColIndex("zip") >= 0 {
		t.Fatal("refused ADD COLUMN left the column in the descriptor")
	}

	// The defaultless form is O(1) and rewrites nothing, so the cap
	// must not touch it however low it is set.
	e.SetBackfillLimit(1)
	if err := e.AddColumn("people", Column{Name: "note", Type: TString}); err != nil {
		t.Fatalf("defaultless ADD COLUMN must ignore the backfill limit: %v", err)
	}

	// 0 disables the guard entirely.
	e.SetBackfillLimit(0)
	if err := e.AddColumn("people", Column{Name: "zip", Type: TString, Default: "'10001'"}); err != nil {
		t.Fatalf("disabled limit should allow the backfill: %v", err)
	}
	if row, _, _ := e.Get("people", 4); row.Col("zip") != "10001" {
		t.Fatalf("zip = %v; want 10001", row.Col("zip"))
	}
}

// TestDefaultBackfillLimitIsSet guards the wiring: a freshly opened
// engine carries the documented default rather than a zero value,
// which would silently disable the guard for every embedder.
func TestDefaultBackfillLimitIsSet(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	if got := e.BackfillLimit(); got != DefaultBackfillLimit {
		t.Fatalf("BackfillLimit() = %d; want %d", got, DefaultBackfillLimit)
	}
}
