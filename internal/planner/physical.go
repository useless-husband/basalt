package planner

import (
	"fmt"
	"strings"
	"time"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/types"
)

// Est is a cost estimate in PostgreSQL's units (sequential page reads).
type Est struct {
	Rows    float64
	Startup float64
	Total   float64
	Width   int
}

// NodeStats are filled in by EXPLAIN ANALYZE.
type NodeStats struct {
	Rows  int64
	Loops int64
	Time  time.Duration
	// First is the time to the first row, summed over loops.
	First time.Duration
	// Extra lines such as "Rows Removed by Filter".
	Removed int64
}

// Base is embedded in every physical node.
type Base struct {
	E     Est
	Stats NodeStats
}

// Plan is a physical plan node.
type Plan interface {
	Layout() []expr.ColumnID
	base() *Base
	children() []Plan
}

func (b *Base) base() *Base { return b }

// Estimate returns the node's estimate.
func Estimate(p Plan) *Est { return &p.base().E }

// Stats returns the node's EXPLAIN ANALYZE counters.
func Stats(p Plan) *NodeStats { return &p.base().Stats }

// SeqScanP reads a table sequentially.
type SeqScanP struct {
	Base
	Table  *catalog.Table
	Alias  string
	ColIDs []expr.ColumnID
	TIDCol expr.ColumnID
	Filter expr.Expr
	Lock   bool
}

// IndexScanP reads rows through a B+tree index. Eq holds values for the
// first len(Eq) index columns; Lo/Hi bound the next column.
type IndexScanP struct {
	Base
	Table          *catalog.Table
	Alias          string
	Index          *catalog.Index
	ColIDs         []expr.ColumnID
	TIDCol         expr.ColumnID
	Eq             []expr.Expr
	Lo, Hi         expr.Expr
	LoIncl, HiIncl bool
	Filter         expr.Expr
	Lock           bool
	// IndexCond is the predicate the index bounds implement (EXPLAIN).
	IndexCond expr.Expr
}

// VScanP reads a virtual system table.
type VScanP struct {
	Base
	VT     *VirtualTable
	Alias  string
	ColIDs []expr.ColumnID
	Filter expr.Expr
}

// ValuesP produces constant rows.
type ValuesP struct {
	Base
	Rows   [][]expr.Expr
	ColIDs []expr.ColumnID
}

// FuncScanP runs a set-returning function.
type FuncScanP struct {
	Base
	Name   string
	Args   []expr.Expr
	ColIDs []expr.ColumnID
	T      types.T
}

// FilterP filters rows.
type FilterP struct {
	Base
	Input Plan
	Cond  expr.Expr
}

// ProjectP computes expressions.
type ProjectP struct {
	Base
	Input  Plan
	Exprs  []expr.Expr
	ColIDs []expr.ColumnID
}

// NestLoopP is a nested loop join. The inner plan is re-executed for every
// outer row with the outer row's columns available as outer bindings, so
// it can be a parameterized index scan.
type NestLoopP struct {
	Base
	Kind         JoinKind
	Outer, Inner Plan
	Cond         expr.Expr
}

// HashJoinP builds a hash table on Inner and probes it with Outer.
type HashJoinP struct {
	Base
	Kind                 JoinKind
	Outer, Inner         Plan
	OuterKeys, InnerKeys []expr.Expr
	Residual             expr.Expr
}

// MergeJoinP joins two inputs sorted on the keys.
type MergeJoinP struct {
	Base
	Kind                 JoinKind
	Outer, Inner         Plan
	OuterKeys, InnerKeys []expr.Expr
	Residual             expr.Expr
}

// AggP groups rows in a hash table (or a single group without GROUP BY).
type AggP struct {
	Base
	Input     Plan
	GroupBy   []expr.Expr
	GroupCols []expr.ColumnID
	Aggs      []*expr.AggCall
}

// SortP sorts rows; TopN > 0 keeps only that many (LIMIT pushed down).
type SortP struct {
	Base
	Input Plan
	Keys  []expr.SortKey
	TopN  expr.Expr
}

