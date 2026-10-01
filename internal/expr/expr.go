// Package expr defines bound (type-checked) expressions, the built-in
// functions, operators and aggregates, and a compiler that turns an
// expression into a Go closure for evaluation.
//
// Expressions refer to columns by ColumnID, a number unique within a query,
// not by position. Physical operators decide row layouts late (after join
// reordering), and a column that is not in an operator's input layout is
// looked up among the outer (correlated) bindings instead.
package expr

import (
	"fmt"
	"strings"

	"github.com/useless-husband/basalt/internal/types"
)

// ColumnID identifies a column within a query.
type ColumnID int32

// Expr is a bound expression.
type Expr interface {
	Type() types.T
	String() string
	children() []Expr
}

// Col references a column.
type Col struct {
	ID   ColumnID
	T    types.T
	Name string // for EXPLAIN, e.g. "t.a"
}

// Const is a constant.
type Const struct {
	V types.Value
	T types.T
}

// Param is a statement parameter $N.
type Param struct {
	N int
	T types.T
}

// Call applies a built-in function or operator.
type Call struct {
	Fn   *Func
	Args []Expr
	T    types.T
}

// And is an n-ary conjunction with SQL three-valued logic.
type And struct{ Args []Expr }

// Or is an n-ary disjunction.
type Or struct{ Args []Expr }

// Not is logical negation.
type Not struct{ Arg Expr }

// When is a CASE arm.
type When struct{ Cond, Then Expr }

// Case is a searched CASE expression.
type Case struct {
	Whens []When
	Else  Expr
	T     types.T
}

// Cast converts a value to another type.
type Cast struct {
	Arg      Expr
	T        types.T
	Explicit bool
}

// IsNull is IS [NOT] NULL.
type IsNull struct {
	Arg Expr
	Not bool
}

// BoolTest kinds.
const (
	TestTrue = iota
	TestFalse
	TestUnknown
)

// IsBool is IS [NOT] TRUE/FALSE/UNKNOWN.
type IsBool struct {
	Arg  Expr
	What int
	Not  bool
}

// Distinct is IS [NOT] DISTINCT FROM.
type Distinct struct {
	L, R Expr
	Not  bool
}

// InList is x [NOT] IN (list).
type InList struct {
	Arg  Expr
	List []Expr
	Not  bool
}

// Coalesce returns its first non-NULL argument.
type Coalesce struct {
	Args []Expr
	T    types.T
}

// NullIf is NULLIF(a, b).
type NullIf struct {
	A, B Expr
	T    types.T
}

// MinMax is GREATEST or LEAST.
type MinMax struct {
	Args     []Expr
	Greatest bool
	T        types.T
}

// SubKind enumerates subquery forms.
type SubKind int

const (
	SubScalar SubKind = iota
	SubExists
	SubAny // x op ANY (subquery), including IN
	SubAll
	SubArray
)

// Subquery is a subquery used as an expression. Plan is the planner's plan
// for it (opaque here). Outer lists the columns of enclosing queries the
// subquery reads; if empty the subquery is evaluated once per statement.
type Subquery struct {
	Kind  SubKind
	Plan  any
	Arg   Expr  // left operand of ANY/ALL
	Cmp   *Func // comparison for ANY/ALL
	Not   bool  // NOT IN / NOT EXISTS
	Outer []ColumnID
	T     types.T
	ID    int
	// Label is the EXPLAIN name, e.g. "SubPlan 1".
	Label string
	// Remap redirects an outer column to the column that carries its value
	// in the evaluating operator's row (after grouping, a grouped column
	// is carried by the Aggregate's group column).
	Remap map[ColumnID]ColumnID
}

// ArrayCons is ARRAY[...].
type ArrayCons struct {
	Elems []Expr
	T     types.T
}

// Subscript is arr[i].
type Subscript struct {
	Arr, Idx Expr
	T        types.T
}

// RowTuple is a row constructor (a, b) used in comparisons.
type RowTuple struct {
	Elems []Expr
}

