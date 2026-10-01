package executor

import (
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/planner"
	"github.com/useless-husband/basalt/internal/types"
)

func nulls(n int) Row { return make(Row, n) }

func concat(a, b Row) Row {
	out := make(Row, 0, len(a)+len(b))
	return append(append(out, a...), b...)
}

// ---- nested loop ----

type nestLoop struct {
	c          *Ctx
	kind       planner.JoinKind
	outer      Iter
	inner      Iter
	cond       expr.Fn
	paramIDs   []expr.ColumnID
	paramPos   []int
	innerWidth int
	outerWidth int
	ec         *expr.Ctx
	ictx       *expr.Ctx

	cur       Row
	matched   bool
	innerOpen bool
	// FULL JOIN: the inner side materialized with match flags.
	innerRows    []Row
	innerMatched []bool
	ii           int
	fullTail     int
	outerDone    bool
}

func (c *Ctx) newNestLoop(p *planner.NestLoopP) (Iter, error) {
	outer, err := c.Build(p.Outer)
	if err != nil {
		return nil, err
	}
	inner, err := c.Build(p.Inner)
	if err != nil {
		return nil, err
	}
	n := &nestLoop{c: c, kind: p.Kind, outer: outer, inner: inner,
		innerWidth: len(p.Inner.Layout()), outerWidth: len(p.Outer.Layout())}
	layout := append(append([]expr.ColumnID(nil), p.Outer.Layout()...), p.Inner.Layout()...)
	if n.cond, err = compile(p.Cond, layoutOf(layout)); err != nil {
		return nil, err
	}
	// The inner side may read outer columns (parameterized scans,
	// correlated filters): pass them as outer bindings.
	used := planner.UsedColumns(p.Inner)
	for i, id := range p.Outer.Layout() {
		if used[id] {
			n.paramIDs = append(n.paramIDs, id)
			n.paramPos = append(n.paramPos, i)
		}
	}
	return n, nil
}

func (n *nestLoop) Open(ec *expr.Ctx) error {
	n.ec = ec
	n.cur = nil
	n.innerOpen = false
	n.outerDone = false
	n.innerRows, n.innerMatched = nil, nil
	n.fullTail = -1
	if n.kind == planner.JoinFull {
		rows, err := Drain(n.inner, ec)
		if err != nil {
			return err
		}
		n.innerRows = rows
		n.innerMatched = make([]bool, len(rows))
	}
	return n.outer.Open(ec)
}

func (n *nestLoop) innerCtx() *expr.Ctx {
	if len(n.paramIDs) == 0 {
		return n.ec
	}
	m := make(map[expr.ColumnID]types.Value, len(n.ec.Outer)+len(n.paramIDs))
	for k, v := range n.ec.Outer {
		m[k] = v
	}
	for i, id := range n.paramIDs {
		m[id] = n.cur[n.paramPos[i]]
	}
	return n.ec.WithOuter(m)
}

func (n *nestLoop) nextInner() (Row, error) {
	if n.innerRows != nil || n.kind == planner.JoinFull {
		if n.ii >= len(n.innerRows) {
			return nil, nil
		}
		r := n.innerRows[n.ii]
		n.ii++
		return r, nil
	}
	return n.inner.Next()
}

func (n *nestLoop) Next() (Row, error) {
	for {
		if n.cur == nil {
			if n.outerDone {
				return n.fullRest()
			}
			r, err := n.outer.Next()
			if err != nil {
				return nil, err
			}
			if r == nil {
				n.outerDone = true
				continue
			}
			if err := n.c.check(); err != nil {
				return nil, err
			}
			n.cur = r
			n.matched = false
			n.ictx = n.innerCtx()
			if n.kind == planner.JoinFull {
				n.ii = 0
			} else if err := n.inner.Open(n.ictx); err != nil {
				return nil, err
			}
		}
		ir, err := n.nextInner()
		if err != nil {
			return nil, err
		}
		if ir == nil {
			cur := n.cur
			n.cur = nil
			if !n.matched {
				switch n.kind {
				case planner.JoinLeft, planner.JoinFull:
					return concat(cur, nulls(n.innerWidth)), nil
				case planner.JoinAnti:
					return cur, nil
				}
			}
			continue
		}
		combined := concat(n.cur, ir)
		if n.cond != nil {
			v, err := n.cond(n.ictx, combined)
			if err != nil {
				return nil, err
			}
			if !expr.Truth(v) {
				continue
			}
		}
		n.matched = true
		if n.kind == planner.JoinFull {
			n.innerMatched[n.ii-1] = true
		}
		switch n.kind {
		case planner.JoinSemi:
			cur := n.cur
			n.cur = nil
			return cur, nil
		case planner.JoinAnti:
			n.cur = nil
			continue
		}
		return combined, nil
	}
}

// fullRest emits the unmatched inner rows of a FULL JOIN.
func (n *nestLoop) fullRest() (Row, error) {
	if n.kind != planner.JoinFull {
		return nil, nil
	}
	for n.fullTail+1 < len(n.innerRows) {
		n.fullTail++
		if !n.innerMatched[n.fullTail] {
			return concat(nulls(n.outerWidth), n.innerRows[n.fullTail]), nil
		}
	}
	return nil, nil
}

