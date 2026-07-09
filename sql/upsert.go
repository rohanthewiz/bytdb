package sql

// upsert.go: INSERT ... ON CONFLICT (DO NOTHING | DO UPDATE). The
// engine needs no new machinery: a conflict is found by probing the
// primary key (Get) or a unique index (a point-bounded index scan)
// with the proposed row's key values, and DO UPDATE rides the same
// UpdateReturning path as UPDATE. DO UPDATE expressions evaluate over
// a two-table scope — the target row then the excluded pseudo-row
// (the row that would have been inserted) — with bare column
// references pre-qualified to the target at parse time.

import (
	"slices"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/bytdb/tuple"
	"github.com/rohanthewiz/serr"
)

// arbiter is one uniqueness constraint a proposed row is probed
// against: the primary key (index nil) or a unique index.
type arbiter struct {
	index *bytdb.IndexDesc
}

// upsert is a prepared ON CONFLICT clause for one INSERT execution.
type upsert struct {
	oc       *OnConflict
	table    string
	desc     *bytdb.TableDesc
	arbiters []arbiter

	// DO UPDATE state, unset for DO NOTHING.
	env      *exEnv         // two-table scope: target, then excluded
	checkEnv *exEnv         // single-table scope for CHECK evaluation
	b        binds          // Where bound over env's scope
	setOrds  map[string]int // SET target ordinals
	setVals  map[string]any // literal SET values coerced for CHECK rows
	checks   []namedCheck
	// touched tracks rows this statement inserted or conflict-updated,
	// by encoded primary key: DO UPDATE reaching one a second time is
	// Postgres's cardinality violation, not a silent double update.
	touched map[string]bool
}

// prepareUpsert resolves an INSERT's ON CONFLICT clause inside the
// statement's transaction; nil clause prepares to nil.
func (d *DB) prepareUpsert(tx *bytdb.Txn, table string, desc *bytdb.TableDesc, oc *OnConflict, checks []namedCheck) (*upsert, error) {
	if oc == nil {
		return nil, nil
	}
	u := &upsert{oc: oc, table: table, desc: desc, checks: checks}
	if err := u.resolveArbiters(); err != nil {
		return nil, err
	}
	if !oc.Update {
		return u, nil
	}
	u.touched = map[string]bool{}
	u.checkEnv = d.tableEnv(tx, table, desc)
	w := len(desc.Columns)
	u.env = &exEnv{d: d, tx: tx, sc: &scope{
		tables: []scopeTable{
			{name: table, desc: desc},
			{name: "excluded", desc: desc, off: w},
		},
		width: 2 * w,
	}}
	u.b = binds{}
	if err := u.b.bind(oc.Where, u.env.sc); err != nil {
		return nil, err
	}
	u.setOrds = map[string]int{}
	for col := range oc.Set {
		ord := desc.ColIndex(col)
		if ord < 0 {
			return nil, serr.New("no such column", "table", table, "column", col)
		}
		u.setOrds[col] = ord
	}
	for col := range oc.SetEx {
		ord := desc.ColIndex(col)
		if ord < 0 {
			return nil, serr.New("no such column", "table", table, "column", col)
		}
		u.setOrds[col] = ord
	}
	// Literal SET values coerce once for CHECK rows, exactly as
	// execUpdate does (see the comment there).
	u.setVals = oc.Set
	if len(checks) > 0 {
		u.setVals = make(map[string]any, len(oc.Set))
		for col, v := range oc.Set {
			cv, err := coerceLit(v, desc.Columns[u.setOrds[col]].Type)
			if err != nil {
				return nil, serr.Wrap(err, "table", table, "column", col)
			}
			u.setVals[col] = cv
		}
	}
	return u, nil
}

// resolveArbiters maps the conflict target onto the table's
// uniqueness constraints. Without a target (DO NOTHING only — the
// parser enforces that) every constraint arbitrates; with one, the
// column set must match the primary key's or a unique index's, as
// Postgres infers.
func (u *upsert) resolveArbiters() error {
	if u.oc.TargetCols == nil {
		u.arbiters = []arbiter{{}}
		for i := range u.desc.Indexes {
			if u.desc.Indexes[i].Unique {
				u.arbiters = append(u.arbiters, arbiter{index: &u.desc.Indexes[i]})
			}
		}
		return nil
	}
	want := make([]int, len(u.oc.TargetCols))
	for i, name := range u.oc.TargetCols {
		ord := u.desc.ColIndex(name)
		if ord < 0 {
			return serr.New("no such column", "table", u.table, "column", name)
		}
		want[i] = ord
	}
	if sameColSet(want, u.desc.PKCols) {
		u.arbiters = []arbiter{{}}
		return nil
	}
	for i := range u.desc.Indexes {
		idx := &u.desc.Indexes[i]
		if idx.Unique && sameColSet(want, idx.Cols) {
			u.arbiters = []arbiter{{index: idx}}
			return nil
		}
	}
	return serr.New("there is no unique or exclusion constraint matching the ON CONFLICT specification",
		"table", u.table)
}

// sameColSet reports whether two ordinal lists name the same column
// set — conflict targets infer order-insensitively, as in Postgres.
func sameColSet(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := slices.Clone(a), slices.Clone(b)
	slices.Sort(as)
	slices.Sort(bs)
	return slices.Equal(as, bs)
}

