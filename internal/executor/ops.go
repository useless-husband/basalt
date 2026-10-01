package executor

import (
	"container/heap"
	"sort"

	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/planner"
	"github.com/useless-husband/basalt/internal/sql"
	"github.com/useless-husband/basalt/internal/types"
)

type filter struct {
	in    Iter
	cond  expr.Fn
	ec    *expr.Ctx
	stats *planner.NodeStats
}

func (c *Ctx) newFilter(p *planner.FilterP) (Iter, error) {
	in, err := c.Build(p.Input)
	if err != nil {
		return nil, err
	}
	f, err := compile(p.Cond, layoutOf(p.Input.Layout()))
	if err != nil {
		return nil, err
	}
	fl := &filter{in: in, cond: f}
	if c.Analyze {
		fl.stats = planner.Stats(p)
	}
	return fl, nil
}

func (f *filter) Open(ec *expr.Ctx) error { f.ec = ec; return f.in.Open(ec) }

func (f *filter) Next() (Row, error) {
	for {
		r, err := f.in.Next()
		if err != nil || r == nil {
			return nil, err
		}
		v, err := f.cond(f.ec, r)
		if err != nil {
			return nil, err
		}
		if expr.Truth(v) {
			return r, nil
		}
		if f.stats != nil {
			f.stats.Removed++
		}
	}
}

func (f *filter) Close() { f.in.Close() }

type project struct {
	in    Iter
	exprs []expr.Fn
	ec    *expr.Ctx
}

func (c *Ctx) newProject(p *planner.ProjectP) (Iter, error) {
	in, err := c.Build(p.Input)
	if err != nil {
		return nil, err
	}
	fs, err := compileAll(p.Exprs, layoutOf(p.Input.Layout()))
	if err != nil {
		return nil, err
	}
	return &project{in: in, exprs: fs}, nil
}

func (p *project) Open(ec *expr.Ctx) error { p.ec = ec; return p.in.Open(ec) }