// LimitP implements LIMIT/OFFSET.
type LimitP struct {
	Base
	Input         Plan
	Limit, Offset expr.Expr
}

// DistinctP removes duplicate rows (hash), or with On keeps the first row
// of each run of equal On values (input sorted).
type DistinctP struct {
	Base
	Input Plan
	On    []expr.Expr
}

// SetOpP implements UNION/INTERSECT/EXCEPT.
type SetOpP struct {
	Base
	Kind        int
	All         bool
	Left, Right Plan
	ColIDs      []expr.ColumnID
}

// MaterializeP buffers its input so it can be rescanned cheaply.
type MaterializeP struct {
	Base
	Input Plan
}

// InsertP, UpdateP and DeleteP are DML roots.
type InsertP struct {
	Base
	Ins    *Insert
	Source Plan
}

// UpdateP updates rows produced by Source.
type UpdateP struct {
	Base
	Upd    *Update
	Source Plan
}

// DeleteP deletes rows produced by Source.
type DeleteP struct {
	Base
	Del    *Delete
	Source Plan
}

func (p *SeqScanP) Layout() []expr.ColumnID   { return scanLayout(p.ColIDs, p.TIDCol) }
func (p *IndexScanP) Layout() []expr.ColumnID { return scanLayout(p.ColIDs, p.TIDCol) }
func (p *VScanP) Layout() []expr.ColumnID     { return p.ColIDs }
func (p *ValuesP) Layout() []expr.ColumnID    { return p.ColIDs }
func (p *FuncScanP) Layout() []expr.ColumnID  { return p.ColIDs }
func (p *FilterP) Layout() []expr.ColumnID    { return p.Input.Layout() }
func (p *ProjectP) Layout() []expr.ColumnID   { return p.ColIDs }
func (p *NestLoopP) Layout() []expr.ColumnID  { return joinLayout(p.Kind, p.Outer, p.Inner) }
func (p *HashJoinP) Layout() []expr.ColumnID  { return joinLayout(p.Kind, p.Outer, p.Inner) }
func (p *MergeJoinP) Layout() []expr.ColumnID { return joinLayout(p.Kind, p.Outer, p.Inner) }
func (p *AggP) Layout() []expr.ColumnID {
	out := append([]expr.ColumnID(nil), p.GroupCols...)
	for _, a := range p.Aggs {
		out = append(out, a.ID)
	}
	return out
}
func (p *SortP) Layout() []expr.ColumnID        { return p.Input.Layout() }
func (p *LimitP) Layout() []expr.ColumnID       { return p.Input.Layout() }
func (p *DistinctP) Layout() []expr.ColumnID    { return p.Input.Layout() }
func (p *SetOpP) Layout() []expr.ColumnID       { return p.ColIDs }
func (p *MaterializeP) Layout() []expr.ColumnID { return p.Input.Layout() }
func (p *InsertP) Layout() []expr.ColumnID      { return nil }
func (p *UpdateP) Layout() []expr.ColumnID      { return nil }
func (p *DeleteP) Layout() []expr.ColumnID      { return nil }

func scanLayout(cols []expr.ColumnID, tid expr.ColumnID) []expr.ColumnID {
	if tid != 0 {
		return append(append([]expr.ColumnID(nil), cols...), tid)
	}
	return cols
}

func joinLayout(k JoinKind, o, i Plan) []expr.ColumnID {
	if k == JoinSemi || k == JoinAnti {
		return o.Layout()
	}
	return append(append([]expr.ColumnID(nil), o.Layout()...), i.Layout()...)
}

