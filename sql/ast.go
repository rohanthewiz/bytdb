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
	OpRegex     // ~   POSIX regex match
	OpNotRegex  // !~
	OpRegexI    // ~*  case-insensitive
	OpNotRegexI // !~*
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

// Cond is a leaf evaluated by the general expression evaluator: any
// condition the legacy Pred shape cannot express (function calls,
// CASE, IN, casts, subqueries, bare boolean operands, ...). Cond
// leaves never push into scans; they filter rows after retrieval.
type Cond struct{ Ex Expr }

func (*And) boolExpr()  {}
func (*Or) boolExpr()   {}
func (*Not) boolExpr()  {}
func (*Pred) boolExpr() {}
func (*Cond) boolExpr() {}

// Expr is a general scalar expression, evaluated per row with
// eval-time name resolution (an unresolved column falls back to the
// enclosing query's scope, which is what makes correlated subqueries
// work). Boolean operators are expressions too, so one grammar parses
// every context; simple shapes are lowered back to Pred/SelectItem
// after parsing to keep index pushdown and literal coercion.
type Expr interface{ expr() }

// ExLit is a literal value or a $n Param; Name is the output column
// name a folded system function carries ("" reads as "?column?").
type ExLit struct {
	Val  any
	Name string
}

// ExCol is a column reference.
type ExCol struct{ Col ColRef }

// ExAgg is an aggregate call inside an expression; only supported
// where aggregates may appear (it errors elsewhere). The argument is
// Col (a plain column), Star (COUNT(*)), or Arg (a general
// expression, evaluated per input row). Distinct makes the aggregate
// consume each distinct non-NULL argument value once (COUNT(DISTINCT
// x), SUM(DISTINCT x), ...).
type ExAgg struct {
	Fn       AggFunc
	Col      ColRef
	Star     bool
	Arg      Expr
	Distinct bool
}

// ExAnd / ExOr / ExNot are boolean operators within an expression.
type ExAnd struct{ Exprs []Expr }
type ExOr struct{ Exprs []Expr }
type ExNot struct{ E Expr }

// ExCmp compares two subexpressions (including the regex operators).
type ExCmp struct {
	Op   PredOp
	L, R Expr
}

// ExAny is L op ANY(R) or, when All is set, L op ALL(R). R is an
// ARRAY[...] constructor, a subquery, or a value that reads as an
// array (a []any or a Postgres '{...}' array-literal string). ANY is
// true when the comparison holds for some element; ALL when it holds
// for every element.
type ExAny struct {
	Op   PredOp
	L, R Expr
	All  bool
}

// ExArray is the ARRAY[e1, e2, ...] constructor. bytdb has no
// first-class array type; the node exists so the right-hand side of
// op ANY(...) / op ALL(...) can be written as a list of elements.
type ExArray struct{ Elems []Expr }

// ExIsNull is E IS [NOT] NULL.
type ExIsNull struct {
	E   Expr
	Not bool
}

// ExIn is E [NOT] IN (list...).
type ExIn struct {
	E    Expr
	List []Expr
	Not  bool
}

// ExCase is both CASE forms: with Operand, each When compares = to
// it; without, each When is a boolean condition. A missing ELSE is
// NULL.
type ExCase struct {
	Operand Expr
	Whens   []ExWhen
	Else    Expr
}

type ExWhen struct{ When, Then Expr }

// ExFunc is a function call by (lowercased, unqualified) name.
// Unknown functions parse fine and error only if evaluated.
type ExFunc struct {
	Name string
	Args []Expr
}

// ExCast is E::type; Type is the lowercased base name, keeping a
// trailing "[]" for array types (which parse but do not evaluate).
type ExCast struct {
	E    Expr
	Type string
}

// ExIndex is E[Idx]; parses, does not evaluate (no arrays).
type ExIndex struct{ E, Idx Expr }

// ExArith is arithmetic or string concatenation: + - * / % ||.
// A unary minus parses as 0 - E.
type ExArith struct {
	Op   string
	L, R Expr
}

