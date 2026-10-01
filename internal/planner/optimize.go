package planner

import (
	"math"
	"math/bits"
	"sort"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/sql"
	"github.com/useless-husband/basalt/internal/types"
)

// Optimizer turns a logical plan into a physical plan.
type Optimizer struct {
	Cat *catalog.Catalog
	// Pages returns the current size of a table in pages.
	Pages func(*catalog.Table) int
	// Setting returns a planner setting such as enable_hashjoin.
	Setting  func(name string) string
	Rewriter *Rewriter
	binder   *Binder
	cols     []ColInfo
	planned  map[Node]Plan
}

// NewOptimizer returns an optimizer for plans built by binder b.
func NewOptimizer(cat *catalog.Catalog, b *Binder, rw *Rewriter, pages func(*catalog.Table) int, setting func(string) string) *Optimizer {
	return &Optimizer{Cat: cat, Pages: pages, Setting: setting, Rewriter: rw, binder: b, planned: map[Node]Plan{}}
}

func (o *Optimizer) enabled(name string) bool {
	if o.Setting == nil {
		return true
	}
	v := o.Setting(name)
	return v != "off" && v != "false" && v != "0"
}

// Optimize rewrites and plans a logical plan.
func (o *Optimizer) Optimize(n Node) (Plan, error) {
	// The binder may have added columns since the optimizer was created.
	o.cols = o.binder.cols
	n = o.Rewriter.Rewrite(n)
	p, err := o.plan(n)
	if err != nil {
		return nil, err
	}
	markNeeded(p)
	return p, nil
}

// markNeeded records, for every table scan of the plan, which of its
// columns are read by some operator (or are part of the plan's output), so
// that the others need not be decoded. Subquery plans are optimized, and
// marked, separately; their correlated references count as reads here.
func markNeeded(p Plan) {
	used := usedColumns(p)
	for _, id := range p.Layout() {
		used[id] = true
	}
	var walk func(Plan)
	walk = func(p Plan) {
		var cols []expr.ColumnID
		var need *[]bool
		switch x := p.(type) {
		case *SeqScanP:
			cols, need = x.ColIDs, &x.Need
		case *IndexScanP:
			cols, need = x.ColIDs, &x.Need
		}
		if need != nil {
			n := make([]bool, len(cols))
			all := true
			for i, id := range cols {
				n[i] = used[id]
				all = all && n[i]
			}
			if !all {
				*need = n
			}
		}
		for _, c := range p.children() {
			walk(c)
		}
	}
	walk(p)
}

// planExprs plans the subqueries contained in expressions.
func (o *Optimizer) planExprs(es ...expr.Expr) error {
	var err error
	for _, e := range es {
		expr.Walk(e, func(x expr.Expr) bool {
			if err != nil {
				return false
			}
			sq, ok := x.(*expr.Subquery)
			if !ok {
				return true
			}
			if ln, ok := sq.Plan.(Node); ok {
				if p, done := o.planned[ln]; done {
					sq.Plan = p
				} else {
					var p Plan
					p, err = o.Optimize(ln)
					if err == nil {
						o.planned[ln] = p
						sq.Plan = p
					}
				}
			}
			return true
		})
	}
	return err
}

func (o *Optimizer) fold(e expr.Expr) expr.Expr { return o.Rewriter.Fold(e) }

