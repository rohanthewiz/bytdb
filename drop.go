package bytdb

// drop.go: drops that follow dependencies the way Postgres's do
// (N-056). This reverses deep-look D-54's RESTRICT-only rule, under which
// bytdb refused any drop something else depended on and asked for the
// dependent object to be removed first.
//
// Postgres records two strengths of dependency, and a drop treats them
// differently:
//
//	auto    The dependent is part of the target and goes with it,
//	        CASCADE or not. Examples: an index or CHECK over a dropped
//	        column, a foreign key whose own (child) columns are dropped,
//	        a dropped table's own indexes and constraints.
//	normal  The dependent is a separate object that uses the target:
//	        another table's foreign key referencing a dropped table or
//	        column, or a view reading a dropped table or view. RESTRICT
//	        (the default) refuses the drop and lists them in the error's
//	        DETAIL (2BP01). CASCADE drops them too, and the SQL layer
//	        raises a "drop cascades to ..." notice for each.
//
// What each drop reaches, checked against Postgres 16:
//
//	target          auto (always)                   normal (CASCADE)
//	──────────────  ──────────────────────────────  ──────────────────────────────────
//	DROP TABLE t    t's indexes, FKs, checks and    FKs on tables outside the drop set
//	                identity counters               that reference t; views reading t,
//	                                                and views reading those views
//	DROP COLUMN c   indexes and CHECKs using c;     FKs (on any table, t included)
//	                t's FKs whose own columns       whose referenced columns include c
//	                include c
//	DROP INDEX i    —                               FKs whose referenced key only i
//	                                                made unique
//	DROP VIEW v     —                               views reading v, transitively
//
// Every drop runs as one DDL transaction. The target and every
// dependent it removes commit together, or nothing changes.
//
// The engine stores views and CHECK constraints as SQL text it does not
// parse, so it cannot see what they read. DropOptions carries callbacks
// from the SQL layer that answer that. When they are nil, views and
// CHECKs are not treated as dependents, which is the engine API's
// behavior from before N-056. Dependencies are by name, not by oid as in
// Postgres: a view reads whatever relation holds the name when it runs.
//
// Two of Postgres's dependencies are not followed here:
//   - A view that reads a dropped column. Postgres binds a view's columns
//     (SELECT * included) when the view is created, and bytdb binds them
//     when it runs, so a column-level dependency would need the view's
//     column references resolved against its FROM list. Such a view
//     fails when queried.
//   - A primary-key column. Postgres drops the key with the column, but
//     a bytdb table always has a primary key (rows are stored by it), so
//     that drop stays refused.

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// ErrDependentObjects classifies a drop refused because other objects
// depend on its target and CASCADE was not given. Its SQLSTATE is
// Postgres's 2BP01 (dependent_objects_still_exist).
var ErrDependentObjects = errors.New("dependent objects still exist")

// DropOptions configures a dependency-aware drop (DropTables,
// DropColumnWith, DropIndexWith, DropViewWith).
type DropOptions struct {
	// Cascade drops the objects that depend on the target ("normal"
	// dependencies in the file comment) along with it. Without it the
	// drop runs as RESTRICT: such dependents refuse it with
	// ErrDependentObjects.
	Cascade bool

	// ViewReads returns the relation names a view's stored query reads,
	// in any order. It makes views dependents of the tables and views
	// they read. It is called inside the drop's transaction, once per
	// stored view, so it must not touch the engine. nil means views are
	// not considered.
	ViewReads func(query string) ([]string, error)

	// CheckReads returns the columns of table that a CHECK expression
	// mentions. A DROP COLUMN drops every CHECK that mentions the
	// column. It is called inside the transaction, like ViewReads, and
	// an error fails the drop rather than leave a CHECK naming a
	// missing column. nil leaves CHECKs alone.
	CheckReads func(table, expr string) ([]string, error)
}

// Dependent kinds.
const (
	DepConstraint = "constraint" // a foreign-key constraint; Dependent.Table names its table
	DepView       = "view"
)