func (n *nestLoop) Close() { n.outer.Close(); n.inner.Close() }

// ---- hash join ----

type hashEntry struct {
	row     Row
	matched bool
}

type hashJoin struct {
	c          *Ctx
	kind       planner.JoinKind
	outer      Iter
	inner      Iter
	okeys      []expr.Fn
	ikeys      []expr.Fn
	residual   expr.Fn
	innerWidth int
	outerWidth int
	ec         *expr.Ctx

	table   map[string][]*hashEntry
	all     []*hashEntry // FULL JOIN: every build row, for the tail
	cur     Row
	cands   []*hashEntry
	ci      int
	matched bool
	done    bool
	tail    int
	keyBuf  []byte
}

func (c *Ctx) newHashJoin(p *planner.HashJoinP) (Iter, error) {
	outer, err := c.Build(p.Outer)
	if err != nil {
		return nil, err
	}
	inner, err := c.Build(p.Inner)
	if err != nil {
		return nil, err
	}
	h := &hashJoin{c: c, kind: p.Kind, outer: outer, inner: inner,
		innerWidth: len(p.Inner.Layout()), outerWidth: len(p.Outer.Layout())}
	if h.okeys, err = compileAll(p.OuterKeys, layoutOf(p.Outer.Layout())); err != nil {
		return nil, err
	}
	if h.ikeys, err = compileAll(p.InnerKeys, layoutOf(p.Inner.Layout())); err != nil {
		return nil, err
	}
	layout := append(append([]expr.ColumnID(nil), p.Outer.Layout()...), p.Inner.Layout()...)
	if h.residual, err = compile(p.Residual, layoutOf(layout)); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *hashJoin) key(fs []expr.Fn, r Row) ([]byte, bool, error) {
	h.keyBuf = h.keyBuf[:0]
	for _, f := range fs {
		v, err := f(h.ec, r)
		if err != nil {
			return nil, false, err
		}
		if v.IsNull() {
			return nil, false, nil
		}
		h.keyBuf = types.EncodeKey(h.keyBuf, v)
	}
	return h.keyBuf, true, nil
}

func (h *hashJoin) Open(ec *expr.Ctx) error {
	h.ec = ec
	h.table = map[string][]*hashEntry{}
	h.all = nil
	h.cur = nil
	h.done = false
	h.tail = -1
	if err := h.inner.Open(ec); err != nil {
		return err
	}
	n := 0
	for {
		r, err := h.inner.Next()
		if err != nil {
			return err
		}
		if r == nil {
			break
		}
		if n++; n%4096 == 0 {
			if err := h.c.check(); err != nil {
				return err
			}
		}
		e := &hashEntry{row: append(Row(nil), r...)}
		if h.kind == planner.JoinFull {
			h.all = append(h.all, e)
		}
		k, ok, err := h.key(h.ikeys, r)
		if err != nil {
			return err
		}
		if !ok {
			continue // a NULL key never matches
		}
		h.table[string(k)] = append(h.table[string(k)], e)
	}
	return h.outer.Open(ec)
}

func (h *hashJoin) Next() (Row, error) {
	for {
		if h.done {
			return h.fullRest()
		}
		if h.cur == nil {
			r, err := h.outer.Next()
			if err != nil {
				return nil, err
			}
			if r == nil {
				h.done = true
				continue
			}
			h.cur = r
			h.matched = false
			h.ci = 0
			k, ok, err := h.key(h.okeys, r)
			if err != nil {
				return nil, err
			}
			h.cands = nil
			if ok {
				h.cands = h.table[string(k)]
			}
		}
		for h.ci < len(h.cands) {
			e := h.cands[h.ci]
			h.ci++
			combined := concat(h.cur, e.row)
			if h.residual != nil {
				v, err := h.residual(h.ec, combined)
				if err != nil {
					return nil, err
				}
				if !expr.Truth(v) {
					continue
				}
			}
			h.matched = true
			e.matched = true
			switch h.kind {
			case planner.JoinSemi:
				cur := h.cur
				h.cur = nil
				return cur, nil
			case planner.JoinAnti:
				h.ci = len(h.cands)
				continue
			}
			return combined, nil
		}
		cur := h.cur
		h.cur = nil
		if !h.matched {
			switch h.kind {
			case planner.JoinLeft, planner.JoinFull:
				return concat(cur, nulls(h.innerWidth)), nil
			case planner.JoinAnti:
				return cur, nil
			}
		}
	}
}

func (h *hashJoin) fullRest() (Row, error) {
	if h.kind != planner.JoinFull {
		return nil, nil
	}
	for h.tail+1 < len(h.all) {
		h.tail++
		if e := h.all[h.tail]; !e.matched {
			return concat(nulls(h.outerWidth), e.row), nil
		}
	}
	return nil, nil
}

func (h *hashJoin) Close() { h.outer.Close(); h.inner.Close(); h.table = nil }

// ---- merge join ----