func (p *SeqScanP) children() []Plan     { return nil }
func (p *IndexScanP) children() []Plan   { return nil }
func (p *VScanP) children() []Plan       { return nil }
func (p *ValuesP) children() []Plan      { return nil }
func (p *FuncScanP) children() []Plan    { return nil }
func (p *FilterP) children() []Plan      { return []Plan{p.Input} }
func (p *ProjectP) children() []Plan     { return []Plan{p.Input} }
func (p *NestLoopP) children() []Plan    { return []Plan{p.Outer, p.Inner} }
func (p *HashJoinP) children() []Plan    { return []Plan{p.Outer, p.Inner} }
func (p *MergeJoinP) children() []Plan   { return []Plan{p.Outer, p.Inner} }
func (p *AggP) children() []Plan         { return []Plan{p.Input} }
func (p *SortP) children() []Plan        { return []Plan{p.Input} }
func (p *LimitP) children() []Plan       { return []Plan{p.Input} }
func (p *DistinctP) children() []Plan    { return []Plan{p.Input} }
func (p *SetOpP) children() []Plan       { return []Plan{p.Left, p.Right} }
func (p *MaterializeP) children() []Plan { return []Plan{p.Input} }
func (p *InsertP) children() []Plan      { return []Plan{p.Source} }
func (p *UpdateP) children() []Plan      { return []Plan{p.Source} }
func (p *DeleteP) children() []Plan      { return []Plan{p.Source} }

// Children returns a node's inputs.
func Children(p Plan) []Plan { return p.children() }

// ---- EXPLAIN ----

// ExplainOptions control EXPLAIN output.
type ExplainOptions struct {
	Costs   bool
	Analyze bool
	Verbose bool
}

// Explain renders a plan in PostgreSQL's text format.
func Explain(p Plan, opt ExplainOptions) []string {
	var lines []string
	var subplans []*expr.Subquery
	var walk func(p Plan, depth int, label string)
	collectSubs := func(es ...expr.Expr) {
		for _, e := range es {
			expr.Walk(e, func(x expr.Expr) bool {
				if sq, ok := x.(*expr.Subquery); ok {
					subplans = append(subplans, sq)
				}
				return true
			})
		}
	}
	walk = func(p Plan, depth int, label string) {
		if pr, ok := p.(*ProjectP); ok && !opt.Verbose && depth > 0 || ok && !opt.Verbose && len(pr.children()) == 1 && !isResultOnly(pr) {
			// Projections are folded into their input, as in PostgreSQL.
			for _, e := range pr.Exprs {
				expr.Walk(e, func(x expr.Expr) bool {
					if sq, ok := x.(*expr.Subquery); ok {
						subplans = append(subplans, sq)
					}
					return true
				})
			}
			walk(pr.Input, depth, label)
			return
		}
		indent := strings.Repeat("      ", max(depth-1, 0))
		prefix := ""
		if depth > 0 {
			prefix = indent + "  ->  "
		}
		detail := "  "
		if depth > 0 {
			detail = indent + "        "
		}
		b := p.base()
		head := prefix + nodeTitle(p)
		if opt.Costs {
			head += fmt.Sprintf("  (cost=%.2f..%.2f rows=%.0f width=%d)", b.E.Startup, b.E.Total, max(b.E.Rows, 1), b.E.Width)
		}
		if opt.Analyze {
			if b.Stats.Loops == 0 {
				head += " (never executed)"
			} else {
				loops := float64(b.Stats.Loops)
				rows := float64(b.Stats.Rows) / loops
				first := float64(b.Stats.First.Microseconds()) / 1000 / loops
				total := float64(b.Stats.Time.Microseconds()) / 1000 / loops
				head += fmt.Sprintf(" (actual time=%.3f..%.3f rows=%.0f loops=%d)", first, total, rows, b.Stats.Loops)
			}
		}
		lines = append(lines, head)
		for _, d := range nodeDetails(p, opt) {
			lines = append(lines, detail+d)
		}
		if opt.Analyze && b.Stats.Removed > 0 {
			lines = append(lines, fmt.Sprintf("%sRows Removed by Filter: %d", detail, b.Stats.Removed))
		}
		for _, e := range nodeExprs(p) {
			collectSubs(e)
		}
		for _, c := range p.children() {
			walk(c, depth+1, "")
		}
	}
	walk(p, 0, "")
	// Subplans are listed after the main plan.
	seen := map[int]bool{}
	for i := 0; i < len(subplans); i++ {
		sq := subplans[i]
		if seen[sq.ID] {
			continue
		}
		seen[sq.ID] = true
		if sp, ok := sq.Plan.(Plan); ok {
			lines = append(lines, "  "+sq.Label)
			for k, l := range Explain(sp, opt) {
				if k == 0 {
					lines = append(lines, "    ->  "+l)
				} else {
					lines = append(lines, "      "+l)
				}
			}
		}
	}
	return lines
}