func (p *project) Next() (Row, error) {
	r, err := p.in.Next()
	if err != nil || r == nil {
		return nil, err
	}
	out := make(Row, len(p.exprs))
	for i, f := range p.exprs {
		v, err := f(p.ec, r)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (p *project) Close() { p.in.Close() }

// ---- sort ----

type sortKey struct {
	fn         expr.Fn
	desc       bool
	nullsFirst bool
}

func compileKeys(keys []expr.SortKey, l expr.Layout) ([]sortKey, error) {
	out := make([]sortKey, len(keys))
	for i, k := range keys {
		f, err := expr.Compile(k.E, l)
		if err != nil {
			return nil, err
		}
		out[i] = sortKey{f, k.Desc, k.NullsFirst}
	}
	return out, nil
}

func compareKeyVals(keys []sortKey, a, b []types.Value) int {
	for i, k := range keys {
		x, y := a[i], b[i]
		var c int
		switch {
		case x.IsNull() && y.IsNull():
			c = 0
		case x.IsNull():
			c = 1
			if k.nullsFirst {
				c = -1
			}
		case y.IsNull():
			c = -1
			if k.nullsFirst {
				c = 1
			}
		default:
			c = types.Compare(x, y)
			if k.desc {
				c = -c
			}
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

type sortedRow struct {
	keys []types.Value
	row  Row
}

type sorter struct {
	c    *Ctx
	in   Iter
	keys []sortKey
	topN expr.Fn
	rows []sortedRow
	i    int
	ec   *expr.Ctx
}

func (c *Ctx) newSort(p *planner.SortP) (Iter, error) {
	in, err := c.Build(p.Input)
	if err != nil {
		return nil, err
	}
	keys, err := compileKeys(p.Keys, layoutOf(p.Input.Layout()))
	if err != nil {
		return nil, err
	}
	topN, err := compile(p.TopN, nil)
	if err != nil {
		return nil, err
	}
	return &sorter{c: c, in: in, keys: keys, topN: topN}, nil
}

// topHeap keeps the N smallest rows (a max-heap on the sort order).
type topHeap struct {
	rows []sortedRow
	keys []sortKey
}

func (h *topHeap) Len() int { return len(h.rows) }
func (h *topHeap) Less(i, j int) bool {
	return compareKeyVals(h.keys, h.rows[i].keys, h.rows[j].keys) > 0
}
func (h *topHeap) Swap(i, j int) { h.rows[i], h.rows[j] = h.rows[j], h.rows[i] }
func (h *topHeap) Push(x any)    { h.rows = append(h.rows, x.(sortedRow)) }
func (h *topHeap) Pop() any {
	x := h.rows[len(h.rows)-1]
	h.rows = h.rows[:len(h.rows)-1]
	return x
}

func (s *sorter) Open(ec *expr.Ctx) error {
	s.ec = ec
	s.i = 0
	s.rows = s.rows[:0]
	limit := int64(-1)
	if s.topN != nil {
		v, err := s.topN(ec, nil)
		if err != nil {
			return err
		}
		if !v.IsNull() && v.I >= 0 {
			limit = v.I
		}
	}
	if err := s.in.Open(ec); err != nil {
		return err
	}
	var th *topHeap
	if limit >= 0 && limit < 1<<20 {
		th = &topHeap{keys: s.keys}
	}
	n := 0
	for {
		r, err := s.in.Next()
		if err != nil {
			return err
		}
		if r == nil {
			break
		}
		if n++; n%4096 == 0 {
			if err := s.c.check(); err != nil {
				return err
			}
		}
		kv := make([]types.Value, len(s.keys))
		for i, k := range s.keys {
			v, err := k.fn(ec, r)
			if err != nil {
				return err
			}
			kv[i] = v
		}
		sr := sortedRow{kv, append(Row(nil), r...)}
		if th != nil {
			if int64(th.Len()) < limit {
				heap.Push(th, sr)
			} else if limit > 0 && compareKeyVals(s.keys, sr.keys, th.rows[0].keys) < 0 {
				th.rows[0] = sr
				heap.Fix(th, 0)
			}
			continue
		}
		s.rows = append(s.rows, sr)
	}
	if th != nil {
		s.rows = th.rows
	}
	sort.SliceStable(s.rows, func(i, j int) bool { return compareKeyVals(s.keys, s.rows[i].keys, s.rows[j].keys) < 0 })
	return nil
}

func (s *sorter) Next() (Row, error) {
	if s.i >= len(s.rows) {
		return nil, nil
	}
	r := s.rows[s.i].row
	s.i++
	return r, nil
}

func (s *sorter) Close() { s.in.Close(); s.rows = nil }

// ---- limit ----

type limit struct {
	in            Iter
	limit, offset expr.Fn
	left, skip    int64
	unlimited     bool
}

func (c *Ctx) newLimit(p *planner.LimitP) (Iter, error) {
	in, err := c.Build(p.Input)
	if err != nil {
		return nil, err
	}
	l, err := compile(p.Limit, nil)
	if err != nil {
		return nil, err
	}
	o, err := compile(p.Offset, nil)
	if err != nil {
		return nil, err
	}
	return &limit{in: in, limit: l, offset: o}, nil
}

func (l *limit) Open(ec *expr.Ctx) error {
	l.unlimited = true
	l.skip = 0
	if l.limit != nil {
		v, err := l.limit(ec, nil)
		if err != nil {
			return err
		}
		if !v.IsNull() {
			if v.I < 0 {
				return pgerr.New(pgerr.InvalidRowCountInLimit, "LIMIT must not be negative")
			}
			l.unlimited = false
			l.left = v.I
		}
	}
	if l.offset != nil {
		v, err := l.offset(ec, nil)
		if err != nil {
			return err
		}
		if !v.IsNull() {
			if v.I < 0 {
				return pgerr.New(pgerr.InvalidRowCountInOffset, "OFFSET must not be negative")
			}
			l.skip = v.I
		}
	}
	if !l.unlimited && l.left == 0 {
		return nil // do not even open the input
	}
	return l.in.Open(ec)
}

func (l *limit) Next() (Row, error) {
	if !l.unlimited && l.left <= 0 {
		return nil, nil
	}
	for l.skip > 0 {
		r, err := l.in.Next()
		if err != nil || r == nil {
			return nil, err
		}
		l.skip--
	}
	r, err := l.in.Next()
	if err != nil || r == nil {
		return nil, err
	}
	l.left--
	return r, nil
}

func (l *limit) Close() { l.in.Close() }

// ---- distinct ----

type distinct struct {
	in   Iter
	on   []expr.Fn
	seen map[string]bool
	prev []types.Value
	ec   *expr.Ctx
	buf  []byte
}

func (c *Ctx) newDistinct(p *planner.DistinctP) (Iter, error) {
	in, err := c.Build(p.Input)
	if err != nil {
		return nil, err
	}
	on, err := compileAll(p.On, layoutOf(p.Input.Layout()))
	if err != nil {
		return nil, err
	}
	return &distinct{in: in, on: on}, nil
}

func (d *distinct) Open(ec *expr.Ctx) error {
	d.ec = ec
	d.seen = map[string]bool{}
	d.prev = nil
	return d.in.Open(ec)
}

func (d *distinct) Next() (Row, error) {
	for {
		r, err := d.in.Next()
		if err != nil || r == nil {
			return nil, err
		}
		if len(d.on) > 0 {
			kv := make([]types.Value, len(d.on))
			for i, f := range d.on {
				v, err := f(d.ec, r)
				if err != nil {
					return nil, err
				}
				kv[i] = v
			}
			if d.prev != nil && types.RowsEqual(d.prev, kv) {
				continue
			}
			d.prev = kv
			return r, nil
		}
		d.buf = types.HashKey(d.buf[:0], r)
		if d.seen[string(d.buf)] {
			continue
		}
		d.seen[string(d.buf)] = true
		return r, nil
	}
}

func (d *distinct) Close() { d.in.Close(); d.seen = nil }

// ---- set operations ----

type setOp struct {
	kind  int
	all   bool
	l, r  Iter
	ec    *expr.Ctx
	stage int
	seen  map[string]int
	buf   []byte
}

func (c *Ctx) newSetOp(p *planner.SetOpP) (Iter, error) {
	l, err := c.Build(p.Left)
	if err != nil {
		return nil, err
	}
	r, err := c.Build(p.Right)
	if err != nil {
		return nil, err
	}
	return &setOp{kind: p.Kind, all: p.All, l: l, r: r}, nil
}

func (s *setOp) Open(ec *expr.Ctx) error {
	s.ec = ec
	s.stage = 0
	s.seen = map[string]int{}
	if s.kind != int(sql.SetUnion) {
		// INTERSECT/EXCEPT: count the right side first.
		if err := s.r.Open(ec); err != nil {
			return err
		}
		for {
			r, err := s.r.Next()
			if err != nil {
				return err
			}
			if r == nil {
				break
			}
			s.buf = types.HashKey(s.buf[:0], r)
			s.seen[string(s.buf)]++
		}
		s.r.Close()
	}
	return s.l.Open(ec)
}

func (s *setOp) Next() (Row, error) {
	switch s.kind {
	case int(sql.SetUnion):
		for {
			var r Row
			var err error
			if s.stage == 0 {
				r, err = s.l.Next()
				if err != nil {
					return nil, err
				}
				if r == nil {
					s.stage = 1
					if err := s.r.Open(s.ec); err != nil {
						return nil, err
					}
					continue
				}
			} else {
				r, err = s.r.Next()
				if err != nil || r == nil {
					return nil, err
				}
			}
			if s.all {
				return r, nil
			}
			s.buf = types.HashKey(s.buf[:0], r)
			if s.seen[string(s.buf)] > 0 {
				continue
			}
			s.seen[string(s.buf)] = 1
			return r, nil
		}
	case int(sql.SetIntersect):
		for {
			r, err := s.l.Next()
			if err != nil || r == nil {
				return nil, err
			}
			s.buf = types.HashKey(s.buf[:0], r)
			k := string(s.buf)
			if s.seen[k] > 0 {
				if s.all {
					s.seen[k]--
				} else {
					s.seen[k] = 0
				}
				return r, nil
			}
		}
	default: // EXCEPT
		emitted := map[string]bool{}
		_ = emitted
		for {
			r, err := s.l.Next()
			if err != nil || r == nil {
				return nil, err
			}
			s.buf = types.HashKey(s.buf[:0], r)
			k := string(s.buf)
			if s.all {
				if s.seen[k] > 0 {
					s.seen[k]--
					continue
				}
				return r, nil
			}
			if s.seen[k] != 0 {
				continue
			}
			s.seen[k] = -1 // emit each distinct row once
			return r, nil
		}
	}
}

func (s *setOp) Close() { s.l.Close(); s.r.Close() }

// ---- materialize ----

type materialize struct {
	in     Iter
	rows   []Row
	loaded bool
	i      int
}

func (m *materialize) Open(ec *expr.Ctx) error {
	m.i = 0
	if m.loaded {
		return nil
	}
	rows, err := Drain(m.in, ec)
	if err != nil {
		return err
	}
	m.rows, m.loaded = rows, true
	return nil
}

func (m *materialize) Next() (Row, error) {
	if m.i >= len(m.rows) {
		return nil, nil
	}
	r := m.rows[m.i]
	m.i++
	return r, nil
}

func (m *materialize) Close() {}

// ---- aggregation ----

type aggSpec struct {
	call   *expr.AggCall
	args   []expr.Fn
	filter expr.Fn
	order  []sortKey
}

type aggGroup struct {
	keys     []types.Value
	states   []expr.AggState
	distinct []map[string]bool
	buffered [][]sortedRow // for aggregates with ORDER BY
}

type agg struct {
	c      *Ctx
	in     Iter
	groups []expr.Fn
	specs  []aggSpec
	ec     *expr.Ctx
	out    []*aggGroup
	i      int
}

func (c *Ctx) newAgg(p *planner.AggP) (Iter, error) {
	in, err := c.Build(p.Input)
	if err != nil {
		return nil, err
	}
	l := layoutOf(p.Input.Layout())
	a := &agg{c: c, in: in}
	if a.groups, err = compileAll(p.GroupBy, l); err != nil {
		return nil, err
	}
	for _, call := range p.Aggs {
		s := aggSpec{call: call}
		if s.args, err = compileAll(call.Args, l); err != nil {
			return nil, err
		}
		if s.filter, err = compile(call.Filter, l); err != nil {
			return nil, err
		}
		if s.order, err = compileKeys(call.OrderBy, l); err != nil {
			return nil, err
		}
		a.specs = append(a.specs, s)
	}
	return a, nil
}

func (a *agg) newGroup(keys []types.Value) *aggGroup {
	g := &aggGroup{keys: keys, states: make([]expr.AggState, len(a.specs)),
		distinct: make([]map[string]bool, len(a.specs)), buffered: make([][]sortedRow, len(a.specs))}
	for i, s := range a.specs {
		g.states[i] = s.call.Def.New(s.call.T)
		if s.call.Distinct {
			g.distinct[i] = map[string]bool{}
		}
	}
	return g
}

func (a *agg) Open(ec *expr.Ctx) error {
	a.ec = ec
	a.i = 0
	a.out = nil
	if err := a.in.Open(ec); err != nil {
		return err
	}
	index := map[string]*aggGroup{}
	var keyBuf []byte
	args := make([][]types.Value, len(a.specs))
	n := 0
	for {
		r, err := a.in.Next()
		if err != nil {
			return err
		}
		if r == nil {
			break
		}
		if n++; n%4096 == 0 {
			if err := a.c.check(); err != nil {
				return err
			}
		}
		keys := make([]types.Value, len(a.groups))
		for i, g := range a.groups {
			v, err := g(ec, r)
			if err != nil {
				return err
			}
			keys[i] = v
		}
		keyBuf = types.HashKey(keyBuf[:0], keys)
		g := index[string(keyBuf)]
		if g == nil {
			g = a.newGroup(keys)
			index[string(keyBuf)] = g
			a.out = append(a.out, g)
		}
		for i, s := range a.specs {
			if s.filter != nil {
				v, err := s.filter(ec, r)
				if err != nil {
					return err
				}
				if !expr.Truth(v) {
					continue
				}
			}
			vals := args[i][:0]
			skip := false
			for _, f := range s.args {
				v, err := f(ec, r)
				if err != nil {
					return err
				}
				if v.IsNull() && !s.call.Def.NullOK && !(s.call.Def.Name == "string_agg" && len(vals) > 0) {
					skip = true
				}
				vals = append(vals, v)
			}
			args[i] = vals
			if skip {
				continue
			}
			if g.distinct[i] != nil {
				k := string(types.HashKey(nil, vals))
				if g.distinct[i][k] {
					continue
				}
				g.distinct[i][k] = true
			}
			if len(s.order) > 0 {
				kv := make([]types.Value, len(s.order))
				for j, k := range s.order {
					v, err := k.fn(ec, r)
					if err != nil {
						return err
					}
					kv[j] = v
				}
				g.buffered[i] = append(g.buffered[i], sortedRow{kv, append(Row(nil), vals...)})
				continue
			}
			if err := g.states[i].Step(vals); err != nil {
				return err
			}
		}
	}
	if len(a.groups) == 0 && len(a.out) == 0 {
		a.out = append(a.out, a.newGroup(nil))
	}
	// Feed ordered aggregates.
	for _, g := range a.out {
		for i, s := range a.specs {
			if len(s.order) == 0 {
				continue
			}
			b := g.buffered[i]
			sort.SliceStable(b, func(x, y int) bool { return compareKeyVals(s.order, b[x].keys, b[y].keys) < 0 })
			for _, sr := range b {
				if err := g.states[i].Step(sr.row); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (a *agg) Next() (Row, error) {
	if a.i >= len(a.out) {
		return nil, nil
	}
	g := a.out[a.i]
	a.i++
	out := make(Row, 0, len(g.keys)+len(a.specs))
	out = append(out, g.keys...)
	for _, s := range g.states {
		v, err := s.Result()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (a *agg) Close() { a.in.Close(); a.out = nil }