// conflict probes the arbiters with a proposed full-width row and
// returns the existing row the insert would collide with. A NULL in
// a key position cannot conflict: NULLs never match in a unique
// index, and a NULL primary key column means an identity draw (or an
// error) at insert, never a collision.
func (u *upsert) conflict(tx *bytdb.Txn, vals []any) (bytdb.Row, bool, error) {
	for _, a := range u.arbiters {
		row, ok, err := u.probe(tx, a, vals)
		if err != nil || ok {
			return row, ok, err
		}
	}
	return bytdb.Row{}, false, nil
}

func (u *upsert) probe(tx *bytdb.Txn, a arbiter, vals []any) (bytdb.Row, bool, error) {
	cols := u.desc.PKCols
	if a.index != nil {
		cols = a.index.Cols
	}
	key := make([]any, len(cols))
	for i, ord := range cols {
		if vals[ord] == nil {
			return bytdb.Row{}, false, nil
		}
		key[i] = vals[ord]
	}
	if a.index == nil {
		return tx.Get(u.table, key...)
	}
	// A unique index has at most one row per full non-NULL key: scan
	// from the key and check whether the first row actually carries it.
	var hit bytdb.Row
	found := false
	for row, err := range tx.ScanIndex(u.table, a.index.Name, key, nil) {
		if err != nil {
			return bytdb.Row{}, false, err
		}
		for i, ord := range cols {
			if c, ok := compareVals(row.Vals[ord], key[i]); !ok || c != 0 {
				return bytdb.Row{}, false, nil
			}
		}
		hit, found = row, true
		break
	}
	return hit, found, nil
}

// markInserted records a row this statement inserted, so a later
// VALUES row conflicting with it cannot DO UPDATE it (Postgres's
// "cannot affect row a second time"). DO NOTHING tracks nothing —
// skipping is idempotent.
func (u *upsert) markInserted(stored []any) error {
	if !u.oc.Update {
		return nil
	}
	k, err := u.pkKey(stored)
	if err != nil {
		return err
	}
	u.touched[k] = true
	return nil
}

func (u *upsert) pkKey(vals []any) (string, error) {
	pk := make([]any, len(u.desc.PKCols))
	for i, ord := range u.desc.PKCols {
		pk[i] = vals[ord]
	}
	buf, err := tuple.Append(nil, pk...)
	if err != nil {
		return "", serr.Wrap(err, "op", "encode conflict key")
	}
	return string(buf), nil
}

// resolve applies DO UPDATE to the existing row a proposed row
// collided with. It returns the stored post-update row and whether an
// update happened — false for a WHERE the conflicting pair does not
// satisfy (the row is skipped, not counted, not returned, as in
// Postgres). The caller has already handled DO NOTHING.
func (u *upsert) resolve(tx *bytdb.Txn, existing bytdb.Row, proposed []any) (bytdb.Row, bool, error) {
	k, err := u.pkKey(existing.Vals)
	if err != nil {
		return bytdb.Row{}, false, err
	}
	if u.touched[k] {
		return bytdb.Row{}, false, serr.New("ON CONFLICT DO UPDATE command cannot affect row a second time",
			"table", u.table)
	}
	// The combined row the clause's expressions see: target columns,
	// then the excluded pseudo-row. An identity column the insert left
	// NULL is NULL in excluded — the draw never happened.
	combined := make([]any, 0, u.env.sc.width)
	combined = append(combined, existing.Vals...)
	combined = append(combined, proposed...)
	if u.oc.Where != nil {
		t, err := evalPreds(u.oc.Where, u.b, u.env, combined)
		if err != nil {
			return bytdb.Row{}, false, err
		}
		if t != triTrue {
			return bytdb.Row{}, false, nil
		}
	}
	// Mirror execUpdate: expressions evaluate against the pre-update
	// state, string results coerce like quoted literals, and the
	// literal map rides through for the engine's own coercion.
	set := u.oc.Set
	if len(u.oc.SetEx) > 0 {
		set = make(map[string]any, len(u.oc.Set)+len(u.oc.SetEx))
		for col, v := range u.oc.Set {
			set[col] = v
		}
		for col, ex := range u.oc.SetEx {
			rowEnv := *u.env
			rowEnv.row = combined
			v, err := evalEx(&rowEnv, ex)
			if err != nil {
				return bytdb.Row{}, false, err
			}
			cv, err := coerceLit(v, u.desc.Columns[u.setOrds[col]].Type)
			if err != nil {
				return bytdb.Row{}, false, serr.Wrap(err, "table", u.table, "column", col)
			}
			set[col] = cv
		}
	}
	if len(u.checks) > 0 {
		nv := slices.Clone(existing.Vals)
		for col, v := range u.setVals {
			nv[u.setOrds[col]] = v
		}
		for col := range u.oc.SetEx {
			nv[u.setOrds[col]] = set[col]
		}
		if err := checkRow(u.checkEnv, u.table, u.checks, nv); err != nil {
			return bytdb.Row{}, false, err
		}
	}
	pk := make([]any, len(u.desc.PKCols))
	for i, ord := range u.desc.PKCols {
		pk[i] = existing.Vals[ord]
	}
	stored, ok, err := tx.UpdateReturning(u.table, pk, set)
	if err != nil {
		return bytdb.Row{}, false, err
	}
	if !ok { // the probe just saw it; a vanish mid-statement is internal
		return bytdb.Row{}, false, serr.New("conflicting row disappeared during upsert", "table", u.table)
	}
	u.touched[k] = true
	// A SET that moved the primary key must count the new location as
	// touched too; the old key stays marked, harmlessly.
	nk, err := u.pkKey(stored.Vals)
	if err != nil {
		return bytdb.Row{}, false, err
	}
	u.touched[nk] = true
	return stored, true, nil
}