// isResultOnly reports whether a projection sits on a one-row Result
// (SELECT without FROM), which PostgreSQL shows as "Result".
func isResultOnly(p *ProjectP) bool {
	v, ok := p.Input.(*ValuesP)
	return ok && len(v.Rows) == 1 && len(v.ColIDs) == 0
}

func joinName(k JoinKind) string {
	switch k {
	case JoinLeft:
		return " Left Join"
	case JoinFull:
		return " Full Join"
	case JoinSemi:
		return " Semi Join"
	case JoinAnti:
		return " Anti Join"
	}
	return ""
}

func nodeTitle(p Plan) string {
	switch x := p.(type) {
	case *SeqScanP:
		return "Seq Scan on " + relName(x.Table.Name, x.Alias)
	case *IndexScanP:
		return "Index Scan using " + x.Index.Name + " on " + relName(x.Table.Name, x.Alias)
	case *VScanP:
		return "Function Scan on " + relName(x.VT.Name, x.Alias)
	case *ValuesP:
		if len(x.Rows) == 1 && len(x.ColIDs) == 0 {
			return "Result"
		}
		return "Values Scan"
	case *FuncScanP:
		return "Function Scan on " + x.Name
	case *FilterP:
		return "Filter"
	case *ProjectP:
		return "Result"
	case *NestLoopP:
		if x.Kind == JoinInner {
			return "Nested Loop"
		}
		return "Nested Loop" + joinName(x.Kind)
	case *HashJoinP:
		if x.Kind == JoinInner {
			return "Hash Join"
		}
		return "Hash" + joinName(x.Kind)
	case *MergeJoinP:
		if x.Kind == JoinInner {
			return "Merge Join"
		}
		return "Merge" + joinName(x.Kind)
	case *AggP:
		if len(x.GroupBy) == 0 {
			return "Aggregate"
		}
		return "HashAggregate"
	case *SortP:
		if x.TopN != nil {
			return "Sort (top-N)"
		}
		return "Sort"
	case *LimitP:
		return "Limit"
	case *DistinctP:
		if len(x.On) > 0 {
			return "Unique"
		}
		return "HashAggregate (distinct)"
	case *SetOpP:
		switch x.Kind {
		case 1:
			if x.All {
				return "Append"
			}
			return "HashSetOp Union"
		case 2:
			return "HashSetOp Intersect"
		}
		return "HashSetOp Except"
	case *MaterializeP:
		return "Materialize"
	case *InsertP:
		return "Insert on " + x.Ins.Table.Name
	case *UpdateP:
		return "Update on " + x.Upd.Table.Name
	case *DeleteP:
		return "Delete on " + x.Del.Table.Name
	}
	return fmt.Sprintf("%T", p)
}

func relName(name, alias string) string {
	if alias != "" && alias != name {
		return name + " " + alias
	}
	return name
}

func exprList(es []expr.Expr) string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.String()
	}
	return strings.Join(parts, ", ")
}

func cond(e expr.Expr) string {
	s := e.String()
	if !strings.HasPrefix(s, "(") {
		s = "(" + s + ")"
	}
	return s
}

