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

// AggFunc is an aggregate function in a select list, HAVING, or
// ORDER BY.
type AggFunc int

const (
	AggNone AggFunc = iota
	AggCount
	AggSum
	AggAvg
	AggMin
	AggMax
)

var aggNames = map[string]AggFunc{
	"count": AggCount, "sum": AggSum, "avg": AggAvg, "min": AggMin, "max": AggMax,
}

func (f AggFunc) name() string {
	for n, fn := range aggNames {
		if fn == f {
			return n
		}
	}
	return ""
}

// SelectItem is one select-list entry: a plain column, or an
// aggregate over a column (COUNT(*) sets Star).
type SelectItem struct {
	Col  string
	Agg  AggFunc
	Star bool // COUNT(*)
}

// AggPred is one HAVING conjunct: item op literal, or item IS [NOT]
// NULL, where the item may be an aggregate or a grouped column.
type AggPred struct {
	Item SelectItem
	Op   PredOp
	Val  any
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

// OrderItem is one ORDER BY key: a column, or (in an aggregate
// query) an aggregate call.
type OrderItem struct {
	SelectItem
	Desc bool
}

// Select is a single-table SELECT. A query with any aggregate item,
// a GROUP BY, or a HAVING is an aggregate query.
type Select struct {
	Table   string
	Star    bool
	Items   []SelectItem
	Where   []Pred
	GroupBy []string
	Having  []AggPred
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