// Dependent is one object a drop reaches through a normal dependency
// (see the file comment): a foreign-key constraint or a view. A RESTRICT
// drop lists these in its refusal, and a CASCADE drop returns the ones
// it removed.
type Dependent struct {
	Kind  string // DepConstraint or DepView
	Name  string
	Table string // the constraint's table; "" for a view
	// On is what the object depends on, worded as in Postgres's DETAIL:
	// "table p", "column code of table p", "index p_code_key", "view v".
	On string
}

// String renders the object as Postgres's dependency report does:
// "constraint c_pid_fkey on table c", "view v". Postgres leaves a
// constraint name bare and quotes a relation name when it needs it
// (`constraint Fk 5 on table "C5"`), and this matches.
func (d Dependent) String() string {
	if d.Kind == DepView {
		return "view " + QuoteIdent(d.Name)
	}
	return "constraint " + d.Name + " on table " + QuoteIdent(d.Table)
}

// errDependents is the RESTRICT refusal, in Postgres's wording. target
// is the dropped object as Postgres describes it ("table p"), or "" for
// a drop of several objects, which Postgres words without naming any
// ("cannot drop desired object(s) because other objects depend on
// them"). The DETAIL has one "<dependent> depends on <what>" line per
// dependent, and the HINT is Postgres's.
func errDependents(target string, deps []Dependent) error {
	lines := make([]string, len(deps))
	for i, d := range deps {
		lines[i] = d.String() + " depends on " + d.On
	}
	msg := "cannot drop " + target + " because other objects depend on it"
	if target == "" {
		msg = "cannot drop desired object(s) because other objects depend on them"
	}
	return KindErr(1, msg,
		[]error{ErrDependentObjects},
		"detail", strings.Join(lines, "\n"),
		"hint", "Use DROP ... CASCADE to drop the dependent objects too.")
}

// DropTables drops the named tables in one transaction: descriptors,
// rows, indexes, identity counters, and the tables' own constraints.
// References among the named tables don't block the drop, so
// DROP TABLE parent, child needs no CASCADE, as in Postgres.
//
// Foreign keys on other tables that reference a named table, and (with
// opts.ViewReads) views that read one, are dependents. Without
// opts.Cascade they refuse the drop with ErrDependentObjects. With it
// they are dropped too and returned: foreign keys by (table, name), then
// views in the order they were reached.
//
// Every name must exist. An IF EXISTS filter belongs to the caller.
func (e *Engine) DropTables(names []string, opts DropOptions) ([]Dependent, error) {
	names = dedupe(names)
	if len(names) == 0 {
		return nil, nil
	}
	// Outputs of the closure, reset on every run because updateDDL
	// retries it after an optimistic conflict (concurrent-writes mode).
	var droppedIDs []uint64
	var cascaded []Dependent
	err := e.updateDDL(func(tx *btypedb.Tx[string, []byte]) error {
		droppedIDs, cascaded = nil, nil
		dropSet := make(map[string]bool, len(names))
		descs := make([]*TableDesc, len(names))
		for i, n := range names {
			desc, err := e.descForDDL(tx, n)
			if err != nil {
				return err
			}
			descs[i] = desc
			dropSet[n] = true
		}

		// Normal dependents, part 1: foreign keys on surviving tables.
		// A table in the drop set takes its own foreign keys with it, so
		// its references (to the set or to itself) are not dependents.
		var deps []Dependent
		prefix := descTablePrefix()
		end := string(tuple.PrefixEnd([]byte(prefix)))
		for k := range tx.Ascend(prefix) {
			if k >= end {
				break
			}
			child := descKeyName(k)
			if dropSet[child] {
				continue
			}
			desc, err := e.tableFromView(tx, child)
			if err != nil {
				return err
			}
			if desc == nil {
				continue
			}
			for _, fk := range desc.ForeignKeys {
				if dropSet[fk.RefTable] {
					deps = append(deps, Dependent{Kind: DepConstraint, Name: fk.Name,
						Table: child, On: "table " + QuoteIdent(fk.RefTable)})
				}
			}
		}

		sortFKDependents(deps)

		// Part 2: views reading a dropped table, and views reading those.
		roots := make(map[string]string, len(names))
		for _, n := range names {
			roots[n] = "table " + QuoteIdent(n)
		}
		views, err := viewDependents(tx, roots, opts.ViewReads)
		if err != nil {
			return err
		}
		deps = append(deps, views...)

		if len(deps) > 0 && !opts.Cascade {
			// A multi-table drop's refusal names no table, as in
			// Postgres. The DETAIL lines show which table each
			// dependent needs.
			target := ""
			if len(names) == 1 {
				target = "table " + QuoteIdent(names[0])
			}
			return errDependents(target, deps)
		}
		if err := e.dropFKDependents(tx, deps, nil); err != nil {
			return err
		}
		if err := dropViewDependents(tx, deps); err != nil {
			return err
		}

		for _, desc := range descs {
			sp := tableSpace(desc.ID)
			if _, err := tx.DeleteRange(string(sp), string(tuple.PrefixEnd(sp))); err != nil {
				return err
			}
			// The table's identity counters live in the system sequences
			// table, outside its own key space.
			idPrefix := identitySeqTablePrefix(desc.ID)
			if _, err := tx.DeleteRange(string(idPrefix), string(tuple.PrefixEnd(idPrefix))); err != nil {
				return err
			}
			if _, err := tx.Delete(descKey(desc.Name)); err != nil {
				return err
			}
			droppedIDs = append(droppedIDs, desc.ID)
		}
		cascaded = deps
		return nil
	})
	if err != nil {
		return nil, serr.Wrap(err, "op", "drop table", "table", strings.Join(names, ", "))
	}
	// In concurrent-writes mode the tables' identity counters may have
	// live in-memory allocators. Drop them so no cached draw can write
	// the deleted keys back.
	for _, id := range droppedIDs {
		e.invalidateCounterPrefix(string(identitySeqTablePrefix(id)))
	}
	for _, n := range names {
		e.cacheEvict(n)
	}
	e.viewCount.Add(-int64(countKind(cascaded, DepView)))
	return cascaded, nil
}