// ExSub is a scalar subquery, executed lazily per row with the
// enclosing scopes visible for correlation.
type ExSub struct{ Sel *Select }

// walkExpr visits e and its subexpressions pre-order; visit returning
// false skips a node's children. A subquery's own expressions are not
// entered — an *ExSub is visited as a leaf (callers that care recurse
// into its Select themselves).
func walkExpr(e Expr, visit func(Expr) bool) {
	if e == nil || !visit(e) {
		return
	}
	switch n := e.(type) {
	case *ExAgg:
		walkExpr(n.Arg, visit)
	case *ExAnd:
		for _, sub := range n.Exprs {
			walkExpr(sub, visit)
		}
	case *ExOr:
		for _, sub := range n.Exprs {
			walkExpr(sub, visit)
		}
	case *ExNot:
		walkExpr(n.E, visit)
	case *ExCmp:
		walkExpr(n.L, visit)
		walkExpr(n.R, visit)
	case *ExAny:
		walkExpr(n.L, visit)
		walkExpr(n.R, visit)
	case *ExIsNull:
		walkExpr(n.E, visit)
	case *ExIn:
		walkExpr(n.E, visit)
		for _, sub := range n.List {
			walkExpr(sub, visit)
		}
	case *ExCase:
		walkExpr(n.Operand, visit)
		for _, w := range n.Whens {
			walkExpr(w.When, visit)
			walkExpr(w.Then, visit)
		}
		walkExpr(n.Else, visit)
	case *ExFunc:
		for _, a := range n.Args {
			walkExpr(a, visit)
		}
	case *ExCast:
		walkExpr(n.E, visit)
	case *ExIndex:
		walkExpr(n.E, visit)
		walkExpr(n.Idx, visit)
	case *ExArray:
		for _, sub := range n.Elems {
			walkExpr(sub, visit)
		}
	case *ExWindow:
		walkExpr(n.Arg, visit)
		walkExpr(n.Offset, visit)
		walkExpr(n.Default, visit)
		for _, sub := range n.Partition {
			walkExpr(sub, visit)
		}
		for _, o := range n.OrderBy {
			walkExpr(o.Ex, visit)
		}
		if n.Frame != nil {
			walkExpr(n.Frame.Start.Offset, visit)
			walkExpr(n.Frame.End.Offset, visit)
		}
	case *ExArith:
		walkExpr(n.L, visit)
		walkExpr(n.R, visit)
	}
}

func (*ExLit) expr()    {}
func (*ExCol) expr()    {}
func (*ExAgg) expr()    {}
func (*ExAnd) expr()    {}
func (*ExOr) expr()     {}
func (*ExNot) expr()    {}
func (*ExCmp) expr()    {}
func (*ExAny) expr()    {}
func (*ExIsNull) expr() {}
func (*ExIn) expr()     {}
func (*ExCase) expr()   {}
func (*ExFunc) expr()   {}
func (*ExCast) expr()   {}
func (*ExIndex) expr()  {}
func (*ExArray) expr()  {}
func (*ExWindow) expr() {}
func (*ExArith) expr()  {}
func (*ExSub) expr()    {}

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

// WinFunc identifies a window-only function: the ranking family
// (argument-free row counters) and the value family (functions that
// surface another row of the partition — LAG/LEAD and friends).
// Aggregate windows (SUM(x) OVER ...) carry an AggFunc instead.
type WinFunc int

const (
	WinNone WinFunc = iota
	WinRowNumber
	WinRank
	WinDenseRank
	WinLag
	WinLead
	WinFirstValue
	WinLastValue
	WinNthValue
)

var winNames = map[string]WinFunc{
	"row_number": WinRowNumber, "rank": WinRank, "dense_rank": WinDenseRank,
	"lag": WinLag, "lead": WinLead,
	"first_value": WinFirstValue, "last_value": WinLastValue, "nth_value": WinNthValue,
}

