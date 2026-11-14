package sql

import "sync"

// catmemo.go: per-catalog-version memoization of system-table rows
// (deep-look D-47, second half).
//
// A catalog table's rows are built from the whole catalog: every
// table's descriptor, every view's derived shape, every sequence. An
// ORM introspecting its schema one table at a time issues one such
// query per table, so startup used to cost O(tables²) — 200 tables
// meant 200 full rebuilds of information_schema.columns. Nothing those
// rows say can change without DDL, and every DDL commit bumps the
// engine's catalog version (Engine.CatalogVersion), so the rows are
// built once per version and shared until the next schema change.
//
// Only tables marked byVersion are memoized. The flag is opt-in
// because a table whose rows read anything besides the committed
// catalog would go stale silently, while a forgotten flag costs only
// a rebuild. The tables that stay live are:
//
//   - pg_stat_activity: the serving layer's connection list.
//   - pg_settings: the session's own SET variables.
//   - pg_sequences: last_value moves with every nextval, which is not
//     DDL. (pg_sequence and information_schema.sequences report only a
//     sequence's options, which change only through ALTER SEQUENCE,
//     so those two are memoized.)
//
// The constant tables (pg_namespace, pg_type, pg_am, ...) are cheap to
// build and stay unmarked.
//
// Sharing: the memo hangs off the DB that New returns and rides every
// copy of it (sessions, per-statement copies), so all sessions of one
// DB share it. The cached slices are handed to the executor as-is,
// which already treats virtual-table rows as shared materializations:
// a CTE's rows are read by every reference to it, and the join steps
// copy a virtual row before extending it (see stepNext). Nothing
// writes through them, which is what makes handing one slice to many
// concurrent statements safe.

// catalogMemo holds the built rows of byVersion system tables for one
// catalog version.
type catalogMemo struct {
	mu   sync.Mutex
	ver  uint64
	rows map[*sysTableDef][][]any
}

// sysRows builds, or reuses, the rows of system table st.
//
// The version is read BEFORE the build, the protocol Engine.catalogVer
// documents: the engine bumps the version after a DDL commits, so rows
// built after reading v reflect a catalog at least as new as v. Stored
// under v, they may be newer than v (a DDL committed mid-build, its
// bump not yet visible), never older. A statement that still reads v
// gets the newer rows, which it could have seen anyway, and the bump
// sends every later statement to a rebuild.
func (d *DB) sysRows(st *sysTableDef) [][]any {
	if !st.byVersion || d.cat == nil {
		return st.rows(d)
	}
	ver := d.e.CatalogVersion()
	if rows, ok := d.cat.get(ver, st); ok {
		return rows
	}
	rows := st.rows(d)
	if rows == nil {
		// Normalize here so a hit and a miss return the same thing;
		// lookup would turn nil into an empty slice anyway.
		rows = [][]any{}
	}
	d.cat.put(ver, st, rows)
	return rows
}

// get returns st's rows memoized at version ver. A memo for an older
// version is dropped on the spot, so the rows of a superseded catalog
// stop holding memory after the first lookup that follows a DDL.
func (m *catalogMemo) get(ver uint64, st *sysTableDef) ([][]any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ver != ver {
		if m.ver < ver {
			m.ver, m.rows = ver, nil
		}
		return nil, false
	}
	rows, ok := m.rows[st]
	return rows, ok
}

// put records st's rows as built at version ver. Two statements that
// miss together both build and the second put wins; the rows are
// equivalent, so that costs only the duplicated work. A build that
// started before a DDL and finishes after a newer version was stored
// is discarded rather than allowed to roll the memo back.
func (m *catalogMemo) put(ver uint64, st *sysTableDef, rows [][]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case ver < m.ver:
		return
	case ver > m.ver:
		m.ver, m.rows = ver, nil
	}
	if m.rows == nil {
		m.rows = map[*sysTableDef][][]any{}
	}
	m.rows[st] = rows
}