// DropColumnWith removes a column and what depends on it, as Postgres's
// ALTER TABLE ... DROP COLUMN does. No rows are rewritten: the column's
// data stays in old row values under its retired ID, skipped on decode,
// and ages out as rows are updated. A later AddColumn with the same name
// gets a fresh ID, so the old data can never resurface.
//
// Always dropped with it, CASCADE or not (Postgres's auto dependencies):
//   - every index that includes the column, multi-column ones too, with
//     its entries;
//   - this table's foreign keys whose own columns include it;
//   - with opts.CheckReads, every CHECK that mentions it;
//   - its identity counter.
//
// Foreign keys whose referenced columns include it, on other tables or
// on this one, are dependents. They need opts.Cascade and are returned
// when dropped. A primary-key column cannot be dropped at all (see the
// file comment).
func (e *Engine) DropColumnWith(table, name string, opts DropOptions) ([]Dependent, error) {
	// Captured for the post-commit allocator invalidation below ("" when
	// the dropped column had no identity counter).
	var droppedCounterKey string
	var cascaded []Dependent
	err := e.updateDDL(func(tx *btypedb.Tx[string, []byte]) error {
		droppedCounterKey, cascaded = "", nil
		old, err := e.descForDDL(tx, table)
		if err != nil {
			return err
		}
		ord := old.ColIndex(name)
		if ord < 0 {
			return errNoSuchColumn(table, name)
		}
		if old.isPK(ord) {
			return serr.New("cannot drop a primary key column", "table", table, "column", name)
		}

		// Normal dependents: foreign keys referencing the column. A
		// self-reference whose own columns include the column too (a
		// column referencing itself) is auto-dropped below, and Postgres
		// lets an auto dependency win over a normal one.
		refs, err := e.referencingFKs(tx, table, false)
		if err != nil {
			return err
		}
		var deps []Dependent
		for _, r := range refs {
			if !slices.Contains(r.FK.RefCols, name) {
				continue
			}
			if r.Child.Name == table && slices.Contains(r.FK.Cols, ord) {
				continue
			}
			deps = append(deps, Dependent{Kind: DepConstraint, Name: r.FK.Name, Table: r.Child.Name,
				On: "column " + name + " of table " + QuoteIdent(table)})
		}
		sortFKDependents(deps)
		if len(deps) > 0 && !opts.Cascade {
			return errDependents("column "+name+" of table "+QuoteIdent(table), deps)
		}

		// CHECKs that mention the column. They are resolved before any
		// write, so a CHECK the callback cannot read fails the drop
		// with nothing staged.
		var dropChecks []string
		if opts.CheckReads != nil {
			for _, ck := range old.Checks {
				cols, err := opts.CheckReads(table, ck.Expr)
				if err != nil {
					return serr.Wrap(err, "constraint", ck.Name)
				}
				if slices.Contains(cols, name) {
					dropChecks = append(dropChecks, ck.Name)
				}
			}
		}

		if old.Columns[ord].Identity {
			// The counter goes with the column; the column ID is never
			// reused, so nothing can resurrect it.
			droppedCounterKey = identitySeqKey(old.ID, old.Columns[ord].ID)
			if _, err := tx.Delete(droppedCounterKey); err != nil {
				return err
			}
		}

		desc := old.clone() // clone() copies every slice the edits below touch
		// Auto: indexes over the column, with their entries.
		for _, ix := range old.Indexes {
			if slices.Contains(ix.Cols, ord) {
				p := indexPrefix(desc.ID, ix.ID)
				if _, err := tx.DeleteRange(string(p), string(tuple.PrefixEnd(p))); err != nil {
					return err
				}
			}
		}
		desc.Indexes = slices.DeleteFunc(desc.Indexes, func(ix IndexDesc) bool {
			return slices.Contains(ix.Cols, ord)
		})
		// Auto: this table's foreign keys over the column. A
		// self-referencing dependent (normal, CASCADE) is removed from
		// this same descriptor by dropFKDependents below.
		desc.ForeignKeys = slices.DeleteFunc(desc.ForeignKeys, func(fk FKDesc) bool {
			return slices.Contains(fk.Cols, ord)
		})
		desc.Checks = slices.DeleteFunc(desc.Checks, func(ck CheckDesc) bool {
			return slices.Contains(dropChecks, ck.Name)
		})
		if err := e.dropFKDependents(tx, deps, desc); err != nil {
			return err
		}

		desc.Columns = slices.Delete(desc.Columns, ord, ord+1)
		// Ordinal references above the removed column shift down by one.
		for i, p := range desc.PKCols {
			if p > ord {
				desc.PKCols[i] = p - 1
			}
		}
		for i := range desc.Indexes {
			shiftOrdinals(desc.Indexes[i].Cols, ord)
		}
		// Foreign-key child columns are stored as ordinals too, and must
		// shift with the same rule. A foreign key over the dropped column
		// is gone by now, but one over a column with a HIGHER ordinal
		// survives. Without the shift its Cols would keep pointing one
		// slot too high, silently enforcing the constraint against the
		// wrong column, or (when the child column was the last one)
		// indexing past desc.Columns and panicking.
		for i := range desc.ForeignKeys {
			shiftOrdinals(desc.ForeignKeys[i].Cols, ord)
		}
		cascaded = deps
		return writeDescIn(tx, table, desc)
	})
	if err != nil {
		return nil, serr.Wrap(err, "op", "drop column", "table", table, "column", name)
	}
	if droppedCounterKey != "" {
		// Concurrent-writes mode: no cached draw may write the deleted
		// counter key back.
		e.invalidateCounter(droppedCounterKey)
	}
	return cascaded, nil
}

