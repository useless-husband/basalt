package planner

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/types"
)

// Cost constants, PostgreSQL's defaults. random_page_cost and
// effective_cache_size are settings (see randomPageCost and cachePages).
const (
	seqPageCost       = 1.0
	cpuTupleCost      = 0.01
	cpuIndexTupleCost = 0.005
	cpuOperatorCost   = 0.0025
	disableCost       = 1.0e10

	defaultEqSel     = 0.005
	defaultRangeSel  = 1.0 / 3
	defaultSel       = 0.25
	defaultNDistinct = 200.0

	// basalt's default random_page_cost is 1.1, the value commonly
	// recommended for SSDs, rather than PostgreSQL's 4.0 (which models
	// spinning disks): basalt's pages come from its buffer pool or an SSD.
	defaultRandomPageCost = 1.1
	defaultCachePages     = 16384
)

func (o *Optimizer) randomPageCost() float64 {
	if o.Setting != nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(o.Setting("random_page_cost")), 64); err == nil && v >= 0 {
			return v
		}
	}
	return defaultRandomPageCost
}

// cachePages returns effective_cache_size in pages: how much of the
// database the planner assumes stays cached across repeated index probes.
func (o *Optimizer) cachePages() float64 {
	if o.Setting == nil {
		return defaultCachePages
	}
	v := strings.TrimSpace(o.Setting("effective_cache_size"))
	unit := 8.0 // kB per unit; a bare number counts pages, as in PostgreSQL
	for _, u := range []struct {
		suffix string
		kb     float64
	}{{"kB", 1}, {"MB", 1024}, {"GB", 1024 * 1024}, {"TB", 1024 * 1024 * 1024}} {
		if strings.HasSuffix(v, u.suffix) {
			v, unit = strings.TrimSpace(strings.TrimSuffix(v, u.suffix)), u.kb
			break
		}
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 {
		return defaultCachePages
	}
	return math.Max(1, n*unit/8)
}

// pagesFetched is the Mackert-Lohman estimate of the distinct page reads
// needed to fetch n tuples at random from a relation of t pages with b
// pages of cache (PostgreSQL's index_pages_fetched).
func pagesFetched(n, t, b float64) float64 {
	t = math.Max(t, 1)
	if t <= b {
		return math.Min(2*t*n/(2*t+n), t)
	}
	lim := 2 * t * b / (2*t - b)
	if n <= lim {
		return 2 * t * n / (2*t + n)
	}
	return b + (n-lim)*(t-b)/t
}

// colStats returns statistics for a base table column, if analyzed.
func (o *Optimizer) colStats(id expr.ColumnID) (*catalog.Table, *catalog.ColumnStats) {
	if int(id) <= 0 || int(id) >= len(o.cols) {
		return nil, nil
	}
	ci := o.cols[id]
	if ci.TableOid == 0 {
		return nil, nil
	}
	t := o.Cat.Tables[ci.TableOid]
	if t == nil || ci.Attnum < 1 {
		return t, nil
	}
	if t.Stats == nil || int(ci.Attnum) > len(t.Stats.Columns) {
		return t, nil
	}
	return t, &t.Stats.Columns[ci.Attnum-1]
}

// tableRows estimates the number of rows in a table.
func (o *Optimizer) tableRows(t *catalog.Table) float64 {
	pages := 1
	if o.Pages != nil {
		pages = max(o.Pages(t), 1)
	}
	if t.Stats != nil && t.Stats.Pages > 0 {
		// Scale the analyzed density to the current size.
		density := t.Stats.Rows / float64(t.Stats.Pages)
		return math.Max(1, density*float64(pages))
	}
	if t.Stats != nil {
		return math.Max(1, t.Stats.Rows)
	}
	// Never analyzed: assume about 100 rows per 8 KiB page.
	if pages <= 1 {
		return 100
	}
	return float64(pages) * 100
}

func (o *Optimizer) tablePages(t *catalog.Table) float64 {
	if o.Pages != nil {
		return float64(max(o.Pages(t), 1))
	}
	return 1
}

// nDistinct estimates the number of distinct values of a column.
func (o *Optimizer) nDistinct(id expr.ColumnID) float64 {
	t, cs := o.colStats(id)
	if cs != nil && cs.NDistinct > 0 {
		return cs.NDistinct
	}
	if t != nil {
		rows := o.tableRows(t)
		ci := o.cols[id]
		for _, ix := range o.Cat.TableIndexes(t) {
			if ix.Unique && len(ix.Columns) == 1 && ix.Columns[0] == int(ci.Attnum)-1 {
				return rows
			}
		}
		return math.Min(defaultNDistinct, rows)
	}
	return defaultNDistinct
}

func typeWidth(t types.T) int {
	switch t.Kind() {
	case types.KBool:
		return 1
	case types.KInt:
		if t.Oid == types.OidInt8 {
			return 8
		}
		if t.Oid == types.OidInt2 {
			return 2
		}
		return 4
	case types.KFloat:
		if t.Oid == types.OidFloat4 {
			return 4
		}
		return 8
	case types.KDate:
		return 4
	case types.KTimestamp, types.KTimestampTZ, types.KNumeric:
		return 8
	case types.KInterval:
		return 16
	}
	if n := t.CharLen(); n > 0 {
		return n
	}
	return 32
}

func (o *Optimizer) width(layout []expr.ColumnID) int {
	w := 0
	for _, id := range layout {
		if int(id) > 0 && int(id) < len(o.cols) {
			if _, cs := o.colStats(id); cs != nil && cs.AvgWidth > 0 {
				w += cs.AvgWidth
				continue
			}
			w += typeWidth(o.cols[id].T)
		} else {
			w += 8
		}
	}
	return w
}

// isParamLike reports whether e can be evaluated before scanning the
// relation owning cols (a constant, parameter or outer reference).
func isParamLike(e expr.Expr, cols map[expr.ColumnID]bool) bool {
	for id := range expr.Columns(e) {
		if cols[id] {
			return false
		}
	}
	return !expr.IsVolatile(e)
}

// sel estimates the selectivity of a predicate.
func (o *Optimizer) sel(e expr.Expr) float64 {
	switch x := e.(type) {
	case *expr.Const:
		if x.V.IsNull() || (x.V.K == types.KBool && !x.V.Bool()) {
			return 0
		}
		return 1
	case *expr.And:
		return o.andSel(x.Args)
	case *expr.Or:
		s := 0.0
		for _, a := range x.Args {
			t := o.sel(a)
			s = s + t - s*t
		}
		return s
	case *expr.Not:
		return 1 - o.sel(x.Arg)
	case *expr.IsNull:
		if c, ok := x.Arg.(*expr.Col); ok {
			if _, cs := o.colStats(c.ID); cs != nil {
				if x.Not {
					return 1 - cs.NullFrac
				}
				return cs.NullFrac
			}
		}
		if x.Not {
			return 1 - defaultEqSel
		}
		return defaultEqSel
	case *expr.InList:
		if c, ok := x.Arg.(*expr.Col); ok {
			s := 0.0
			for _, v := range x.List {
				s += o.eqSel(c, v)
			}
			if x.Not {
				return math.Max(0, 1-s)
			}
			return math.Min(1, s)
		}
		return math.Min(1, defaultEqSel*float64(len(x.List)))
	case *expr.Col:
		return 0.5
	case *expr.Call:
		if len(x.Args) == 2 && expr.IsComparison(x.Fn.Op) {
			l, r := x.Args[0], x.Args[1]
			lc, lok := stripCast(l).(*expr.Col)
			rc, rok := stripCast(r).(*expr.Col)
			switch {
			case lok && rok:
				if x.Fn.Op == "=" {
					return 1 / math.Max(1, math.Max(o.nDistinct(lc.ID), o.nDistinct(rc.ID)))
				}
				if x.Fn.Op == "<>" {
					return 1 - defaultEqSel
				}
				return defaultRangeSel
			case lok:
				return o.compareSel(lc, x.Fn.Op, r)
			case rok:
				return o.compareSel(rc, expr.CommuteOp(x.Fn.Op), l)
			}
			if x.Fn.Op == "=" {
				return defaultEqSel
			}
			return defaultRangeSel
		}
		switch x.Fn.Op {
		case "~~", "~~*", "~", "~*":
			return 0.05
		case "!~~", "!~~*", "!~", "!~*":
			return 0.95
		}
	case *expr.Subquery:
		return 0.5
	}
	return defaultSel
}

// histPos places a value on a line for interpolation inside a histogram
// bucket.
func histPos(v types.Value) (float64, bool) {
	switch v.K {
	case types.KInt, types.KFloat, types.KNumeric:
		return v.AsFloat(), true
	case types.KDate, types.KTimestamp, types.KTimestampTZ:
		return float64(v.I), true
	}
	return 0, false
}

// andSel estimates a conjunction. Conditions are assumed independent,
// except that a lower and an upper bound on the same column (x >= a AND
// x < b) are combined into the fraction between them, as PostgreSQL's
// clauselist_selectivity does; multiplying them would count the rows
// below a and above b as both passing.
func (o *Optimizer) andSel(args []expr.Expr) float64 {
	type bounds struct {
		lo, hi       float64
		hasLo, hasHi bool
		nonNull      float64
	}
	var ranges map[expr.ColumnID]*bounds
	var order []expr.ColumnID
	s := 1.0
	for _, a := range args {
		c, lower, sel, ok := o.rangeBound(a)
		if !ok {
			s *= o.sel(a)
			continue
		}
		if ranges == nil {
			ranges = map[expr.ColumnID]*bounds{}
		}
		b := ranges[c.ID]
		if b == nil {
			b = &bounds{nonNull: 1}
			if _, cs := o.colStats(c.ID); cs != nil {
				b.nonNull = 1 - cs.NullFrac
			}
			ranges[c.ID] = b
			order = append(order, c.ID)
		}
		if lower {
			if !b.hasLo || sel < b.lo {
				b.lo = sel
			}
			b.hasLo = true
		} else {
			if !b.hasHi || sel < b.hi {
				b.hi = sel
			}
			b.hasHi = true
		}
	}
	for _, id := range order {
		b := ranges[id]
		switch {
		case b.hasLo && b.hasHi:
			s *= math.Max(0.0001, b.lo+b.hi-b.nonNull)
		case b.hasLo:
			s *= b.lo
		default:
			s *= b.hi
		}
	}
	return s
}

// rangeBound recognizes col < const, col >= const and the like when the
// column has a histogram, returning whether it is a lower bound and its
// selectivity.
func (o *Optimizer) rangeBound(e expr.Expr) (*expr.Col, bool, float64, bool) {
	call, ok := e.(*expr.Call)
	if !ok || len(call.Args) != 2 || !expr.IsComparison(call.Fn.Op) {
		return nil, false, 0, false
	}
	op := call.Fn.Op
	c, isCol := stripCast(call.Args[0]).(*expr.Col)
	v := call.Args[1]
	if !isCol {
		c, isCol = stripCast(call.Args[1]).(*expr.Col)
		v = call.Args[0]
		op = expr.CommuteOp(op)
	}
	k, isConst := v.(*expr.Const)
	if !isCol || !isConst || k.V.IsNull() {
		return nil, false, 0, false
	}
	var lower bool
	switch op {
	case ">", ">=":
		lower = true
	case "<", "<=":
	default:
		return nil, false, 0, false
	}
	if _, cs := o.colStats(c.ID); cs == nil || len(cs.HistVals) < 2 || cs.HistVals[0].K != k.V.K {
		return nil, false, 0, false
	}
	return c, lower, o.compareSel(c, op, v), true
}

func stripCast(e expr.Expr) expr.Expr {
	if c, ok := e.(*expr.Cast); ok {
		return c.Arg
	}
	return e
}

func (o *Optimizer) eqSel(c *expr.Col, v expr.Expr) float64 {
	_, cs := o.colStats(c.ID)
	k, isConst := v.(*expr.Const)
	if isConst && k.V.IsNull() {
		return 0
	}
	if cs == nil {
		return 1 / math.Max(1, o.nDistinct(c.ID))
	}
	if isConst {
		mcvSum := 0.0
		for i, m := range cs.MCVVals {
			mcvSum += cs.MCVFreq[i]
			if types.Compare(m, k.V) == 0 && m.K == k.V.K {
				return cs.MCVFreq[i]
			}
		}
		rest := cs.NDistinct - float64(len(cs.MCVVals))
		if rest < 1 {
			return math.Min(defaultEqSel, 1-mcvSum)
		}
		return math.Max(0, (1-cs.NullFrac-mcvSum)/rest)
	}
	return (1 - cs.NullFrac) / math.Max(1, cs.NDistinct)
}

// compareSel estimates col op value.
func (o *Optimizer) compareSel(c *expr.Col, op string, v expr.Expr) float64 {
	switch op {
	case "=":
		return o.eqSel(c, v)
	case "<>":
		return 1 - o.eqSel(c, v)
	}
	_, cs := o.colStats(c.ID)
	k, isConst := v.(*expr.Const)
	if cs == nil || !isConst || k.V.IsNull() || len(cs.HistVals) < 2 {
		return defaultRangeSel
	}
	// Fraction of the histogram below the value.
	h := cs.HistVals
	if k.V.K != h[0].K {
		return defaultRangeSel
	}
	i := sort.Search(len(h), func(i int) bool { return types.Compare(h[i], k.V) >= 0 })
	var frac float64
	switch {
	case i == 0:
		frac = 0
	case i >= len(h):
		frac = 1
	default:
		// Linear interpolation inside the bucket for numeric values.
		within := 0.5
		a, aok := histPos(h[i-1])
		b, bok := histPos(h[i])
		x, xok := histPos(k.V)
		if aok && bok && xok && b > a {
			within = (x - a) / (b - a)
		}
		frac = (float64(i-1) + within) / float64(len(h)-1)
	}
	nonNull := 1 - cs.NullFrac
	switch op {
	case "<", "<=":
		return math.Max(0.0001, frac*nonNull)
	case ">", ">=":
		return math.Max(0.0001, (1-frac)*nonNull)
	}
	return defaultRangeSel
}

func log2(n float64) float64 {
	if n < 2 {
		return 1
	}
	return math.Log2(n)
}

func sortCost(rows float64) float64 { return 2 * cpuOperatorCost * rows * log2(rows) }