type mergeJoin struct {
	c          *Ctx
	kind       planner.JoinKind
	outer      Iter
	inner      Iter
	okeys      []expr.Fn
	ikeys      []expr.Fn
	residual   expr.Fn
	innerWidth int
	ec         *expr.Ctx

	oRow, iRow Row
	oKey, iKey []types.Value
	iDone      bool
	group      []Row
	groupKey   []types.Value
	gi         int
	matched    bool
	inGroup    bool
}

func (c *Ctx) newMergeJoin(p *planner.MergeJoinP) (Iter, error) {
	outer, err := c.Build(p.Outer)
	if err != nil {
		return nil, err
	}
	inner, err := c.Build(p.Inner)
	if err != nil {
		return nil, err
	}
	m := &mergeJoin{c: c, kind: p.Kind, outer: outer, inner: inner, innerWidth: len(p.Inner.Layout())}
	if m.okeys, err = compileAll(p.OuterKeys, layoutOf(p.Outer.Layout())); err != nil {
		return nil, err
	}
	if m.ikeys, err = compileAll(p.InnerKeys, layoutOf(p.Inner.Layout())); err != nil {
		return nil, err
	}
	layout := append(append([]expr.ColumnID(nil), p.Outer.Layout()...), p.Inner.Layout()...)
	if m.residual, err = compile(p.Residual, layoutOf(layout)); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *mergeJoin) keys(fs []expr.Fn, r Row) ([]types.Value, error) {
	out := make([]types.Value, len(fs))
	for i, f := range fs {
		v, err := f(m.ec, r)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func hasNull(vs []types.Value) bool {
	for _, v := range vs {
		if v.IsNull() {
			return true
		}
	}
	return false
}

func cmpKeys(a, b []types.Value) int {
	for i := range a {
		if c := types.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return 0
}

func (m *mergeJoin) advanceOuter() error {
	r, err := m.outer.Next()
	if err != nil {
		return err
	}
	m.oRow = r
	if r != nil {
		m.oKey, err = m.keys(m.okeys, r)
	}
	return err
}

func (m *mergeJoin) advanceInner() error {
	r, err := m.inner.Next()
	if err != nil {
		return err
	}
	m.iRow = r
	if r == nil {
		m.iDone = true
		return nil
	}
	m.iKey, err = m.keys(m.ikeys, r)
	if err == nil && hasNull(m.iKey) {
		// NULLs sort last and match nothing: the inner side is finished.
		m.iDone = true
		m.iRow = nil
	}
	return err
}

func (m *mergeJoin) Open(ec *expr.Ctx) error {
	m.ec = ec
	m.iDone = false
	m.group = nil
	m.inGroup = false
	if err := m.inner.Open(ec); err != nil {
		return err
	}
	if err := m.outer.Open(ec); err != nil {
		return err
	}
	if err := m.advanceInner(); err != nil {
		return err
	}
	return m.advanceOuter()
}

func (m *mergeJoin) Next() (Row, error) {
	for {
		if m.oRow == nil {
			return nil, nil
		}
		if m.inGroup {
			for m.gi < len(m.group) {
				ir := m.group[m.gi]
				m.gi++
				combined := concat(m.oRow, ir)
				if m.residual != nil {
					v, err := m.residual(m.ec, combined)
					if err != nil {
						return nil, err
					}
					if !expr.Truth(v) {
						continue
					}
				}
				m.matched = true
				return combined, nil
			}
			// Done with this outer row.
			cur, matched := m.oRow, m.matched
			if err := m.advanceOuter(); err != nil {
				return nil, err
			}
			if m.oRow != nil && !hasNull(m.oKey) && cmpKeys(m.oKey, m.groupKey) == 0 {
				m.gi, m.matched = 0, false
			} else {
				m.inGroup = false
			}
			if !matched && m.kind == planner.JoinLeft {
				return concat(cur, nulls(m.innerWidth)), nil
			}
			continue
		}
		if hasNull(m.oKey) || m.iDone {
			cur := m.oRow
			if err := m.advanceOuter(); err != nil {
				return nil, err
			}
			if m.kind == planner.JoinLeft {
				return concat(cur, nulls(m.innerWidth)), nil
			}
			continue
		}
		c := cmpKeys(m.oKey, m.iKey)
		switch {
		case c < 0:
			cur := m.oRow
			if err := m.advanceOuter(); err != nil {
				return nil, err
			}
			if m.kind == planner.JoinLeft {
				return concat(cur, nulls(m.innerWidth)), nil
			}
		case c > 0:
			if err := m.advanceInner(); err != nil {
				return nil, err
			}
		default:
			// Collect the run of inner rows with this key.
			m.groupKey = m.iKey
			m.group = m.group[:0]
			for !m.iDone && cmpKeys(m.iKey, m.groupKey) == 0 {
				m.group = append(m.group, append(Row(nil), m.iRow...))
				if err := m.advanceInner(); err != nil {
					return nil, err
				}
			}
			m.inGroup = true
			m.gi, m.matched = 0, false
		}
	}
}

func (m *mergeJoin) Close() { m.outer.Close(); m.inner.Close() }