// shiftOrdinals renumbers column ordinals after the column at ord was
// removed: every ordinal above it moves down by one. The slice must be
// the caller's own (a clone()'s).
func shiftOrdinals(ords []int, ord int) {
	for j, c := range ords {
		if c > ord {
			ords[j] = c - 1
		}
	}
}

// DropIndexWith removes a secondary index and its entries. A unique
// index can be what makes a foreign key's referenced columns unique.
// Foreign keys left with no unique key covering their referenced
// columns are dependents: they need opts.Cascade and are returned when
// dropped. A foreign key that another unique key still covers is not a
// dependent. Postgres ties each foreign key to one index, while bytdb
// only asks that some unique key cover the columns.
func (e *Engine) DropIndexWith(table, name string, opts DropOptions) ([]Dependent, error) {
	var cascaded []Dependent
	err := e.updateDDL(func(tx *btypedb.Tx[string, []byte]) error {
		cascaded = nil
		old, err := e.descForDDL(tx, table)
		if err != nil {
			return err
		}
		idx := old.Index(name)
		if idx == nil {
			return serr.New("no such index", "table", table, "index", name)
		}
		desc := old.clone() // clone() copies Indexes, so DeleteFunc is safe
		desc.Indexes = slices.DeleteFunc(desc.Indexes, func(x IndexDesc) bool { return x.Name == name })
		var deps []Dependent
		if idx.Unique {
			refs, err := e.referencingFKs(tx, table, false)
			if err != nil {
				return err
			}
			for _, r := range refs {
				if uniqueKeyCovers(old, r.FK.RefCols) && !uniqueKeyCovers(desc, r.FK.RefCols) {
					deps = append(deps, Dependent{Kind: DepConstraint, Name: r.FK.Name,
						Table: r.Child.Name, On: "index " + QuoteIdent(name)})
				}
			}
		}
		sortFKDependents(deps)
		if len(deps) > 0 && !opts.Cascade {
			return errDependents("index "+QuoteIdent(name), deps)
		}
		if err := e.dropFKDependents(tx, deps, desc); err != nil {
			return err
		}
		p := indexPrefix(desc.ID, idx.ID)
		if _, err := tx.DeleteRange(string(p), string(tuple.PrefixEnd(p))); err != nil {
			return err
		}
		cascaded = deps
		return writeDescIn(tx, table, desc)
	})
	if err != nil {
		return nil, serr.Wrap(err, "op", "drop index", "table", table, "index", name)
	}
	return cascaded, nil
}

