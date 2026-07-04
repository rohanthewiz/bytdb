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

// Param is a $n placeholder (1-based), parsed wherever a literal may
// appear and replaced by the n-th bound argument at execution.
type Param int

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

// Pred is a leaf predicate: item op literal, item op item, or item
// IS [NOT] NULL. Items are plain columns in WHERE and ON; in HAVING
// they may also be aggregate calls. A literal-first comparison is
// normalized at parse time, so the left side is always an item.
type Pred struct {
	Item  SelectItem
	Op    PredOp
	Val   any         // literal right side; nil for IS [NOT] NULL or when RItem is set
	RItem *SelectItem // column (in HAVING: aggregate) right side; nil when comparing with Val
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

// ColRef names a column, optionally qualified by a table name or
// alias: c or t.c.
type ColRef struct {
	Table string // qualifier; "" when unqualified
	Name  string
}

func (c ColRef) String() string {
	if c.Table == "" {
		return c.Name
	}
	return c.Table + "." + c.Name
}

// SelectItem is one select-list entry: a plain column, an aggregate
// over a column, COUNT(*) (Agg with Star), t.* (Star with Col.Table,
// select lists only), or a literal (IsLit; select lists only —
// parsed literals and folded zero-argument functions like version()).
type SelectItem struct {
	Col     ColRef
	Agg     AggFunc
	Star    bool
	IsLit   bool
	Lit     any
	LitName string // output column name; "?column?" for plain literals
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

// JoinType is how a FROM item combines with the tables before it.
type JoinType int

const (
	JoinInner JoinType = iota
	JoinLeft           // LEFT [OUTER] JOIN: unmatched left rows extend with NULLs
	JoinCross          // CROSS JOIN, or a comma-separated table
)

// FromItem is one table of a FROM clause. Joins are left-deep: each
// item after the first joins to the combination of all items before
// it. The first item's Join and On are unset; CROSS JOIN has no On.
type FromItem struct {
	Table string
	Alias string // "" : referenced by the table name
	Join  JoinType
	On    BoolExpr
}

// Select is SELECT over one table or a chain of joins. A query with
// any aggregate item, a GROUP BY, or a HAVING is an aggregate query.
type Select struct {
	From    []FromItem
	Star    bool
	Items   []SelectItem
	Where   BoolExpr // nil: all rows
	GroupBy []ColRef
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
