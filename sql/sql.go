// Package sql is bytdb's SQL frontend: a small Postgres-flavored
// dialect parsed, planned, and executed directly over an Engine.
//
// Supported statements:
//
//	CREATE TABLE t (id int PRIMARY KEY, name text, ...)
//	CREATE TABLE t (a int, b int, ..., PRIMARY KEY (a, b))
//	DROP TABLE t
//	ALTER TABLE t ADD [COLUMN] c type
//	ALTER TABLE t DROP [COLUMN] c
//	CREATE [UNIQUE] INDEX idx ON t (c, ...)
//	DROP INDEX idx [ON t]
//	INSERT INTO t [(c, ...)] VALUES (v, ...), ...
//	SELECT * | c, ... FROM t [WHERE ...] [ORDER BY c [ASC|DESC], ...] [LIMIT n] [OFFSET n]
//	UPDATE t SET c = v, ... [WHERE ...]
//	DELETE FROM t [WHERE ...]
//
// A WHERE clause is AND-ed predicates: column op literal (=, !=, <>,
// <, <=, >, >=, either operand order) or column IS [NOT] NULL. The
// planner turns equality and range predicates on a prefix of the
// primary key or of a secondary index into point gets or bounded
// ordered scans; every predicate is still re-checked per row, so
// pushdown only narrows what is visited.
//
// The dialect follows Postgres conventions: 'string' literals with ''
// escapes, "quoted" identifiers, unquoted identifiers folded to
// lowercase, -- and /* */ comments, and type aliases (int, integer,
// bigint, int8...; float, float8, real, double precision; text,
// varchar[(n)], string; bool, boolean; bytea, bytes).
//
// Each statement is atomic: a multi-row INSERT, an UPDATE, or a DELETE
// runs in one engine transaction and rolls back entirely on error.
package sql

import (
	"github.com/rohanthewiz/bytdb"
	"github.com/rohanthewiz/serr"
)

// DB executes SQL statements against a bytdb Engine.
type DB struct {
	e *bytdb.Engine
}

// New wraps an Engine for SQL execution.
func New(e *bytdb.Engine) *DB { return &DB{e: e} }

// Result is the outcome of one statement. SELECT fills Cols, Types,
// and Rows; INSERT, UPDATE, and DELETE fill RowsAffected; DDL fills
// nothing.
type Result struct {
	Cols         []string
	Types        []bytdb.ColType
	Rows         [][]any
	RowsAffected int
}

// Exec parses and executes one SQL statement.
func (d *DB) Exec(query string) (*Result, error) {
	st, err := Parse(query)
	if err != nil {
		return nil, err
	}
	switch s := st.(type) {
	case *CreateTable:
		cols := make([]bytdb.Column, len(s.Cols))
		for i, c := range s.Cols {
			cols[i] = bytdb.Column{Name: c.Name, Type: c.Type}
		}
		if _, err := d.e.CreateTable(s.Table, cols, s.PK...); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *DropTable:
		if err := d.e.DropTable(s.Table); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *AddColumn:
		if err := d.e.AddColumn(s.Table, bytdb.Column{Name: s.Col.Name, Type: s.Col.Type}); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *DropColumn:
		if err := d.e.DropColumn(s.Table, s.Col); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *CreateIndex:
		if _, err := d.e.CreateIndex(s.Table, s.Name, s.Unique, s.Cols...); err != nil {
			return nil, err
		}
		return &Result{}, nil
	case *DropIndex:
		return d.execDropIndex(s)
	case *Insert:
		return d.execInsert(s)
	case *Select:
		return d.execSelect(s)
	case *Update:
		return d.execUpdate(s)
	case *Delete:
		return d.execDelete(s)
	}
	return nil, serr.New("unhandled statement type")
}