// DropViewWith removes a view, reporting whether it existed. With
// opts.ViewReads, views that read it (directly or through other views)
// are dependents. They need opts.Cascade and are returned when dropped.
func (e *Engine) DropViewWith(name string, opts DropOptions) (bool, []Dependent, error) {
	existed := false
	var cascaded []Dependent
	err := e.updateDDL(func(tx *btypedb.Tx[string, []byte]) error {
		existed, cascaded = false, nil // the closure may re-run; see DropTables
		if !tx.Contains(viewKey(name)) {
			return nil
		}
		existed = true
		deps, err := viewDependents(tx, map[string]string{name: "view " + QuoteIdent(name)}, opts.ViewReads)
		if err != nil {
			return err
		}
		if len(deps) > 0 && !opts.Cascade {
			return errDependents("view "+QuoteIdent(name), deps)
		}
		if err := dropViewDependents(tx, deps); err != nil {
			return err
		}
		if _, err := tx.Delete(viewKey(name)); err != nil {
			return err
		}
		cascaded = deps
		return nil
	})
	if err != nil {
		return false, nil, serr.Wrap(err, "op", "drop view", "view", name)
	}
	if existed {
		e.viewCount.Add(-int64(1 + countKind(cascaded, DepView)))
	}
	return existed, cascaded, nil
}

// dropFKDependents removes the foreign-key dependents in deps from their
// tables' descriptors, within the drop's transaction. self, when not
// nil, is the descriptor the caller is already rewriting (the dropped
// column's or index's table). A dependent on that table is removed from
// it in place, since writing a fresh copy here would be overwritten by
// the caller's own write. Other tables are re-read with descForDDL,
// edited on a clone, and written back, one write per table.
func (e *Engine) dropFKDependents(tx *btypedb.Tx[string, []byte], deps []Dependent, self *TableDesc) error {
	byTable := map[string][]string{}
	var order []string // deterministic write order
	for _, d := range deps {
		if d.Kind != DepConstraint {
			continue
		}
		if _, seen := byTable[d.Table]; !seen {
			order = append(order, d.Table)
		}
		byTable[d.Table] = append(byTable[d.Table], d.Name)
	}
	for _, t := range order {
		drop := byTable[t]
		if self != nil && t == self.Name {
			self.ForeignKeys = slices.DeleteFunc(self.ForeignKeys,
				func(fk FKDesc) bool { return slices.Contains(drop, fk.Name) })
			continue
		}
		old, err := e.descForDDL(tx, t)
		if err != nil {
			return err
		}
		desc := old.clone()
		desc.ForeignKeys = slices.DeleteFunc(desc.ForeignKeys,
			func(fk FKDesc) bool { return slices.Contains(drop, fk.Name) })
		if err := writeDescIn(tx, t, desc); err != nil {
			return err
		}
	}
	return nil
}

