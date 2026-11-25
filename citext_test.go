package bytdb_test

// N-053: the citext column type at the engine level — keys and unique
// indexes that ignore case while every value reads back as written, a
// point lookup in another case, ALTER COLUMN TYPE to and from text, and
// a reopen.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/bytdb"
)

func TestCITextKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ci.db")
	e, err := bytdb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable("u", []bytdb.Column{
		{Name: "login", Type: bytdb.TCIText},
		{Name: "email", Type: bytdb.TCIText},
	}, "login"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndex("u", "u_email", true, "email"); err != nil {
		t.Fatal(err)
	}
	for _, r := range [][2]any{{"Ada", "ADA@x.org"}, {bytdb.CIText("grace"), []byte("Grace@X.org")}, {"ALAN", "alan@x.org"}} {
		if err := e.Insert("u", r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Insert("u", "ADA", "other@x.org"); !errors.Is(err, bytdb.ErrDuplicateKey) {
		t.Fatalf("ADA after Ada: %v", err)
	}
	if err := e.Insert("u", "barbara", "GRACE@x.ORG"); !errors.Is(err, bytdb.ErrUniqueViolation) {
		t.Fatalf("email equal ignoring case: %v", err)
	}
	check := func(e *bytdb.Engine) {
		t.Helper()
		var got []string
		for r, err := range e.Scan("u") {
			if err != nil {
				t.Fatal(err)
			}
			s, _ := r.String("login")
			got = append(got, s)
		}
		// Key order ignores case; each key reads back as written.
		if strings.Join(got, " ") != "Ada ALAN grace" {
			t.Errorf("scan = %v", got)
		}
		r, ok, err := e.Get("u", "aDa")
		if s, _ := r.String("login"); err != nil || !ok || s != "Ada" {
			t.Errorf("Get(aDa) = %v %v %v", r.Vals, ok, err)
		}
		var viaIndex []string
		for r, err := range e.ScanIndex("u", "u_email", []any{"b"}, []any{"Z"}) {
			if err != nil {
				t.Fatal(err)
			}
			s, _ := r.String("login")
			viaIndex = append(viaIndex, s)
		}
		if strings.Join(viaIndex, " ") != "grace" {
			t.Errorf("index range = %v", viaIndex)
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

	// citext -> text keeps the characters, and the key becomes
	// case-sensitive; text -> citext folds again, refusing collisions.
	if err := e.AlterColumnType("u", "email", bytdb.ColumnTypeChange{Type: bytdb.TString}); err != nil {
		t.Fatal(err)
	}
	if err := e.Insert("u", "edsger", "ALAN@x.org"); err != nil {
		t.Fatalf("ALAN@x.org beside alan@x.org as text: %v", err)
	}
	if err := e.AlterColumnType("u", "email", bytdb.ColumnTypeChange{Type: bytdb.TCIText}); err == nil {
		t.Error("text -> citext accepted two emails equal ignoring case")
	}
}
