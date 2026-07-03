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

// BoolExpr is a WHERE or HAVING condition: a tree of AND/OR/NOT over
// Pred leaves, evaluated with SQL three-valued logic (a comparison
// against NULL is unknown; a row or group matches only when the tree
// is definitely true).
type BoolExpr interface{ boolExpr() }

// And and Or are n-ary: the parser flattens chains of the same
// operator.
type And struct{ Exprs []BoolExpr }
type Or struct{ Exprs []BoolExpr }
type Not struct{ Expr BoolExpr }

// Pred is a leaf predicate: item op literal, or item IS [NOT] NULL.
// The item is a plain column in WHERE; in HAVING it may also be an
// aggregate call. One side must be the item and the other a literal —
// a reversed comparison is normalized at parse time.
type Pred struct {
	Item SelectItem
	Op   PredOp
	Val  any // literal value; nil for IS [NOT] NULL
}

func (*And) boolExpr()  {}
func (*Or) boolExpr()   {}
func (*Not) boolExpr()  {}
func (*Pred) boolExpr() {}

// walkPreds visits every leaf predicate in the tree.
func walkPreds(e BoolExpr, visit func(*Pred) error) error {
	switch n := e.(type) {
	case nil:
		return nil
	case *Pred:
		return visit(n)
	case *Not:
		return walkPreds(n.Expr, visit)
	case *And:
		for _, sub := range n.Exprs {
			if err := walkPreds(sub, visit); err != nil {
				return err
			}
		}
	case *Or:
		for _, sub := range n.Exprs {
			if err := walkPreds(sub, visit); err != nil {
				return err
			}
		}
	}
	return nil
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
	Where   BoolExpr // nil: all rows
	GroupBy []string
	Having  BoolExpr // nil: all groups; leaves may be aggregates
	OrderBy []OrderItem
	Limit   int64 // -1: no limit
	Offset  int64
}

// Update is UPDATE t SET col = lit, ... [WHERE ...].
type Update struct {
	Table string
	Set   map[string]any
	Where BoolExpr
}

// Delete is DELETE FROM t [WHERE ...].
type Delete struct {
	Table string
	Where BoolExpr
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