// dropViewDependents deletes the view dependents in deps.
func dropViewDependents(tx *btypedb.Tx[string, []byte], deps []Dependent) error {
	for _, d := range deps {
		if d.Kind != DepView {
			continue
		}
		if _, err := tx.Delete(viewKey(d.Name)); err != nil {
			return err
		}
	}
	return nil
}

// viewDependents finds the views that read any of roots, directly or
// through other views. roots maps each dropped relation's name to how
// Postgres describes it ("table p", "view v"), which becomes a direct
// dependent's On. Views in roots are not their own dependents. That
// matters only for a cycle, which CREATE OR REPLACE VIEW can close.
//
// The search is breadth-first, so the views reading roots come first,
// then the views reading those, and so on. Within a level views come in
// name order (the views table's key order). A view is listed once,
// with On naming the first relation it reads in the level before it, in
// name order. reads == nil means views are not considered.
//
//	roots {p}      level 1             level 2
//	  table p  <-  view v2 (reads p)  <-  view v3 (reads v2)
//	  => [view v2 depends on table p, view v3 depends on view v2]
func viewDependents(v kvView, roots map[string]string, reads func(string) ([]string, error)) ([]Dependent, error) {
	if reads == nil {
		return nil, nil
	}
	// One pass over the views table: each view and what it reads. The
	// callback parses the stored text, so do it once per view rather
	// than once per level.
	type viewInfo struct {
		name  string
		reads []string
	}
	var all []viewInfo
	prefix := mustEncode(int64(sysViewTableID), int64(primaryIndexID))
	end := string(tuple.PrefixEnd(prefix))
	for k, blob := range v.Ascend(string(prefix)) {
		if k >= end {
			break
		}
		var vd ViewDesc
		if json.Unmarshal(blob, &vd) != nil {
			continue // unreadable rows are skipped, as Views() skips them
		}
		names, err := reads(vd.Query)
		if err != nil {
			return nil, serr.Wrap(err, "op", "read view dependencies", "view", vd.Name)
		}
		names = slices.Clone(names)
		slices.Sort(names) // a stable On when a view reads several dropped relations
		all = append(all, viewInfo{vd.Name, names})
	}
	if len(all) == 0 {
		return nil, nil
	}

	reached := make(map[string]bool, len(roots))
	frontier := make(map[string]string, len(roots)) // name -> its description as an On
	for n, d := range roots {
		reached[n] = true
		frontier[n] = d
	}
	var out []Dependent
	for len(frontier) > 0 {
		next := map[string]string{}
		for _, vi := range all {
			if reached[vi.name] {
				continue
			}
			for _, r := range vi.reads {
				if on, ok := frontier[r]; ok {
					out = append(out, Dependent{Kind: DepView, Name: vi.name, On: on})
					reached[vi.name] = true
					next[vi.name] = "view " + QuoteIdent(vi.name)
					break
				}
			}
		}
		frontier = next
	}
	return out, nil
}

// sortFKDependents orders foreign-key dependents by (table, name), so a
// refusal's DETAIL and a cascade's notices come out the same on every
// run. referencingFKs walks the tables in name order already, but a
// table's own constraint order is declaration order.
func sortFKDependents(deps []Dependent) {
	slices.SortStableFunc(deps, func(a, b Dependent) int {
		if c := strings.Compare(a.Table, b.Table); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
}

// countKind counts the dependents of one kind.
func countKind(deps []Dependent, kind string) int {
	n := 0
	for _, d := range deps {
		if d.Kind == kind {
			n++
		}
	}
	return n
}

// dedupe returns names without repeats, first occurrence kept. Postgres
// runs DROP TABLE t, t as one drop of t (checked on PG16).
func dedupe(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := names[:0:0]
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
