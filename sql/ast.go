package sql

import "github.com/rohanthewiz/bytdb"

// Statement is one parsed SQL statement.
type Statement interface{ stmt() }

// PredOp is a predicate's comparison operator.
type PredOp int

const (
	OpEQ PredOp = iota
	OpNE
	OpLT
	OpLE
	OpGT
	OpGE
	OpIsNull
	OpIsNotNull
)

// Pred is one WHERE conjunct: column op literal, or column IS [NOT]
// NULL. The grammar allows only AND-ed predicates with a column on one
// side and a literal on the other (a reversed comparison is normalized
// at parse time), so a WHERE clause is a flat slice.
type Pred struct {
	Col string
	Op  PredOp
	Val any // literal value; nil for IS [NOT] NULL
}

// ColDef is one column of a CREATE TABLE or ALTER TABLE ADD COLUMN.
type ColDef struct {
	Name string
	Type bytdb.ColType
}

// CreateTable is CREATE TABLE t (col type [PRIMARY KEY], ...,
// [PRIMARY KEY (col, ...)]).
type CreateTable struct {
	Table string
	Cols  []ColDef
	PK    []string
}

// DropTable is DROP TABLE t.
type DropTable struct{ Table string }

// AddColumn is ALTER TABLE t ADD [COLUMN] col type.
type AddColumn struct {
	Table string
	Col   ColDef
}

// DropColumn is ALTER TABLE t DROP [COLUMN] col.
type DropColumn struct{ Table, Col string }

// CreateIndex is CREATE [UNIQUE] INDEX name ON t (col, ...).
type CreateIndex struct {
	Name   string
	Table  string
	Unique bool
	Cols   []string
}

// DropIndex is DROP INDEX name [ON t]. With no ON clause the index is
// resolved by name across tables.
type DropIndex struct{ Name, Table string }

// Insert is INSERT INTO t [(cols)] VALUES (lit, ...), .... Values are
// parsed literals: int64, float64, string, bool, or nil.
type Insert struct {
	Table string
	Cols  []string // nil: values in declared column order
	Rows  [][]any
}

// OrderItem is one ORDER BY key.
type OrderItem struct {
	Col  string
	Desc bool
}

// Select is a single-table SELECT.
type Select struct {
	Table   string
	Star    bool
	Cols    []string
	Where   []Pred
	OrderBy []OrderItem
	Limit   int64 // -1: no limit
	Offset  int64
}

// Update is UPDATE t SET col = lit, ... [WHERE ...].
type Update struct {
	Table string
	Set   map[string]any
	Where []Pred
}

// Delete is DELETE FROM t [WHERE ...].
type Delete struct {
	Table string
	Where []Pred
}

func (*CreateTable) stmt() {}
func (*DropTable) stmt()   {}
func (*AddColumn) stmt()   {}
func (*DropColumn) stmt()  {}
func (*CreateIndex) stmt() {}
func (*DropIndex) stmt()   {}
func (*Insert) stmt()      {}
func (*Select) stmt()      {}
func (*Update) stmt()      {}
func (*Delete) stmt()      {}