func nodeDetails(p Plan, opt ExplainOptions) []string {
	var out []string
	switch x := p.(type) {
	case *SeqScanP:
		if x.Filter != nil {
			out = append(out, "Filter: "+cond(x.Filter))
		}
	case *IndexScanP:
		if x.IndexCond != nil {
			out = append(out, "Index Cond: "+cond(x.IndexCond))
		}
		// The executor rechecks every predicate; show only those the index
		// bounds do not already implement.
		done := map[string]bool{}
		for _, c := range expr.Conjuncts(x.IndexCond) {
			done[c.String()] = true
			if call, ok := c.(*expr.Call); ok && call.Fn.Op == "=" && len(call.Args) == 2 {
				done["("+call.Args[1].String()+" = "+call.Args[0].String()+")"] = true
			}
		}
		var rest []expr.Expr
		for _, c := range expr.Conjuncts(x.Filter) {
			if !done[c.String()] {
				rest = append(rest, c)
			}
		}
		if len(rest) > 0 {
			out = append(out, "Filter: "+cond(expr.MakeAnd(rest)))
		}
	case *VScanP:
		if x.Filter != nil {
			out = append(out, "Filter: "+cond(x.Filter))
		}
	case *FilterP:
		out = append(out, "Filter: "+cond(x.Cond))
	case *ProjectP:
		if opt.Verbose {
			out = append(out, "Output: "+exprList(x.Exprs))
		}
	case *NestLoopP:
		if x.Cond != nil {
			out = append(out, "Join Filter: "+cond(x.Cond))
		}
	case *HashJoinP:
		out = append(out, "Hash Cond: "+keyCond(x.OuterKeys, x.InnerKeys))
		if x.Residual != nil {
			out = append(out, "Join Filter: "+cond(x.Residual))
		}
	case *MergeJoinP:
		out = append(out, "Merge Cond: "+keyCond(x.OuterKeys, x.InnerKeys))
		if x.Residual != nil {
			out = append(out, "Join Filter: "+cond(x.Residual))
		}
	case *AggP:
		if len(x.GroupBy) > 0 {
			out = append(out, "Group Key: "+exprList(x.GroupBy))
		}
	case *SortP:
		var keys []string
		for _, k := range x.Keys {
			s := k.E.String()
			if k.Desc {
				s += " DESC"
			}
			if k.NullsFirst != k.Desc {
				if k.NullsFirst {
					s += " NULLS FIRST"
				} else {
					s += " NULLS LAST"
				}
			}
			keys = append(keys, s)
		}
		out = append(out, "Sort Key: "+strings.Join(keys, ", "))
	case *DistinctP:
		if len(x.On) > 0 {
			out = append(out, "Unique Key: "+exprList(x.On))
		}
	}
	return out
}

func keyCond(o, i []expr.Expr) string {
	parts := make([]string, len(o))
	for k := range o {
		parts[k] = "(" + o[k].String() + " = " + i[k].String() + ")"
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, " AND ") + ")"
}

// nodeExprs returns the expressions a node evaluates (to find subplans).
func nodeExprs(p Plan) []expr.Expr {
	var es []expr.Expr
	add := func(e ...expr.Expr) {
		for _, x := range e {
			if x != nil {
				es = append(es, x)
			}
		}
	}
	switch x := p.(type) {
	case *SeqScanP:
		add(x.Filter)
	case *IndexScanP:
		add(x.Filter)
		add(x.Eq...)
		add(x.Lo, x.Hi)
	case *VScanP:
		add(x.Filter)
	case *ValuesP:
		for _, r := range x.Rows {
			add(r...)
		}
	case *FilterP:
		add(x.Cond)
	case *ProjectP:
		add(x.Exprs...)
	case *NestLoopP:
		add(x.Cond)
	case *HashJoinP:
		add(x.Residual)
	case *MergeJoinP:
		add(x.Residual)
	case *AggP:
		add(x.GroupBy...)
		for _, a := range x.Aggs {
			add(a.Args...)
			add(a.Filter)
		}
	case *InsertP:
		add(x.Ins.Exprs...)
		add(x.Ins.Returning...)
	case *UpdateP:
		for _, e := range x.Upd.Set {
			add(e)
		}
		add(x.Upd.Returning...)
	case *DeleteP:
		add(x.Del.Returning...)
	}
	return es
}

// NodeExprs is exported for the executor (subplan discovery).
func NodeExprs(p Plan) []expr.Expr { return nodeExprs(p) }