func (o *Optimizer) plan(n Node) (Plan, error) {
	switch x := n.(type) {
	case *Scan:
		return o.planScan(x)
	case *VScan:
		p := &VScanP{VT: x.VT, Alias: x.Alias, ColIDs: x.ColIDs, Filter: expr.MakeAnd(x.Filters)}
		rows := 100.0
		for _, f := range x.Filters {
			rows *= o.sel(f)
		}
		p.E = Est{Rows: math.Max(rows, 1), Total: 1 + 100*cpuTupleCost, Width: o.width(x.ColIDs)}
		return p, o.planExprs(x.Filters...)
	case *Values:
		p := &ValuesP{Rows: x.Rows, ColIDs: x.ColIDs}
		p.E = Est{Rows: float64(len(x.Rows)), Total: float64(len(x.Rows)) * cpuOperatorCost, Width: o.width(x.ColIDs)}
		for _, r := range x.Rows {
			if err := o.planExprs(r...); err != nil {
				return nil, err
			}
		}
		return p, nil
	case *FuncScan:
		p := &FuncScanP{Name: x.Name, Args: x.Args, ColIDs: x.ColIDs, T: x.T}
		rows := 1000.0
		if x.Name == "generate_series" && len(x.Args) >= 2 {
			a, aok := x.Args[0].(*expr.Const)
			b, bok := x.Args[1].(*expr.Const)
			if aok && bok && a.V.K == types.KInt && b.V.K == types.KInt {
				rows = math.Max(1, float64(b.V.I-a.V.I+1))
			}
		}
		p.E = Est{Rows: rows, Total: rows * cpuOperatorCost, Width: o.width(x.ColIDs)}
		return p, o.planExprs(x.Args...)
	case *Filter:
		in, err := o.plan(x.Input)
		if err != nil {
			return nil, err
		}
		return o.filter(in, x.Cond)
	case *Project:
		in, err := o.plan(x.Input)
		if err != nil {
			return nil, err
		}
		p := &ProjectP{Input: in, Exprs: x.Exprs, ColIDs: x.ColIDs}
		ie := Estimate(in)
		p.E = Est{Rows: ie.Rows, Startup: ie.Startup, Total: ie.Total + ie.Rows*cpuOperatorCost*float64(len(x.Exprs)), Width: o.width(x.ColIDs)}
		return p, o.planExprs(x.Exprs...)
	case *Join:
		if x.Kind == JoinInner {
			return o.planJoinRegion(x)
		}
		return o.planOuterJoin(x)
	case *Aggregate:
		in, err := o.plan(x.Input)
		if err != nil {
			return nil, err
		}
		p := &AggP{Input: in, GroupBy: x.GroupBy, GroupCols: x.GroupCols, Aggs: x.Aggs}
		ie := Estimate(in)
		groups := 1.0
		if len(x.GroupBy) > 0 {
			for _, g := range x.GroupBy {
				if c, ok := g.(*expr.Col); ok {
					groups *= o.nDistinct(c.ID)
				} else {
					groups *= defaultNDistinct
				}
			}
			groups = math.Min(groups, math.Max(ie.Rows, 1))
		}
		work := ie.Rows * cpuOperatorCost * float64(len(x.GroupBy)+len(x.Aggs))
		p.E = Est{Rows: groups, Startup: ie.Total + work, Total: ie.Total + work + groups*cpuTupleCost, Width: o.width(p.Layout())}
		var es []expr.Expr
		es = append(es, x.GroupBy...)
		for _, a := range x.Aggs {
			es = append(es, a.Args...)
			if a.Filter != nil {
				es = append(es, a.Filter)
			}
		}
		return p, o.planExprs(es...)
	case *Sort:
		in, err := o.plan(x.Input)
		if err != nil {
			return nil, err
		}
		return o.sort(in, x.Keys, nil)
	case *Limit:
		return o.planLimit(x)
	case *Distinct:
		in, err := o.plan(x.Input)
		if err != nil {
			return nil, err
		}
		p := &DistinctP{Input: in, On: x.On}
		ie := Estimate(in)
		rows := math.Max(1, ie.Rows*0.5)
		if len(x.On) > 0 {
			rows = math.Max(1, ie.Rows*0.2)
		}
		p.E = Est{Rows: rows, Startup: ie.Total, Total: ie.Total + ie.Rows*cpuOperatorCost, Width: ie.Width}
		return p, o.planExprs(x.On...)
	case *SetOp:
		l, err := o.plan(x.Left)
		if err != nil {
			return nil, err
		}
		r, err := o.plan(x.Right)
		if err != nil {
			return nil, err
		}
		p := &SetOpP{Kind: x.Kind, All: x.All, Left: l, Right: r, ColIDs: x.ColIDs}
		le, re := Estimate(l), Estimate(r)
		rows := le.Rows + re.Rows
		if x.Kind != int(sql.SetUnion) {
			rows = le.Rows
		}
		p.E = Est{Rows: rows, Startup: le.Total + re.Total, Total: le.Total + re.Total + rows*cpuOperatorCost, Width: le.Width}
		return p, nil
	}
	return nil, pgerr.Internal("cannot plan %T", n)
}

func (o *Optimizer) filter(in Plan, c expr.Expr) (Plan, error) {
	ie := Estimate(in)
	p := &FilterP{Input: in, Cond: c}
	p.E = Est{Rows: math.Max(1, ie.Rows*o.sel(c)), Startup: ie.Startup, Total: ie.Total + ie.Rows*cpuOperatorCost, Width: ie.Width}
	return p, o.planExprs(c)
}