func (e *Col) Type() types.T       { return e.T }
func (e *Const) Type() types.T     { return e.T }
func (e *Param) Type() types.T     { return e.T }
func (e *Call) Type() types.T      { return e.T }
func (e *And) Type() types.T       { return types.Bool }
func (e *Or) Type() types.T        { return types.Bool }
func (e *Not) Type() types.T       { return types.Bool }
func (e *Case) Type() types.T      { return e.T }
func (e *Cast) Type() types.T      { return e.T }
func (e *IsNull) Type() types.T    { return types.Bool }
func (e *IsBool) Type() types.T    { return types.Bool }
func (e *Distinct) Type() types.T  { return types.Bool }
func (e *InList) Type() types.T    { return types.Bool }
func (e *Coalesce) Type() types.T  { return e.T }
func (e *NullIf) Type() types.T    { return e.T }
func (e *MinMax) Type() types.T    { return e.T }
func (e *Subquery) Type() types.T  { return e.T }
func (e *ArrayCons) Type() types.T { return e.T }
func (e *Subscript) Type() types.T { return e.T }
func (e *RowTuple) Type() types.T  { return types.Record }

func (e *Col) children() []Expr   { return nil }
func (e *Const) children() []Expr { return nil }
func (e *Param) children() []Expr { return nil }
func (e *Call) children() []Expr  { return e.Args }
func (e *And) children() []Expr   { return e.Args }
func (e *Or) children() []Expr    { return e.Args }
func (e *Not) children() []Expr   { return []Expr{e.Arg} }
func (e *Case) children() []Expr {
	var out []Expr
	for _, w := range e.Whens {
		out = append(out, w.Cond, w.Then)
	}
	if e.Else != nil {
		out = append(out, e.Else)
	}
	return out
}
func (e *Cast) children() []Expr     { return []Expr{e.Arg} }
func (e *IsNull) children() []Expr   { return []Expr{e.Arg} }
func (e *IsBool) children() []Expr   { return []Expr{e.Arg} }
func (e *Distinct) children() []Expr { return []Expr{e.L, e.R} }
func (e *InList) children() []Expr   { return append([]Expr{e.Arg}, e.List...) }
func (e *Coalesce) children() []Expr { return e.Args }
func (e *NullIf) children() []Expr   { return []Expr{e.A, e.B} }
func (e *MinMax) children() []Expr   { return e.Args }
func (e *Subquery) children() []Expr {
	if e.Arg != nil {
		return []Expr{e.Arg}
	}
	return nil
}
func (e *ArrayCons) children() []Expr { return e.Elems }
func (e *Subscript) children() []Expr { return []Expr{e.Arr, e.Idx} }
func (e *RowTuple) children() []Expr  { return e.Elems }

// Walk visits e and its children depth-first; fn returns false to skip a
// node's children.
func Walk(e Expr, fn func(Expr) bool) {
	if e == nil || !fn(e) {
		return
	}
	for _, c := range e.children() {
		Walk(c, fn)
	}
}

// Columns returns the set of columns referenced by e, including the outer
// references of subqueries.
func Columns(e Expr) map[ColumnID]bool {
	out := map[ColumnID]bool{}
	Walk(e, func(x Expr) bool {
		switch c := x.(type) {
		case *Col:
			out[c.ID] = true
		case *Subquery:
			for _, id := range c.Outer {
				out[id] = true
			}
		}
		return true
	})
	return out
}

// HasSubquery reports whether e contains a subquery.
func HasSubquery(e Expr) bool {
	found := false
	Walk(e, func(x Expr) bool {
		if _, ok := x.(*Subquery); ok {
			found = true
		}
		return !found
	})
	return found
}

// IsVolatile reports whether e calls a volatile function.
func IsVolatile(e Expr) bool {
	v := false
	Walk(e, func(x Expr) bool {
		if c, ok := x.(*Call); ok && c.Fn.Volatile {
			v = true
		}
		if _, ok := x.(*Subquery); ok {
			v = true
		}
		return !v
	})
	return v
}

// Equal reports structural equality of two expressions.
func Equal(a, b Expr) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	switch x := a.(type) {
	case *Col:
		y, ok := b.(*Col)
		return ok && x.ID == y.ID
	case *Const:
		y, ok := b.(*Const)
		return ok && x.T == y.T && types.Equal(x.V, y.V) && x.V.K == y.V.K
	case *Param:
		y, ok := b.(*Param)
		return ok && x.N == y.N
	case *Call:
		y, ok := b.(*Call)
		return ok && x.Fn == y.Fn && exprsEqual(x.Args, y.Args)
	case *Cast:
		y, ok := b.(*Cast)
		return ok && x.T == y.T && Equal(x.Arg, y.Arg)
	case *Subquery:
		return a == b
	}
	if fmt.Sprintf("%T", a) != fmt.Sprintf("%T", b) {
		return false
	}
	ac, bc := a.children(), b.children()
	if !exprsEqual(ac, bc) {
		return false
	}
	return a.String() == b.String()
}