// winArity gives a window-only function's argument-count range. The
// parser validates counts here so the executor can trust the shape:
// Arg is always set for the value family, and Offset only where it
// means something (LAG/LEAD's offset, NTH_VALUE's n).
func winArity(f WinFunc) (lo, hi int) {
	switch f {
	case WinLag, WinLead:
		return 1, 3 // value [, offset [, default]]
	case WinFirstValue, WinLastValue:
		return 1, 1
	case WinNthValue:
		return 2, 2 // value, n
	}
	return 0, 0 // ranking family
}

func (f WinFunc) name() string {
	for n, fn := range winNames {
		if fn == f {
			return n
		}
	}
	return ""
}

// FrameMode selects how an explicit window frame measures its bounds:
// ROWS counts physical rows, GROUPS counts peer groups (rows tying on
// the window ORDER BY), and RANGE is peer-aware — its CURRENT ROW
// bound covers the whole peer group, and its offset bounds are
// distances measured on the (single, numeric) ORDER BY key rather
// than row counts.
type FrameMode int

const (
	FrameRange FrameMode = iota // the SQL default mode
	FrameRows
	FrameGroups
)

func (m FrameMode) name() string {
	switch m {
	case FrameRows:
		return "ROWS"
	case FrameGroups:
		return "GROUPS"
	}
	return "RANGE"
}

// FrameBoundType is one endpoint kind of a window frame. The order is
// meaningful: it matches the direction a frame extends, so validity
// checks read as ordering (a start bound must not sort after its end
// bound — the parser enforces the specific illegal pairs).
type FrameBoundType int

const (
	BoundUnboundedPreceding FrameBoundType = iota
	BoundOffsetPreceding
	BoundCurrentRow
	BoundOffsetFollowing
	BoundUnboundedFollowing
)

// FrameExclusion is a frame's EXCLUDE clause: after the bounds pick a
// row's frame, exclusion removes rows near the current row from it.
// The zero value is EXCLUDE NO OTHERS (the default — nothing removed),
// so an absent clause needs no special casing. GROUP removes the
// current row and its ORDER BY peers, TIES removes only the peers
// (the current row stays, if the bounds included it at all), CURRENT
// ROW removes just the current row.
type FrameExclusion int

const (
	ExcludeNoOthers FrameExclusion = iota
	ExcludeCurrentRow
	ExcludeGroup
	ExcludeTies
)

func (x FrameExclusion) name() string {
	switch x {
	case ExcludeCurrentRow:
		return "EXCLUDE CURRENT ROW"
	case ExcludeGroup:
		return "EXCLUDE GROUP"
	case ExcludeTies:
		return "EXCLUDE TIES"
	}
	return "EXCLUDE NO OTHERS"
}

// FrameBound is one frame endpoint; Offset is set only for the
// offset-taking bound types, and must be row-independent (the parser
// rejects column references inside it, so the executor can evaluate
// it once per window rather than per row, as Postgres does).
type FrameBound struct {
	Type   FrameBoundType
	Offset Expr
}

// WindowFrame is an explicit frame clause:
// {ROWS|RANGE|GROUPS} BETWEEN <start> AND <end> [EXCLUDE ...]. The
// single-bound form (no BETWEEN) parses with End = CURRENT ROW, its
// SQL meaning.
type WindowFrame struct {
	Mode    FrameMode
	Start   FrameBound
	End     FrameBound
	Exclude FrameExclusion
}

// ExWindow is a window function call: fn(args) OVER (PARTITION BY ...
// ORDER BY ... [frame]). Exactly one of Win (ranking/value family) or
// Agg (an aggregate evaluated over the frame) is set. With no explicit
// Frame the SQL default applies: RANGE UNBOUNDED PRECEDING .. CURRENT
// ROW — the whole partition when there is no ORDER BY, else running
// with peer sharing. Ranking functions and LAG/LEAD ignore the frame
// and address the whole partition, as in Postgres.
type ExWindow struct {
	Win       WinFunc
	Agg       AggFunc // AggNone unless an aggregate window
	Arg       Expr    // aggregate / value-function argument; nil for COUNT(*) and ranking fns
	Offset    Expr    // LAG/LEAD offset (default 1) or NTH_VALUE's n; nil otherwise
	Default   Expr    // LAG/LEAD out-of-partition fallback; nil means NULL
	Star      bool    // COUNT(*) OVER (...)
	Distinct  bool    // aggregate windows only: dedup within each row's frame
	Partition []Expr
	OrderBy   []OrderItem
	Frame     *WindowFrame // nil: the default frame
}

