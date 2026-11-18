package bytdb

// Engine-level tests for dependency-aware drops (N-056, drop.go). The
// SQL layer's tests (sql/drop_cascade_test.go) cover the statements and
// their notices against PG16's behavior. These pin what only the
// engine can show: the key ranges that go, the sentinel, atomicity
// when the commit fails, the view count, and the state after reopen.

import (
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb/tuple"
)

// keysUnder counts the stored keys under prefix.
func keysUnder(e *Engine, prefix []byte) int {
	n := 0
	end := string(tuple.PrefixEnd(prefix))
	e.kv.View(func(tx *btypedb.Tx[string, []byte]) error {
		for k := range tx.Ascend(string(prefix)) {
			if k >= end {
				break
			}
			n++
		}
		return nil
	})
	return n
}

// fkNames lists a table's foreign-key names.
func fkNames(e *Engine, table string) []string {
	var out []string
	for _, fk := range e.Table(table).ForeignKeys {
		out = append(out, fk.Name)
	}
	return out
}

// parentChild builds p(id, code unique) and c(id, pid -> p.id,
// pc -> p.code, k), with an index on c.k and three rows each.
func parentChild(t *testing.T, e *Engine) {
	t.Helper()
	if _, err := e.CreateTable("p", []Column{{Name: "id", Type: TInt}, {Name: "code", Type: TString}}, "id"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndex("p", "p_code_key", true, "code"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateTable("c", []Column{{Name: "id", Type: TInt}, {Name: "pid", Type: TInt},
		{Name: "pc", Type: TString}, {Name: "k", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateIndex("c", "c_k", false, "k"); err != nil {
		t.Fatal(err)
	}
	for _, fk := range []FKDesc{
		{Name: "c_pid_fkey", Cols: []int{1}, RefTable: "p", RefCols: []string{"id"}},
		{Name: "c_pc_fkey", Cols: []int{2}, RefTable: "p", RefCols: []string{"code"}},
	} {
		if err := e.AddForeignKey("c", fk, false); err != nil {
			t.Fatal(err)
		}
	}
	for i := int64(1); i <= 3; i++ {
		code := "c" + strconv.FormatInt(i, 10)
		if err := e.Insert("p", i, code); err != nil {
			t.Fatal(err)
		}
		if err := e.Insert("c", i, i, code, i*10); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDropColumnWithEngine: the column's indexes and own foreign keys go
// with it, entries included, CASCADE or not. A foreign key referencing
// the column is a dependent: refused with ErrDependentObjects under
// RESTRICT, dropped (and returned) under CASCADE. Everything survives a
// reopen.
func TestDropColumnWithEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	e := openEngine(t, path)
	parentChild(t, e)
	cDesc := e.Table("c")
	kIdx := cDesc.Index("c_k")
	if keysUnder(e, indexPrefix(cDesc.ID, kIdx.ID)) != 3 {
		t.Fatal("c_k entries missing before the drop")
	}

	// Auto: dropping c.k takes c_k and its three entries.
	deps, err := e.DropColumnWith("c", "k", DropOptions{})
	if err != nil || len(deps) != 0 {
		t.Fatalf("drop c.k: %v %v", deps, err)
	}
	if e.Table("c").Index("c_k") != nil {
		t.Fatal("c_k survived")
	}
	if n := keysUnder(e, indexPrefix(cDesc.ID, kIdx.ID)); n != 0 {
		t.Fatalf("c_k entries left behind: %d", n)
	}

	// Auto: dropping c.pid takes c's own FK over it. The FK over pc,
	// which sat at a higher ordinal, shifts down and still enforces.
	if _, err := e.DropColumnWith("c", "pid", DropOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := fkNames(e, "c"); !slices.Equal(got, []string{"c_pc_fkey"}) {
		t.Fatalf("c FKs after dropping pid: %v", got)
	}
	if fk := e.Table("c").ForeignKeys[0]; fk.Cols[0] != 1 || e.Table("c").Columns[1].Name != "pc" {
		t.Fatalf("c_pc_fkey ordinal not shifted: %+v", fk)
	}

	// Normal: p.code is referenced by c_pc_fkey.
	_, err = e.DropColumnWith("p", "code", DropOptions{})
	if !errors.Is(err, ErrDependentObjects) ||
		!strings.Contains(err.Error(), "cannot drop column code of table p because other objects depend on it") {
		t.Fatalf("RESTRICT drop of a referenced column: %v", err)
	}
	if e.Table("p").ColIndex("code") < 0 || len(e.Table("c").ForeignKeys) != 1 {
		t.Fatal("a refused drop changed something")
	}
	deps, err = e.DropColumnWith("p", "code", DropOptions{Cascade: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []Dependent{{Kind: DepConstraint, Name: "c_pc_fkey", Table: "c", On: "column code of table p"}}
	if !slices.Equal(deps, want) {
		t.Fatalf("cascaded = %+v", deps)
	}
	if len(e.Table("c").ForeignKeys) != 0 || e.Table("p").Index("p_code_key") != nil {
		t.Fatal("CASCADE left the FK or the unique index")
	}

	// The primary key column stays undroppable.
	wantErr(t, func() error { _, err := e.DropColumnWith("p", "id", DropOptions{Cascade: true}); return err }(),
		"cannot drop a primary key column")

	// The descriptors were written, not just cached.
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e = openEngine(t, path)
	defer e.Close()
	if d := e.Table("c"); len(d.Indexes) != 0 || len(d.ForeignKeys) != 0 || len(d.Columns) != 2 {
		t.Fatalf("c after reopen: %+v", d)
	}
	if d := e.Table("p"); len(d.Indexes) != 0 || len(d.Columns) != 1 {
		t.Fatalf("p after reopen: %+v", d)
	}
}

// TestDropTablesEngine: references inside the drop set pass; references
// from outside are dependents. The views a ViewReads callback names are
// dependents too, transitively, and the view count follows the drops.
func TestDropTablesEngine(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	parentChild(t, e)

	// The callback reads a fake view language: the query text is the
	// comma-separated list of names it reads.
	reads := func(q string) ([]string, error) { return strings.Split(q, ","), nil }
	for _, v := range [][2]string{{"vp", "p"}, {"vvp", "vp"}, {"vother", "c"}} {
		if err := e.CreateView(v[0], v[1], false); err != nil {
			t.Fatal(err)
		}
	}

	_, err := e.DropTables([]string{"p"}, DropOptions{ViewReads: reads})
	if !errors.Is(err, ErrDependentObjects) {
		t.Fatalf("RESTRICT drop of p: %v", err)
	}
	if e.Table("p") == nil || len(e.Views()) != 3 {
		t.Fatal("a refused drop changed something")
	}
	// Two names: Postgres's wording names neither.
	_, err = e.DropTables([]string{"p", "p"}, DropOptions{}) // deduped: one table
	wantErr(t, err, "cannot drop table p because")
	_, err = e.DropTables([]string{"c", "p"}, DropOptions{ViewReads: reads})
	wantErr(t, err, "cannot drop desired object(s) because other objects depend on them")

	// CASCADE: c's two FKs, then vp and (through it) vvp. vother reads
	// c, which survives.
	deps, err := e.DropTables([]string{"p"}, DropOptions{Cascade: true, ViewReads: reads})
	if err != nil {
		t.Fatal(err)
	}
	want := []Dependent{
		{Kind: DepConstraint, Name: "c_pc_fkey", Table: "c", On: "table p"},
		{Kind: DepConstraint, Name: "c_pid_fkey", Table: "c", On: "table p"},
		{Kind: DepView, Name: "vp", On: "table p"},
		{Kind: DepView, Name: "vvp", On: "view vp"},
	}
	if !slices.Equal(deps, want) {
		t.Fatalf("cascaded =\n%+v\nwant\n%+v", deps, want)
	}
	if e.Table("p") != nil || len(e.Table("c").ForeignKeys) != 0 {
		t.Fatal("p or c's FKs survived")
	}
	if vs := e.Views(); len(vs) != 1 || vs[0].Name != "vother" || !e.HasViews() {
		t.Fatalf("views after cascade: %v", vs)
	}

	// The parent and child together need no CASCADE.
	if _, err := e.CreateTable("p2", []Column{{Name: "id", Type: TInt}}, "id"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddForeignKey("c", FKDesc{Name: "c_p2", Cols: []int{1}, RefTable: "p2", RefCols: []string{"id"}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DropTables([]string{"p2", "c"}, DropOptions{ViewReads: reads}); !errors.Is(err, ErrDependentObjects) {
		t.Fatalf("vother still reads c: %v", err) // the view, not the FK, is in the way
	}
	if _, err := e.DropView("vother"); err != nil {
		t.Fatal(err)
	}
	if e.HasViews() {
		t.Fatal("view count off after the cascade")
	}
	if _, err := e.DropTables([]string{"p2", "c"}, DropOptions{ViewReads: reads}); err != nil {
		t.Fatalf("DROP TABLE p2, c: %v", err)
	}
	if len(e.Tables()) != 0 {
		t.Fatalf("tables left: %v", e.Tables())
	}
}

// TestDropCascadeAtomic: a CASCADE drop whose commit fails leaves the
// target and every dependent as they were. That is the point of running
// the whole drop as one DDL transaction.
func TestDropCascadeAtomic(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	parentChild(t, e)
	reads := func(q string) ([]string, error) { return strings.Split(q, ","), nil }
	if err := e.CreateView("vp", "p", false); err != nil {
		t.Fatal(err)
	}

	e.testCommitErr = errCommitBoom
	_, errT := e.DropTables([]string{"p"}, DropOptions{Cascade: true, ViewReads: reads})
	_, errC := e.DropColumnWith("p", "code", DropOptions{Cascade: true})
	_, errI := e.DropIndexWith("p", "p_code_key", DropOptions{Cascade: true})
	_, _, errV := e.DropViewWith("vp", DropOptions{Cascade: true, ViewReads: reads})
	e.testCommitErr = nil
	for _, err := range []error{errT, errC, errI, errV} {
		if !errors.Is(err, errCommitBoom) {
			t.Fatalf("simulated commit failure not reported: %v", err)
		}
	}
	if e.Table("p") == nil || e.Table("p").Index("p_code_key") == nil || e.Table("p").ColIndex("code") < 0 {
		t.Fatal("p changed by failed drops")
	}
	if got := fkNames(e, "c"); len(got) != 2 {
		t.Fatalf("c's FKs changed by failed drops: %v", got)
	}
	if e.View("vp") == nil || !e.HasViews() {
		t.Fatal("vp changed by failed drops")
	}
}

// TestDropIndexWithEngine: a unique index is a dependency only for the
// foreign keys no other unique key covers.
func TestDropIndexWithEngine(t *testing.T) {
	e := openEngine(t, filepath.Join(t.TempDir(), "test.db"))
	defer e.Close()
	parentChild(t, e)
	// A second unique index over the same column covers c_pc_fkey too.
	if _, err := e.CreateIndex("p", "p_code_key2", true, "code"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DropIndexWith("p", "p_code_key", DropOptions{}); err != nil {
		t.Fatalf("drop of a redundant unique index: %v", err)
	}
	_, err := e.DropIndexWith("p", "p_code_key2", DropOptions{})
	if !errors.Is(err, ErrDependentObjects) || !strings.Contains(err.Error(), "cannot drop index p_code_key2") {
		t.Fatalf("RESTRICT drop of the last covering index: %v", err)
	}
	deps, err := e.DropIndexWith("p", "p_code_key2", DropOptions{Cascade: true})
	if err != nil || len(deps) != 1 || deps[0].String() != "constraint c_pc_fkey on table c" {
		t.Fatalf("cascade: %+v %v", deps, err)
	}
	if got := fkNames(e, "c"); !slices.Equal(got, []string{"c_pid_fkey"}) {
		t.Fatalf("c FKs: %v", got)
	}
}

// TestDependentString pins Postgres's quoting: relation names quoted
// when they need it (reserved words too), constraint names bare.
func TestDependentString(t *testing.T) {
	for _, tc := range []struct {
		d    Dependent
		want string
	}{
		{Dependent{Kind: DepConstraint, Name: "Fk 5", Table: "C5"}, `constraint Fk 5 on table "C5"`},
		{Dependent{Kind: DepConstraint, Name: "c_fk", Table: "c"}, `constraint c_fk on table c`},
		{Dependent{Kind: DepView, Name: "user"}, `view "user"`},
		{Dependent{Kind: DepView, Name: "v_1"}, `view v_1`},
	} {
		if got := tc.d.String(); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.d, got, tc.want)
		}
	}
}