func exprsEqual(a, b []Expr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Map rebuilds e bottom-up, replacing each node by fn(node) (fn receives
// the node with already-mapped children).
func Map(e Expr, fn func(Expr) Expr) Expr {
	if e == nil {
		return nil
	}
	return fn(mapChildren(e, func(c Expr) Expr { return Map(c, fn) }))
}

// Replace rebuilds e top-down: if fn returns a non-nil expression for a
// node, that node (and its subtree) is replaced; otherwise its children
// are processed.
func Replace(e Expr, fn func(Expr) Expr) Expr {
	if e == nil {
		return nil
	}
	if r := fn(e); r != nil {
		return r
	}
	return mapChildren(e, func(c Expr) Expr { return Replace(c, fn) })
}

// mapChildren returns a copy of e with f applied to each direct child.
func mapChildren(e Expr, f func(Expr) Expr) Expr {
	mapAll := func(es []Expr) []Expr {
		out := make([]Expr, len(es))
		for i, x := range es {
			out[i] = f(x)
		}
		return out
	}
	opt := func(x Expr) Expr {
		if x == nil {
			return nil
		}
		return f(x)
	}
	switch x := e.(type) {
	case *Call:
		return &Call{Fn: x.Fn, Args: mapAll(x.Args), T: x.T}
	case *And:
		return &And{Args: mapAll(x.Args)}
	case *Or:
		return &Or{Args: mapAll(x.Args)}
	case *Not:
		return &Not{Arg: f(x.Arg)}
	case *Case:
		c := &Case{T: x.T, Else: opt(x.Else)}
		for _, w := range x.Whens {
			c.Whens = append(c.Whens, When{f(w.Cond), f(w.Then)})
		}
		return c
	case *Cast:
		return &Cast{Arg: f(x.Arg), T: x.T, Explicit: x.Explicit}
	case *IsNull:
		return &IsNull{Arg: f(x.Arg), Not: x.Not}
	case *IsBool:
		return &IsBool{Arg: f(x.Arg), What: x.What, Not: x.Not}
	case *Distinct:
		return &Distinct{L: f(x.L), R: f(x.R), Not: x.Not}
	case *InList:
		return &InList{Arg: f(x.Arg), List: mapAll(x.List), Not: x.Not}
	case *Coalesce:
		return &Coalesce{Args: mapAll(x.Args), T: x.T}
	case *NullIf:
		return &NullIf{A: f(x.A), B: f(x.B), T: x.T}
	case *MinMax:
		return &MinMax{Args: mapAll(x.Args), Greatest: x.Greatest, T: x.T}
	case *Subquery:
		c := *x
		c.Arg = opt(x.Arg)
		return &c
	case *ArrayCons:
		return &ArrayCons{Elems: mapAll(x.Elems), T: x.T}
	case *Subscript:
		return &Subscript{Arr: f(x.Arr), Idx: f(x.Idx), T: x.T}
	case *RowTuple:
		return &RowTuple{Elems: mapAll(x.Elems)}
	}
	return e
}

// Conjuncts splits an AND tree into its terms.
func Conjuncts(e Expr) []Expr {
	if e == nil {
		return nil
	}
	if a, ok := e.(*And); ok {
		var out []Expr
		for _, x := range a.Args {
			out = append(out, Conjuncts(x)...)
		}
		return out
	}
	return []Expr{e}
}

// MakeAnd combines terms into a conjunction (nil if empty).
func MakeAnd(terms []Expr) Expr {
	switch len(terms) {
	case 0:
		return nil
	case 1:
		return terms[0]
	}
	return &And{Args: terms}
}

// ---- formatting (PostgreSQL EXPLAIN style) ----

func (e *Col) String() string { return e.Name }

func (e *Const) String() string {
	if e.V.IsNull() {
		return "NULL"
	}
	s := types.ToText(e.V, e.T)
	switch e.T.Kind() {
	case types.KInt, types.KFloat, types.KNumeric:
		if strings.HasPrefix(s, "-") {
			return "'" + s + "'::" + e.T.String()
		}
		return s
	case types.KBool:
		if e.V.Bool() {
			return "true"
		}
		return "false"
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'::" + e.T.String()
}

func (e *Param) String() string { return fmt.Sprintf("$%d", e.N) }

func (e *Call) String() string {
	if e.Fn.Op != "" {
		if len(e.Args) == 1 {
			return "(" + e.Fn.Op + " " + e.Args[0].String() + ")"
		}
		return "(" + e.Args[0].String() + " " + e.Fn.Op + " " + e.Args[1].String() + ")"
	}
	parts := make([]string, len(e.Args))
	for i, a := range e.Args {
		parts[i] = a.String()
	}
	return e.Fn.Name + "(" + strings.Join(parts, ", ") + ")"
}

func joinExprs(es []Expr, sep string) string {
	parts := make([]string, len(es))
	for i, a := range es {
		parts[i] = a.String()
	}
	return strings.Join(parts, sep)
}

func (e *And) String() string { return "(" + joinExprs(e.Args, " AND ") + ")" }
func (e *Or) String() string  { return "(" + joinExprs(e.Args, " OR ") + ")" }
func (e *Not) String() string { return "(NOT " + e.Arg.String() + ")" }

func (e *Case) String() string {
	var b strings.Builder
	b.WriteString("CASE")
	for _, w := range e.Whens {
		b.WriteString(" WHEN " + w.Cond.String() + " THEN " + w.Then.String())
	}
	if e.Else != nil {
		b.WriteString(" ELSE " + e.Else.String())
	}
	b.WriteString(" END")
	return b.String()
}

func (e *Cast) String() string { return "(" + e.Arg.String() + ")::" + e.T.String() }

func (e *IsNull) String() string {
	if e.Not {
		return "(" + e.Arg.String() + " IS NOT NULL)"
	}
	return "(" + e.Arg.String() + " IS NULL)"
}

func (e *IsBool) String() string {
	w := [...]string{"TRUE", "FALSE", "UNKNOWN"}[e.What]
	if e.Not {
		w = "NOT " + w
	}
	return "(" + e.Arg.String() + " IS " + w + ")"
}

func (e *Distinct) String() string {
	op := " IS DISTINCT FROM "
	if e.Not {
		op = " IS NOT DISTINCT FROM "
	}
	return "(" + e.L.String() + op + e.R.String() + ")"
}

func (e *InList) String() string {
	op := " = ANY "
	if e.Not {
		op = " <> ALL "
	}
	return "(" + e.Arg.String() + op + "(ARRAY[" + joinExprs(e.List, ", ") + "]))"
}

func (e *Coalesce) String() string { return "COALESCE(" + joinExprs(e.Args, ", ") + ")" }
func (e *NullIf) String() string   { return "NULLIF(" + e.A.String() + ", " + e.B.String() + ")" }
func (e *MinMax) String() string {
	if e.Greatest {
		return "GREATEST(" + joinExprs(e.Args, ", ") + ")"
	}
	return "LEAST(" + joinExprs(e.Args, ", ") + ")"
}

func (e *Subquery) String() string {
	label := e.Label
	switch e.Kind {
	case SubExists:
		if e.Not {
			return "(NOT EXISTS(" + label + "))"
		}
		return "EXISTS(" + label + ")"
	case SubAny:
		s := "(" + e.Arg.String() + " " + e.Cmp.Op + " ANY (" + label + "))"
		if e.Not {
			return "(NOT " + s + ")"
		}
		return s
	case SubAll:
		return "(" + e.Arg.String() + " " + e.Cmp.Op + " ALL (" + label + "))"
	case SubArray:
		return "ARRAY(" + label + ")"
	}
	return "(" + label + ")"
}

func (e *ArrayCons) String() string { return "ARRAY[" + joinExprs(e.Elems, ", ") + "]" }
func (e *Subscript) String() string { return e.Arr.String() + "[" + e.Idx.String() + "]" }
func (e *RowTuple) String() string  { return "ROW(" + joinExprs(e.Elems, ", ") + ")" }

// NullConst returns a typed NULL constant.
func NullConst(t types.T) *Const { return &Const{V: types.Null, T: t} }

// IsConst reports whether e is a constant (and returns it).
func IsConst(e Expr) (*Const, bool) {
	c, ok := e.(*Const)
	return c, ok
}