func (o *Optimizer) sort(in Plan, keys []expr.SortKey, topN expr.Expr) (Plan, error) {
	ie := Estimate(in)
	p := &SortP{Input: in, Keys: keys, TopN: topN}
	n := math.Max(ie.Rows, 1)
	p.E = Est{Rows: ie.Rows, Startup: ie.Total + sortCost(n), Total: ie.Total + sortCost(n) + n*cpuOperatorCost, Width: ie.Width}
	var es []expr.Expr
	for _, k := range keys {
		es = append(es, k.E)
	}
	return p, o.planExprs(es...)
}

// ---- scans and index selection ----

// indexMatch describes how predicates map onto an index.
type indexMatch struct {
	ix             *catalog.Index
	eq             []expr.Expr
	lo, hi         expr.Expr
	loIncl, hiIncl bool
	used           []expr.Expr // predicates implemented by the bounds
	sel            float64
}

// matchIndex finds equality predicates on a prefix of the index columns
// and a range on the next column. Predicate values must be computable
// before the scan (constants, parameters or outer columns).
func (o *Optimizer) matchIndex(ix *catalog.Index, colIDs []expr.ColumnID, preds []expr.Expr) indexMatch {
	own := colSet(colIDs)
	m := indexMatch{ix: ix, sel: 1}
	type cmp struct {
		op   string
		val  expr.Expr
		pred expr.Expr
	}
	byCol := map[expr.ColumnID][]cmp{}
	for _, p := range preds {
		c, ok := p.(*expr.Call)
		if !ok || len(c.Args) != 2 || !expr.IsComparison(c.Fn.Op) || c.Fn.Op == "<>" {
			continue
		}
		l, r := c.Args[0], c.Args[1]
		if lc, ok := l.(*expr.Col); ok && own[lc.ID] && isParamLike(r, own) && r.Type().Oid == lc.T.Oid {
			byCol[lc.ID] = append(byCol[lc.ID], cmp{c.Fn.Op, r, p})
		} else if rc, ok := r.(*expr.Col); ok && own[rc.ID] && isParamLike(l, own) && l.Type().Oid == rc.T.Oid {
			byCol[rc.ID] = append(byCol[rc.ID], cmp{expr.CommuteOp(c.Fn.Op), l, p})
		}
	}
	for _, pos := range ix.Columns {
		id := colIDs[pos]
		cs := byCol[id]
		found := false
		for _, c := range cs {
			if c.op == "=" {
				m.eq = append(m.eq, c.val)
				m.used = append(m.used, c.pred)
				m.sel *= o.sel(c.pred)
				found = true
				break
			}
		}
		if found {
			continue
		}
		for _, c := range cs {
			switch c.op {
			case ">", ">=":
				if m.lo == nil {
					m.lo, m.loIncl = c.val, c.op == ">="
					m.used = append(m.used, c.pred)
					m.sel *= o.sel(c.pred)
				}
			case "<", "<=":
				if m.hi == nil {
					m.hi, m.hiIncl = c.val, c.op == "<="
					m.used = append(m.used, c.pred)
					m.sel *= o.sel(c.pred)
				}
			}
		}
		break
	}
	return m
}

func (o *Optimizer) indexHeight(rows float64) float64 {
	return math.Max(1, math.Ceil(math.Log(math.Max(rows, 2))/math.Log(200)))
}

// indexScanCost estimates fetching matched rows through an index.
func (o *Optimizer) indexScanCost(t *catalog.Table, matched float64) (startup, total float64) {
	pages := o.tablePages(t)
	startup = o.indexHeight(o.tableRows(t)) * randomPageCost * 0.25
	heapPages := math.Min(matched, pages)
	total = startup + matched*(cpuIndexTupleCost+cpuTupleCost) + heapPages*randomPageCost*0.5 + matched*cpuOperatorCost
	return startup, total
}

func (o *Optimizer) seqScan(s *Scan) *SeqScanP {
	t := s.Table
	rows := o.tableRows(t)
	p := &SeqScanP{Table: t, Alias: s.Alias, ColIDs: s.ColIDs, TIDCol: s.TIDCol, Filter: expr.MakeAnd(s.Filters), Lock: s.Lock}
	sel := 1.0
	for _, f := range s.Filters {
		sel *= o.sel(f)
	}
	total := o.tablePages(t)*seqPageCost + rows*cpuTupleCost + rows*cpuOperatorCost*float64(len(s.Filters))
	if !o.enabled("enable_seqscan") {
		total += disableCost
	}
	p.E = Est{Rows: math.Max(1, rows*sel), Total: total, Width: o.width(s.ColIDs)}
	return p
}

