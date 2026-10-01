// Package planner turns parsed statements into executable plans: the
// binder resolves names and types and builds a logical plan, rewrite rules
// fold constants and push predicates down, and the physical planner picks
// access paths, join order and join algorithms with a cost model fed by
// ANALYZE statistics.
package planner

import (
	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/types"
)

// ColInfo describes a column of the query.
type ColInfo struct {
	Name     string
	T        types.T
	Table    string // alias of the source table, if any
	TableOid uint32
	Attnum   int16
}

// Node is a logical plan node.
type Node interface {
	Cols() []expr.ColumnID
}

// Scan reads a base table.
type Scan struct {
	Table   *catalog.Table
	Alias   string
	ColIDs  []expr.ColumnID // one per table column
	TIDCol  expr.ColumnID   // row identifier column, 0 if unused
	Filters []expr.Expr
	Lock    bool // FOR UPDATE / target of UPDATE or DELETE
}

// VirtualTable is a system table generated on demand (pg_catalog).
type VirtualTable struct {
	Name    string
	Columns []VirtualColumn
	Rows    func() ([][]types.Value, error)
}

// VirtualColumn is a column of a virtual table.
type VirtualColumn struct {
	Name string
	T    types.T
}

// VScan reads a virtual table.
type VScan struct {
	VT      *VirtualTable
	Alias   string
	ColIDs  []expr.ColumnID
	Filters []expr.Expr
}

// Values is a VALUES list or a FROM-less SELECT (one empty row).
type Values struct {
	Rows   [][]expr.Expr
	ColIDs []expr.ColumnID
}

// FuncScan is a set-returning function in FROM.
type FuncScan struct {
	Name   string
	Args   []expr.Expr
	ColIDs []expr.ColumnID
	T      types.T
}

// Filter keeps rows for which Cond is true.
type Filter struct {
	Input Node
	Cond  expr.Expr
}

// Project computes output columns.
type Project struct {
	Input  Node
	Exprs  []expr.Expr
	ColIDs []expr.ColumnID
}

// JoinKind enumerates logical join kinds.
type JoinKind int

const (
	JoinInner JoinKind = iota
	JoinLeft
	JoinFull
	JoinSemi
	JoinAnti
)

func (k JoinKind) String() string {
	return [...]string{"Inner", "Left", "Full", "Semi", "Anti"}[k]
}

// Join combines two inputs.
type Join struct {
	Kind        JoinKind
	Left, Right Node
	Cond        expr.Expr // nil for a cross join
}

// Aggregate groups rows and computes aggregates.
type Aggregate struct {
	Input     Node
	GroupBy   []expr.Expr
	GroupCols []expr.ColumnID
	Aggs      []*expr.AggCall
}

// Sort orders rows.
type Sort struct {
	Input Node
	Keys  []expr.SortKey
}

// Limit implements LIMIT/OFFSET.
type Limit struct {
	Input         Node
	Limit, Offset expr.Expr
}

// Distinct removes duplicates. With On set it keeps the first row of each
// group of equal On values (DISTINCT ON), which requires sorted input.
type Distinct struct {
	Input Node
	On    []expr.Expr
}

// SetOp is UNION/INTERSECT/EXCEPT.
type SetOp struct {
	Kind        int // sql.SetUnion etc.
	All         bool
	Left, Right Node
	ColIDs      []expr.ColumnID
}

func (n *Scan) Cols() []expr.ColumnID {
	if n.TIDCol != 0 {
		return append(append([]expr.ColumnID(nil), n.ColIDs...), n.TIDCol)
	}
	return n.ColIDs
}
func (n *VScan) Cols() []expr.ColumnID    { return n.ColIDs }
func (n *Values) Cols() []expr.ColumnID   { return n.ColIDs }
func (n *FuncScan) Cols() []expr.ColumnID { return n.ColIDs }
func (n *Filter) Cols() []expr.ColumnID   { return n.Input.Cols() }
func (n *Project) Cols() []expr.ColumnID  { return n.ColIDs }
func (n *Join) Cols() []expr.ColumnID {
	if n.Kind == JoinSemi || n.Kind == JoinAnti {
		return n.Left.Cols()
	}
	return append(append([]expr.ColumnID(nil), n.Left.Cols()...), n.Right.Cols()...)
}
func (n *Aggregate) Cols() []expr.ColumnID {
	out := append([]expr.ColumnID(nil), n.GroupCols...)
	for _, a := range n.Aggs {
		out = append(out, a.ID)
	}
	return out
}
func (n *Sort) Cols() []expr.ColumnID     { return n.Input.Cols() }
func (n *Limit) Cols() []expr.ColumnID    { return n.Input.Cols() }
func (n *Distinct) Cols() []expr.ColumnID { return n.Input.Cols() }
func (n *SetOp) Cols() []expr.ColumnID    { return n.ColIDs }

// ---- statements ----

// Query is a bound SELECT.
type Query struct {
	Root Node
	// Output describes the result columns (Root.Cols() in order).
	Output []OutputCol
}

// OutputCol is a result column.
type OutputCol struct {
	Name string
	ID   expr.ColumnID
	T    types.T
	// Source table and attribute, for RowDescription.
	TableOid uint32
	Attnum   int16
}

// Insert is a bound INSERT.
type Insert struct {
	Table  *catalog.Table
	Source Node
	// Exprs computes each table column from a source row (layout of
	// Source.Cols()).
	Exprs               []expr.Expr
	OnConflictDoNothing bool
	ConflictCols        []int
	// ON CONFLICT DO UPDATE
	ConflictSet   map[int]expr.Expr // column -> expression over ExistingCols+ExcludedCols
	ConflictWhere expr.Expr
	ExistingCols  []expr.ColumnID
	ExcludedCols  []expr.ColumnID
	Returning     []expr.Expr
	ReturningCols []expr.ColumnID // the inserted row's columns
	Output        []OutputCol
}

// Update is a bound UPDATE. Source yields the target row's TID and old
// column values (Scan.ColIDs) plus any FROM columns.
type Update struct {
	Table     *catalog.Table
	Source    Node
	Scan      *Scan
	Set       map[int]expr.Expr // column position -> new value
	Returning []expr.Expr
	NewCols   []expr.ColumnID // new row values, for RETURNING
	Output    []OutputCol
}

// Delete is a bound DELETE.
type Delete struct {
	Table     *catalog.Table
	Source    Node
	Scan      *Scan
	Returning []expr.Expr
	Output    []OutputCol
}