// fnName is the window function's name, ranking or aggregate.
func (w *ExWindow) fnName() string {
	if w.Win != WinNone {
		return w.Win.name()
	}
	return w.Agg.name()
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
// select lists only), a literal (IsLit — parsed literals and folded
// zero-argument functions like version()), or, when none of those
// shapes fit, a general expression (Ex non-nil), evaluated per row.
type SelectItem struct {
	Col     ColRef
	Agg     AggFunc
	Star    bool
	IsLit   bool
	Lit     any
	LitName string // output column name; "?column?" for plain literals
	Ex      Expr   // general expression; nil for the legacy shapes
	As      string // output alias from AS (or a bare alias); "" for none
}

// ColDef is one column of a CREATE TABLE or ALTER TABLE ADD COLUMN.
// Identity marks an auto-increment column (SERIAL or GENERATED BY
// DEFAULT AS IDENTITY); it implies NotNull.
type ColDef struct {
	Name     string
	Type     bytdb.ColType
	NotNull  bool
	Identity bool
	// MaxLen is a VARCHAR(n) length limit in characters (0: none).
	MaxLen int
	// Unique marks a UNIQUE column constraint — sugar the executor
	// lowers to a single-column unique index named table_col_key.
	Unique bool
	// Ref is a column-level REFERENCES constraint (Cols filled with
	// this column by the CREATE TABLE parser); nil when absent.
	Ref *FKDef
	// Default is the column's DEFAULT constant; HasDefault
	// distinguishes an absent clause (DEFAULT NULL parses as absent —
	// it declares what a defaultless column already does).
	Default    any
	HasDefault bool
}

// CheckDef is one CHECK constraint of a CREATE TABLE: the parsed
// expression plus its source text (which is what the descriptor
// stores). Col names the column a column-level check was declared on
// (naming only — the expression may reference any column, as in
// Postgres); "" for a table-level check.
type CheckDef struct {
	Name string // from CONSTRAINT name; "": named by convention
	Col  string
	Ex   Expr
	Text string
}

// FKDef is one FOREIGN KEY constraint as parsed: the child columns
// and the referenced table/columns (RefCols nil: the parent's primary
// key). Only NO ACTION/RESTRICT actions parse — there are no cascades.
type FKDef struct {
	Name     string // from CONSTRAINT name; "": named table_col_fkey
	Cols     []string
	RefTable string
	RefCols  []string
}

// CreateTable is CREATE TABLE t (col type [constraints], ...,
// [PRIMARY KEY (col, ...)], [[CONSTRAINT name] CHECK (expr)], ...).
// Uniques collects UNIQUE constraints — column-level and table-level
// alike — as column lists; the executor lowers each to a unique index
// (UNIQUE here is sugar over CREATE UNIQUE INDEX, as the pg_constraint
// synthesis assumes). FKs collects FOREIGN KEY constraints, column-
// level REFERENCES included.
type CreateTable struct {
	Table   string
	Cols    []ColDef
	PK      []string
	Checks  []CheckDef
	Uniques [][]string
	FKs     []FKDef
}

// DropTable is DROP TABLE t.
type DropTable struct{ Table string }

// Truncate is TRUNCATE [TABLE] t [, ...] [RESTART|CONTINUE IDENTITY]
// [RESTRICT]: delete every row (and index entry) of each named table
// in one statement, schema untouched. It is DML, not DDL — it runs
// inside transaction blocks, and all tables truncate atomically.
// CASCADE is rejected (bytdb has no foreign-key cascades to follow).
type Truncate struct {
	Tables          []string
	RestartIdentity bool // RESTART IDENTITY: identity counters draw from 1 again
}

// ShowVar is SHOW name (or SHOW ALL): report a configuration
// parameter as a one-row result. A Session answers from its SET state
// over the built-in defaults; a bare DB reports the defaults.
type ShowVar struct{ Name string } // "all" for SHOW ALL

// CreateView is CREATE [OR REPLACE] VIEW name AS select. The view
// stores the SELECT's source text; executing a statement that names
// the view materializes that query at that moment (bytdb views are
// unmaterialized, like Postgres's).
type CreateView struct {
	Name      string
	Query     string // the SELECT's verbatim source text
	Sel       *Select
	OrReplace bool
}

// DropView is DROP VIEW [IF EXISTS] name.
type DropView struct {
	Name     string
	IfExists bool
}

// AddColumn is ALTER TABLE t ADD [COLUMN] col type.
type AddColumn struct {
	Table string
	Col   ColDef
}

// DropColumn is ALTER TABLE t DROP [COLUMN] col.
type DropColumn struct{ Table, Col string }

// AddConstraint is ALTER TABLE t ADD [CONSTRAINT name] CHECK (expr).
type AddConstraint struct {
	Table string
	Check CheckDef
}

// AddFK is ALTER TABLE t ADD [CONSTRAINT name] FOREIGN KEY (cols)
// REFERENCES parent [(cols)]. Existing rows are validated in the
// transaction that publishes the constraint.
type AddFK struct {
	Table string
	FK    FKDef
}

// DropConstraint is ALTER TABLE t DROP CONSTRAINT [IF EXISTS] name.
type DropConstraint struct {
	Table    string
	Name     string
	IfExists bool
}

// CreateIndex is CREATE [UNIQUE] INDEX name ON t (col [ASC|DESC], ...).
// Desc is parallel to Cols (nil: all ascending).
type CreateIndex struct {
	Name   string
	Table  string
	Unique bool
	Cols   []string
	Desc   []bool
}

// DropIndex is DROP INDEX name [ON t]. With no ON clause the index is
// resolved by name across tables.
type DropIndex struct{ Name, Table string }

// SeqOptions is the option list of CREATE or ALTER SEQUENCE. Pointer
// fields distinguish "not mentioned" (nil — keep the default, or on
// ALTER the current value) from an explicit setting; NoMin/NoMax are
// the explicit NO MINVALUE / NO MAXVALUE spellings, which reset the
// bound to its default.
type SeqOptions struct {
	AsType    string // "": bigint; else the declared int type's name
	Increment *int64
	Min, Max  *int64
	NoMin     bool
	NoMax     bool
	Start     *int64
	Cache     *int64
	Cycle     *bool
	// ALTER SEQUENCE only: RESTART [WITH n]. Restart without a value
	// reuses the sequence's START.
	Restart     bool
	RestartWith *int64
}

// CreateSequence is CREATE SEQUENCE [IF NOT EXISTS] name [options].
type CreateSequence struct {
	Name        string
	IfNotExists bool
	Opts        SeqOptions
}

// DropSequence is DROP SEQUENCE [IF EXISTS] name [RESTRICT].
type DropSequence struct {
	Name     string
	IfExists bool
}

// AlterSequence is ALTER SEQUENCE name options.
type AlterSequence struct {
	Name string
	Opts SeqOptions
}

// defaultMarker is the DEFAULT keyword as an INSERT value: resolved
// at execution to the column's default, or NULL without one.
type defaultMarker struct{}

// Insert is INSERT INTO t [(cols)] VALUES (expr, ...), ...
// [RETURNING ...]. Plain literals stay parsed values — int64, float64,
// string, bool, nil, a Param, or a defaultMarker — the fast path: they
// coerce once at the engine and need no evaluation. Anything richer
// (nextval('s'), 1+2, 'a'||'b', a scalar subquery) is stored as an
// Expr and evaluated per row at execution, against an empty scope:
// VALUES has no input row, so column references fail there, as in
// Postgres. INSERT ... DEFAULT VALUES parses as an empty column list
// with one empty row.
type Insert struct {
	Table    string
	Cols     []string // nil: values in declared column order
	Rows     [][]any
	Conflict *OnConflict // nil: a collision is an error, as ever
	Returning
}

// OnConflict is INSERT's ON CONFLICT clause: what to do instead of
// erroring when a proposed row collides with an existing one on the
// primary key or a unique index. TargetCols names the conflict target
// — the column set of the PK or of one unique index (nil: any of
// them, allowed only with DO NOTHING, as in Postgres). Update false
// is DO NOTHING: the proposed row is silently dropped. Update true is
// DO UPDATE: the existing row gets Set/SetEx applied (the same split
// as Update's), where unqualified column references read the existing
// row and excluded.col reads the proposed one; Where, when present,
// limits which conflicting rows update.
type OnConflict struct {
	TargetCols []string
	Update     bool
	Set        map[string]any
	SetEx      map[string]Expr
	Where      BoolExpr
}

// Returning is the optional RETURNING clause of INSERT, UPDATE, and
// DELETE: select items evaluated against each affected row (as stored
// for INSERT/UPDATE — identity values filled, SET applied — and as it
// was for DELETE). Star is RETURNING *; the two are exclusive, as in
// a select list. A statement with neither returns no rows.
type Returning struct {
	RetItems []SelectItem
	RetStar  bool
}

// hasReturning reports whether the statement carries a RETURNING
// clause at all — the executor's switch between "count only" and
// "count plus rows".
func (r *Returning) hasReturning() bool { return r.RetStar || len(r.RetItems) > 0 }

// retSelect wraps the clause as a synthetic single-table SELECT so the
// projection machinery (projectSelect, describeSelect's typing) can be
// reused verbatim: RETURNING is exactly a select list whose FROM is
// the statement's target table.
func (r *Returning) retSelect() *Select { return &Select{Star: r.RetStar, Items: r.RetItems} }

// GroupItem is one GROUP BY key: a column reference, an integer
// ordinal naming a select-list position (Pos > 0, as in GROUP BY 1),
// or a general expression (Ex non-nil).
type GroupItem struct {
	Col ColRef
	Pos int64 // 1-based select-list position; 0: Col or Ex is the key
	Ex  Expr
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
	Table    string
	Alias    string // "" : referenced by the table name
	Join     JoinType
	On       BoolExpr
	FuncArgs []Expr // non-nil: a set-returning function call in FROM —
	// parses (psql writes them inside never-evaluated subqueries) but
	// errors if the scan is ever built
}

// CTE is one WITH entry: a named subquery materialized once, before
// the statement body runs, and visible as a table everywhere in the
// statement. Derived tables (FROM (SELECT ...) alias) lower to
// synthetic CTEs at parse — their names contain '*', which no real
// identifier can, so they can never collide. Cols, when non-empty,
// renames the output columns (WITH x(a, b) AS ...).
type CTE struct {
	Name string
	Cols []string
	Sel  *Select
}

// Select is SELECT over one table or a chain of joins. A query with
// any aggregate item, a GROUP BY, or a HAVING is an aggregate query.
// A non-empty Union makes this the first arm of a UNION chain;
// OrderBy, Limit, and Offset then apply to the combined result, and
// the arms' own OrderBy/Limit/Offset are unset. With holds the
// statement's CTEs — written and synthetic — in dependency order
// (each may reference the ones before it; recursion is rejected).
type Select struct {
	With []CTE
	From []FromItem
	// Distinct is SELECT DISTINCT: the projected rows dedup before
	// ORDER BY / OFFSET / LIMIT apply, which is why ORDER BY is then
	// restricted to output columns and positions — sorting by a value
	// the projection dropped would make "which duplicate survived"
	// observable, so Postgres forbids it and bytdb follows.
	Distinct bool
	Star     bool
	Items    []SelectItem
	Where    BoolExpr // nil: all rows
	GroupBy  []GroupItem
	Having   BoolExpr // nil: all groups; leaves may be aggregates
	OrderBy  []OrderItem
	Limit    int64 // -1: no limit
	Offset   int64
	Union    []UnionArm
}

// UnionArm is one UNION [ALL] continuation, combined left to right:
// a distinct arm dedups everything accumulated so far, per SQL.
type UnionArm struct {
	All bool
	Sel *Select
}

// Update is UPDATE t SET col = expr, ... [WHERE ...] [RETURNING ...].
// Plain literal (and $n placeholder) assignments live in Set — the
// fast path: they coerce once per statement and need no per-row
// evaluation. Anything else (SET age = age + 1, a CASE, a scalar
// subquery) lands in SetEx and is evaluated against each matching row
// at execution. A column appears in at most one of the two maps.
type Update struct {
	Table string
	Set   map[string]any
	SetEx map[string]Expr
	Where BoolExpr
	Returning
}

// Delete is DELETE FROM t [WHERE ...] [RETURNING ...].
type Delete struct {
	Table string
	Where BoolExpr
	Returning
}

// TxnKind distinguishes the transaction-control statements.
type TxnKind int

const (
	TxnBegin TxnKind = iota
	TxnCommit
	TxnRollback
	TxnSavepoint  // SAVEPOINT name
	TxnRelease    // RELEASE [SAVEPOINT] name
	TxnRollbackTo // ROLLBACK [WORK|TRANSACTION] TO [SAVEPOINT] name
)

// TxnControl is BEGIN / START TRANSACTION, COMMIT / END, ROLLBACK /
// ABORT, or a savepoint statement, executed by a Session (a bare DB
// rejects it). Isolation levels parse and are ignored — the engine's
// single-writer transactions are serializable, which satisfies every
// level — and READ ONLY is honored. Tag is the Postgres command tag
// of the form used (END reports COMMIT, ABORT reports ROLLBACK).
type TxnControl struct {
	Kind     TxnKind
	ReadOnly bool
	Tag      string
	Name     string // savepoint name for the savepoint kinds
}

// Explain is EXPLAIN <statement>: describe the statement's access
// plan without executing it. ANALYZE is rejected (bytdb does not
// instrument execution); VERBOSE, COSTS, and FORMAT TEXT parse and
// are ignored.
type Explain struct{ Stmt Statement }

// SetVar is SET [SESSION|LOCAL] name {=|TO} value, or RESET name.
// bytdb gives semantics to statement_timeout (a Session honors it on
// every following statement); any other parameter is accepted and
// remembered but changes nothing, so the housekeeping SETs drivers
// and ORMs send on connect succeed. Value holds the rendered value
// text, comma-joined for list parameters like search_path; IsDefault
// marks SET ... TO DEFAULT and RESET, which restore the parameter's
// default (for statement_timeout: no timeout).
type SetVar struct {
	Name      string
	Value     string
	IsDefault bool
	Tag       string // command tag: SET or RESET
}

func (*CreateTable) stmt()    {}
func (*Explain) stmt()        {}
func (*DropTable) stmt()      {}
func (*Truncate) stmt()       {}
func (*ShowVar) stmt()        {}
func (*CreateView) stmt()     {}
func (*DropView) stmt()       {}
func (*AddColumn) stmt()      {}
func (*DropColumn) stmt()     {}
func (*AddConstraint) stmt()  {}
func (*AddFK) stmt()          {}
func (*DropConstraint) stmt() {}
func (*CreateIndex) stmt()    {}
func (*DropIndex) stmt()      {}
func (*CreateSequence) stmt() {}
func (*DropSequence) stmt()   {}
func (*AlterSequence) stmt()  {}
func (*Insert) stmt()         {}
func (*Select) stmt()         {}
func (*Update) stmt()         {}
func (*Delete) stmt()         {}
func (*TxnControl) stmt()     {}
func (*SetVar) stmt()         {}
