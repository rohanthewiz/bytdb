package sql

// rowbudget.go: the per-materialization row budget. Every place the
// executor accumulates rows in memory — a SELECT's collected rows, a
// window query's base rows, GROUP BY's groups, a hash join's build
// table, a subquery's value set, a UNION's concatenation, RETURNING's
// output — charges one budget as it grows, so a single statement
// cannot allocate without bound between the engine's own checks.
//
//	append row ──▶ budget.check(len)
//	                 │
//	                 ├─ len > MaxResultRows  ──▶ 54000 "result row limit"
//	                 └─ every memCheckEvery  ──▶ Engine.CheckMemory
//	                                             (53200 when over the ceiling)
//
// The budget is per materialization, not per statement: a view
// materialized and then joined is two row sets, each held to the cap.
// That keeps the check a pure function of one slice's length (no shared
// counter threaded through every executor), and it matches what the cap
// is protecting against — one row set growing without bound. A
// statement can still hold several capped sets at once; the memory
// ceiling, polled from the same calls, is what bounds their sum.

import (
	"strconv"

	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// memCheckEvery is how many accumulated rows pass between memory-
// ceiling polls. CheckMemory is already cheap (a cached verdict), but
// the row-cap comparison runs on every row and this keeps the poll off
// that hot path entirely; at a few hundred bytes per row, 1024 rows is
// well under a megabyte of growth between polls.
const memCheckEvery = 1024

// rowBudget polices one in-memory row set. The zero value (no engine)
// never refuses, which is what catalog-shape derivation wants.
type rowBudget struct {
	e     *bytdb.Engine
	limit int64 // from Engine.MaxResultRows at the materialization's start; <= 0: no cap
	noMem bool  // skip the memory-ceiling poll (DB.freeing)
}

// budget starts a row budget for one materialization. The cap is read
// once here, so a concurrent SetMaxResultRows never applies halfway
// through a row set.
func (d *DB) budget() rowBudget {
	if d == nil || d.e == nil {
		return rowBudget{}
	}
	return rowBudget{e: d.e, limit: d.e.MaxResultRows(), noMem: d.freeing}
}

// check is called with the row set's size after each append. It
// returns the error that should abort the statement, or nil.
func (b rowBudget) check(n int) error {
	if b.e == nil {
		return nil
	}
	if b.limit > 0 && int64(n) > b.limit {
		return resultTooLargeErr(b.limit)
	}
	if !b.noMem && n%memCheckEvery == 0 {
		return b.e.CheckMemory()
	}
	return nil
}

// resultTooLargeErr is the row-cap refusal. pgwire keys SQLSTATE 54000
// (program_limit_exceeded) off the "result row limit" phrase, so keep
// it stable. The message names the remedies in the order a caller
// should reach for them: narrow or paginate the query first, raise the
// cap only for a query that really needs that many rows in memory.
func resultTooLargeErr(limit int64) error {
	return serr.New("query would hold more rows in memory than the result row limit allows; "+
		"narrow it with WHERE, add a LIMIT, or paginate — or raise the limit "+
		"(Engine.SetMaxResultRows; bytdbd -max-result-rows)",
		"maxResultRows", strconv.FormatInt(limit, 10))
}