func (o *Optimizer) indexScan(s *Scan, m indexMatch, filters []expr.Expr) *IndexScanP {
	t := s.Table
	rows := o.tableRows(t)
	sel := 1.0
	for _, f := range filters {
		sel *= o.sel(f)
	}
	matched := math.Max(1, rows*m.sel)
	if m.ix.Unique && len(m.eq) == len(m.ix.Columns) {
		matched = 1
	}
	startup, total := o.indexScanCost(t, matched)
	if !o.enabled("enable_indexscan") {
		startup += disableCost
		total += disableCost
	}
	p := &IndexScanP{Table: t, Alias: s.Alias, Index: m.ix, ColIDs: s.ColIDs, TIDCol: s.TIDCol,
		Eq: m.eq, Lo: m.lo, Hi: m.hi, LoIncl: m.loIncl, HiIncl: m.hiIncl, Lock: s.Lock,
		IndexCond: expr.MakeAnd(m.used)}
	// Recheck every predicate: cheap, and robust against any mismatch
	// between the bounds and the predicates' exact semantics.
	p.Filter = expr.MakeAnd(filters)
	p.E = Est{Rows: math.Max(1, math.Min(matched, rows*sel)), Startup: startup, Total: total, Width: o.width(s.ColIDs)}
	return p
}

func (o *Optimizer) planScan(s *Scan) (Plan, error) {
	if err := o.planExprs(s.Filters...); err != nil {
		return nil, err
	}
	var best Plan = o.seqScan(s)
	for _, ix := range o.Cat.TableIndexes(s.Table) {
		m := o.matchIndex(ix, s.ColIDs, s.Filters)
		if len(m.eq) == 0 && m.lo == nil && m.hi == nil {
			continue
		}
		p := o.indexScan(s, m, s.Filters)
		if p.E.Total < Estimate(best).Total {
			best = p
		}
	}
	return best, nil
}

// orderedScan returns an index scan of s whose order satisfies keys, if an
// index provides it.
func (o *Optimizer) orderedScan(s *Scan, keys []expr.SortKey) *IndexScanP {
	for _, ix := range o.Cat.TableIndexes(s.Table) {
		if len(keys) > len(ix.Columns) {
			continue
		}
		ok := true
		for i, k := range keys {
			c, isCol := k.E.(*expr.Col)
			if !isCol || k.Desc || k.NullsFirst || c.ID != s.ColIDs[ix.Columns[i]] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		m := o.matchIndex(ix, s.ColIDs, s.Filters)
		if len(m.eq) > 0 {
			// Equality on a prefix still yields index order for the rest,
			// but keep it simple: only use range bounds on the first column.
			m = indexMatch{ix: ix, sel: 1, lo: nil}
		}
		p := o.indexScan(s, m, s.Filters)
		if m.lo == nil && m.hi == nil {
			rows := o.tableRows(s.Table)
			p.E.Startup, p.E.Total = o.indexScanCost(s.Table, rows)
			p.E.Total += rows * randomPageCost * 0.25 // unclustered heap access
			if !o.enabled("enable_indexscan") {
				p.E.Startup += disableCost
				p.E.Total += disableCost
			}
		}
		return p
	}
	return nil
}

func (o *Optimizer) planLimit(x *Limit) (Plan, error) {
	if err := o.planExprs(x.Limit, x.Offset); err != nil {
		return nil, err
	}
	// Look for Limit -> [Project ->] Sort -> [Filter/]Scan to use an
	// ordered index scan or a top-N sort.
	var proj *Project
	in := x.Input
	if p, ok := in.(*Project); ok {
		proj, in = p, p.Input
	}
	var child Plan
	if srt, ok := in.(*Sort); ok && x.Limit != nil {
		sub, err := o.plan(srt.Input)
		if err != nil {
			return nil, err
		}
		var topN expr.Expr = x.Limit
		if x.Offset != nil {
			topN = &expr.Call{Fn: expr.MustBinary("+", types.Int8), Args: []expr.Expr{x.Limit, x.Offset}, T: types.Int8}
		}
		best, err := o.sort(sub, srt.Keys, topN)
		if err != nil {
			return nil, err
		}
		want := limitRows(x)
		if scan, ok := srt.Input.(*Scan); ok {
			if alt := o.orderedScan(scan, srt.Keys); alt != nil {
				if limitCost(alt, want) < limitCost(best, want) {
					best = alt
				}
			}
		}
		child = best
		if proj != nil {
			ie := Estimate(child)
			pp := &ProjectP{Input: child, Exprs: proj.Exprs, ColIDs: proj.ColIDs}
			pp.E = Est{Rows: ie.Rows, Startup: ie.Startup, Total: ie.Total, Width: o.width(proj.ColIDs)}
			if err := o.planExprs(proj.Exprs...); err != nil {
				return nil, err
			}
			child = pp
		}
	} else {
		var err error
		if child, err = o.plan(x.Input); err != nil {
			return nil, err
		}
	}
	p := &LimitP{Input: child, Limit: x.Limit, Offset: x.Offset}
	ce := Estimate(child)
	want := limitRows(x)
	rows := ce.Rows
	if want >= 0 {
		rows = math.Min(rows, want)
	}
	p.E = Est{Rows: rows, Startup: ce.Startup, Total: limitCost(child, want), Width: ce.Width}
	return p, nil
}

func limitRows(x *Limit) float64 {
	if x.Limit == nil {
		return -1
	}
	n := 100.0 // parameter: guess
	if c, ok := x.Limit.(*expr.Const); ok && !c.V.IsNull() {
		n = float64(c.V.I)
	}
	if x.Offset != nil {
		if c, ok := x.Offset.(*expr.Const); ok && !c.V.IsNull() {
			n += float64(c.V.I)
		}
	}
	return n
}

func limitCost(p Plan, want float64) float64 {
	e := Estimate(p)
	if want < 0 || e.Rows <= 0 {
		return e.Total
	}
	frac := math.Min(1, want/e.Rows)
	return e.Startup + (e.Total-e.Startup)*frac
}

// ---- joins ----

type joinRel struct {
	node Node
	plan Plan
	cols map[expr.ColumnID]bool
	scan *Scan
}

type joinPred struct {
	e    expr.Expr
	mask uint64
}

func flattenInner(n Node, leaves *[]Node, preds *[]expr.Expr) {
	if j, ok := n.(*Join); ok && j.Kind == JoinInner {
		flattenInner(j.Left, leaves, preds)
		flattenInner(j.Right, leaves, preds)
		*preds = append(*preds, expr.Conjuncts(j.Cond)...)
		return
	}
	*leaves = append(*leaves, n)
}

// dpLimit is the largest join for which every join order is considered
// (dynamic programming over subsets, 3^n splits); larger joins are
// ordered greedily.
const dpLimit = 8

func (o *Optimizer) planJoinRegion(j *Join) (Plan, error) {
	var leaves []Node
	var preds []expr.Expr
	flattenInner(j, &leaves, &preds)
	if len(leaves) > 64 {
		return nil, pgerr.New(pgerr.StatementTooComplex, "too many tables in one join (%d)", len(leaves))
	}
	rels := make([]*joinRel, len(leaves))
	for i, l := range leaves {
		p, err := o.plan(l)
		if err != nil {
			return nil, err
		}
		r := &joinRel{node: l, plan: p, cols: colSet(l.Cols())}
		if s, ok := l.(*Scan); ok {
			r.scan = s
		}
		rels[i] = r
	}
	if err := o.planExprs(preds...); err != nil {
		return nil, err
	}
	var jp []joinPred
	var top []expr.Expr
	for _, p := range preds {
		var mask uint64
		for id := range expr.Columns(p) {
			for i, r := range rels {
				if r.cols[id] {
					mask |= 1 << i
				}
			}
		}
		switch {
		case mask == 0 || expr.IsVolatile(p):
			top = append(top, p)
		case bits.OnesCount64(mask) == 1:
			i := bits.TrailingZeros64(mask)
			fp, err := o.filter(rels[i].plan, p)
			if err != nil {
				return nil, err
			}
			rels[i].plan = fp
			rels[i].scan = nil
		default:
			jp = append(jp, joinPred{p, mask})
		}
	}
	n := len(rels)
	full := uint64(1)<<n - 1
	rowsCache := map[uint64]float64{}
	rowsOf := func(mask uint64) float64 {
		if r, ok := rowsCache[mask]; ok {
			return r
		}
		r := 1.0
		for i := 0; i < n; i++ {
			if mask&(1<<i) != 0 {
				r *= Estimate(rels[i].plan).Rows
			}
		}
		for _, p := range jp {
			if p.mask&mask == p.mask {
				r *= o.sel(p.e)
			}
		}
		r = math.Max(1, r)
		rowsCache[mask] = r
		return r
	}
	predsFor := func(a, b uint64) []expr.Expr {
		var out []expr.Expr
		s := a | b
		for _, p := range jp {
			if p.mask&s == p.mask && p.mask&a != 0 && p.mask&b != 0 {
				out = append(out, p.e)
			}
		}
		return out
	}
	var result Plan
	if n <= dpLimit {
		best := make(map[uint64]Plan, 1<<n)
		for i := 0; i < n; i++ {
			best[1<<i] = rels[i].plan
		}
		var subsets []uint64
		for s := uint64(1); s <= full; s++ {
			if bits.OnesCount64(s) >= 2 {
				subsets = append(subsets, s)
			}
		}
		sort.Slice(subsets, func(i, k int) bool { return bits.OnesCount64(subsets[i]) < bits.OnesCount64(subsets[k]) })
		for _, s := range subsets {
			connectedExists := false
			for a := (s - 1) & s; a > 0; a = (a - 1) & s {
				if len(predsFor(a, s^a)) > 0 && best[a] != nil && best[s^a] != nil {
					connectedExists = true
					break
				}
			}
			var bestPlan Plan
			for a := (s - 1) & s; a > 0; a = (a - 1) & s {
				b := s ^ a
				pa, pb := best[a], best[b]
				if pa == nil || pb == nil {
					continue
				}
				ps := predsFor(a, b)
				if connectedExists && len(ps) == 0 {
					continue
				}
				var innerRel *joinRel
				if bits.OnesCount64(b) == 1 {
					innerRel = rels[bits.TrailingZeros64(b)]
				}
				for _, c := range o.joinCandidates(JoinInner, pa, pb, innerRel, ps, rowsOf(s)) {
					if bestPlan == nil || Estimate(c).Total < Estimate(bestPlan).Total {
						bestPlan = c
					}
				}
			}
			best[s] = bestPlan
		}
		result = best[full]
	} else {
		// Greedy: repeatedly add the relation that is cheapest to join.
		mask := uint64(1)
		cur := rels[0].plan
		for mask != full {
			var bestPlan Plan
			var bestI int
			bestCost := math.Inf(1)
			for i := 0; i < n; i++ {
				if mask&(1<<i) != 0 {
					continue
				}
				ps := predsFor(mask, 1<<i)
				for _, c := range o.joinCandidates(JoinInner, cur, rels[i].plan, rels[i], ps, rowsOf(mask|1<<i)) {
					cost := Estimate(c).Total
					if len(ps) == 0 {
						cost += disableCost / 2 // avoid cross products while a join predicate is available
					}
					if cost < bestCost {
						bestPlan, bestI, bestCost = c, i, cost
					}
				}
			}
			cur = bestPlan
			mask |= 1 << bestI
		}
		result = cur
	}
	if result == nil {
		return nil, pgerr.Internal("join planning failed")
	}
	if len(top) > 0 {
		return o.filter(result, expr.MakeAnd(top))
	}
	return result, nil
}

// splitKeys separates equi-join keys (outer expr = inner expr) from the
// remaining predicates.
func splitKeys(preds []expr.Expr, outer, inner map[expr.ColumnID]bool) (ok, ik, rest []expr.Expr) {
	all := map[expr.ColumnID]bool{}
	for k := range outer {
		all[k] = true
	}
	for k := range inner {
		all[k] = true
	}
	for _, p := range preds {
		c, isCall := p.(*expr.Call)
		if isCall && c.Fn.Op == "=" && len(c.Args) == 2 && !expr.IsVolatile(p) && !expr.HasSubquery(p) {
			l, r := c.Args[0], c.Args[1]
			lo, li := refersOnly(l, outer, all) && refersAny(l, outer), refersOnly(l, inner, all) && refersAny(l, inner)
			ro, ri := refersOnly(r, outer, all) && refersAny(r, outer), refersOnly(r, inner, all) && refersAny(r, inner)
			if lo && ri {
				ok, ik = append(ok, l), append(ik, r)
				continue
			}
			if li && ro {
				ok, ik = append(ok, r), append(ik, l)
				continue
			}
		}
		rest = append(rest, p)
	}
	return
}

// usedColumns returns the columns referenced anywhere in a plan subtree.
func usedColumns(p Plan) map[expr.ColumnID]bool {
	out := map[expr.ColumnID]bool{}
	var walk func(Plan)
	walk = func(p Plan) {
		for _, e := range nodeExprs(p) {
			for id := range expr.Columns(e) {
				out[id] = true
			}
		}
		if s, ok := p.(*SortP); ok {
			for _, k := range s.Keys {
				for id := range expr.Columns(k.E) {
					out[id] = true
				}
			}
		}
		if s, ok := p.(*DistinctP); ok {
			for _, e := range s.On {
				for id := range expr.Columns(e) {
					out[id] = true
				}
			}
		}
		for _, c := range p.children() {
			walk(c)
		}
	}
	walk(p)
	return out
}

// dependsOn reports whether plan p reads any of the given columns (so it
// must be re-executed for each outer row).
func dependsOn(p Plan, cols map[expr.ColumnID]bool) bool {
	for id := range usedColumns(p) {
		if cols[id] {
			return true
		}
	}
	return false
}

// joinCandidates returns physical alternatives for joining outer with inner.
func (o *Optimizer) joinCandidates(kind JoinKind, outer, inner Plan, innerRel *joinRel, preds []expr.Expr, outRows float64) []Plan {
	oc, ic := colSet(outer.Layout()), colSet(inner.Layout())
	ok, ik, rest := splitKeys(preds, oc, ic)
	oe, ie := Estimate(outer), Estimate(inner)
	width := oe.Width + ie.Width
	if kind == JoinSemi || kind == JoinAnti {
		width = oe.Width
	}
	var out []Plan
	penalty := func(setting string) float64 {
		if o.enabled(setting) {
			return 0
		}
		return disableCost
	}
	// Hash join.
	if len(ok) > 0 {
		hj := &HashJoinP{Kind: kind, Outer: outer, Inner: inner, OuterKeys: ok, InnerKeys: ik, Residual: expr.MakeAnd(rest)}
		build := ie.Total + ie.Rows*(cpuOperatorCost+cpuTupleCost)
		total := build + oe.Total + oe.Rows*cpuOperatorCost*float64(len(ok)) + outRows*cpuTupleCost + penalty("enable_hashjoin")
		hj.E = Est{Rows: outRows, Startup: build + oe.Startup + penalty("enable_hashjoin"), Total: total, Width: width}
		out = append(out, hj)
	}
	// Merge join (inner and left joins) over sorted inputs.
	if len(ok) > 0 && (kind == JoinInner || kind == JoinLeft) {
		so, si := outer, inner
		var okeys, ikeys []expr.SortKey
		for k := range ok {
			okeys = append(okeys, expr.SortKey{E: ok[k]})
			ikeys = append(ikeys, expr.SortKey{E: ik[k]})
		}
		so, _ = o.sort(outer, okeys, nil)
		si, _ = o.sort(inner, ikeys, nil)
		mj := &MergeJoinP{Kind: kind, Outer: so, Inner: si, OuterKeys: ok, InnerKeys: ik, Residual: expr.MakeAnd(rest)}
		soe, sie := Estimate(so), Estimate(si)
		total := soe.Total + sie.Total + (oe.Rows+ie.Rows)*cpuOperatorCost + outRows*cpuTupleCost + penalty("enable_mergejoin")
		mj.E = Est{Rows: outRows, Startup: soe.Startup + sie.Startup + penalty("enable_mergejoin"), Total: total, Width: width}
		out = append(out, mj)
	}
	// Nested loop with a parameterized index scan on the inner relation.
	if innerRel != nil && innerRel.scan != nil && kind != JoinFull {
		s := innerRel.scan
		var params []expr.Expr
		for k := range ok {
			if c, isCol := ik[k].(*expr.Col); isCol {
				params = append(params, &expr.Call{Fn: expr.CompareFunc("=", c.T), Args: []expr.Expr{c, ok[k]}, T: types.Bool})
			}
		}
		if len(params) > 0 {
			for _, ix := range o.Cat.TableIndexes(s.Table) {
				all := append(append([]expr.Expr(nil), s.Filters...), params...)
				m := o.matchIndex(ix, s.ColIDs, all)
				if len(m.eq) == 0 {
					continue
				}
				// The inner scan rechecks its own filters and the join keys.
				isp := o.indexScan(s, m, append(append([]expr.Expr(nil), s.Filters...), preds...))
				nl := &NestLoopP{Kind: kind, Outer: outer, Inner: isp}
				per := Estimate(isp)
				total := oe.Total + oe.Rows*per.Total + outRows*cpuTupleCost + penalty("enable_nestloop")
				nl.E = Est{Rows: outRows, Startup: oe.Startup + per.Startup + penalty("enable_nestloop"), Total: total, Width: width}
				out = append(out, nl)
			}
		}
	}
	// Plain nested loop: rescan (materialized) inner for each outer row.
	{
		in := inner
		rescan := ie.Total
		if !dependsOn(inner, oc) {
			m := &MaterializeP{Input: inner}
			m.E = Est{Rows: ie.Rows, Startup: ie.Startup, Total: ie.Total + ie.Rows*cpuOperatorCost, Width: ie.Width}
			in = m
			rescan = ie.Rows * cpuOperatorCost
		}
		nl := &NestLoopP{Kind: kind, Outer: outer, Inner: in, Cond: expr.MakeAnd(preds)}
		total := oe.Total + ie.Total + math.Max(oe.Rows-1, 0)*rescan + oe.Rows*ie.Rows*cpuOperatorCost*float64(max(len(preds), 1)) + outRows*cpuTupleCost + penalty("enable_nestloop")
		nl.E = Est{Rows: outRows, Startup: oe.Startup + ie.Startup + penalty("enable_nestloop"), Total: total, Width: width}
		out = append(out, nl)
	}
	return out
}

func (o *Optimizer) planOuterJoin(j *Join) (Plan, error) {
	l, err := o.plan(j.Left)
	if err != nil {
		return nil, err
	}
	r, err := o.plan(j.Right)
	if err != nil {
		return nil, err
	}
	preds := expr.Conjuncts(j.Cond)
	if err := o.planExprs(preds...); err != nil {
		return nil, err
	}
	le, re := Estimate(l), Estimate(r)
	sel := 1.0
	for _, p := range preds {
		sel *= o.sel(p)
	}
	rows := le.Rows * re.Rows * sel
	switch j.Kind {
	case JoinLeft:
		rows = math.Max(rows, le.Rows)
	case JoinFull:
		rows = math.Max(rows, le.Rows+re.Rows)
	case JoinSemi:
		rows = le.Rows * math.Min(1, sel*re.Rows)
	case JoinAnti:
		rows = le.Rows * math.Max(0.1, 1-math.Min(1, sel*re.Rows))
	}
	var innerRel *joinRel
	if s, ok := j.Right.(*Scan); ok {
		innerRel = &joinRel{node: s, plan: r, cols: colSet(s.Cols()), scan: s}
	}
	cands := o.joinCandidates(j.Kind, l, r, innerRel, preds, math.Max(1, rows))
	var best Plan
	for _, c := range cands {
		if best == nil || Estimate(c).Total < Estimate(best).Total {
			best = c
		}
	}
	return best, nil
}

// ---- DML ----

// PlanInsert plans an INSERT.
func (o *Optimizer) PlanInsert(ins *Insert) (Plan, error) {
	src, err := o.Optimize(ins.Source)
	if err != nil {
		return nil, err
	}
	for i := range ins.Exprs {
		ins.Exprs[i] = o.fold(ins.Exprs[i])
	}
	if err := o.planExprs(append(append([]expr.Expr(nil), ins.Exprs...), ins.Returning...)...); err != nil {
		return nil, err
	}
	p := &InsertP{Ins: ins, Source: src}
	se := Estimate(src)
	p.E = Est{Rows: 0, Startup: se.Total, Total: se.Total + se.Rows*cpuTupleCost*10}
	return p, nil
}

// PlanUpdate plans an UPDATE.
func (o *Optimizer) PlanUpdate(up *Update) (Plan, error) {
	src, err := o.Optimize(up.Source)
	if err != nil {
		return nil, err
	}
	var es []expr.Expr
	for i, e := range up.Set {
		up.Set[i] = o.fold(e)
		es = append(es, up.Set[i])
	}
	if err := o.planExprs(append(es, up.Returning...)...); err != nil {
		return nil, err
	}
	p := &UpdateP{Upd: up, Source: src}
	se := Estimate(src)
	p.E = Est{Startup: se.Total, Total: se.Total + se.Rows*cpuTupleCost*10}
	return p, nil
}

// PlanDelete plans a DELETE.
func (o *Optimizer) PlanDelete(del *Delete) (Plan, error) {
	src, err := o.Optimize(del.Source)
	if err != nil {
		return nil, err
	}
	if err := o.planExprs(del.Returning...); err != nil {
		return nil, err
	}
	p := &DeleteP{Del: del, Source: src}
	se := Estimate(src)
	p.E = Est{Startup: se.Total, Total: se.Total + se.Rows*cpuTupleCost*10}
	return p, nil
}

// UsedColumns returns the columns referenced anywhere in a plan subtree.
func UsedColumns(p Plan) map[expr.ColumnID]bool { return usedColumns(p) }
