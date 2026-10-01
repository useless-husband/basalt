package executor

import (
	"bytes"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/planner"
	"github.com/useless-husband/basalt/internal/storage"
	"github.com/useless-husband/basalt/internal/types"
)

// tableReader turns visible heap tuples into rows of a scan's layout.
type tableReader struct {
	c       *Ctx
	t       *catalog.Table
	withTID bool
	filter  expr.Fn
	ec      *expr.Ctx
	stats   *planner.NodeStats
}

func (r *tableReader) row(tid storage.TID, tuple []byte) (Row, bool, error) {
	h := storage.DecodeHeader(tuple)
	if !r.c.Txn.Visible(h) {
		return nil, false, nil
	}
	vals, err := decodeTuple(r.t, tuple)
	if err != nil {
		return nil, false, err
	}
	if n := storedColumns(tuple); n < len(r.t.Columns) {
		fillMissing(r.t, vals, n)
	}
	if r.withTID {
		vals = append(vals, types.NewInt(int64(tid)))
	}
	if r.filter != nil {
		v, err := r.filter(r.ec, vals)
		if err != nil {
			return nil, false, err
		}
		if !expr.Truth(v) {
			if r.stats != nil {
				r.stats.Removed++
			}
			return nil, false, nil
		}
	}
	return vals, true, nil
}

type seqScan struct {
	tableReader
	heap *storage.Heap
	cur  *storage.HeapCursor
	n    int
}

func (c *Ctx) newSeqScan(p *planner.SeqScanP) (Iter, error) {
	f, err := compile(p.Filter, layoutOf(p.Layout()))
	if err != nil {
		return nil, err
	}
	s := &seqScan{tableReader: tableReader{c: c, t: p.Table, withTID: p.TIDCol != 0, filter: f}, heap: c.Store.Heap(p.Table.Heap)}
	if c.Analyze {
		s.stats = planner.Stats(p)
	}
	return s, nil
}

func (s *seqScan) Open(ec *expr.Ctx) error {
	s.ec = ec
	s.cur = s.heap.Scan()
	s.n = 0
	return nil
}

func (s *seqScan) Next() (Row, error) {
	for {
		tid, tuple, ok, err := s.cur.Next()
		if err != nil || !ok {
			return nil, err
		}
		if s.n++; s.n%1024 == 0 {
			if err := s.c.check(); err != nil {
				return nil, err
			}
		}
		r, keep, err := s.row(tid, tuple)
		if err != nil {
			return nil, err
		}
		if keep {
			return r, nil
		}
	}
}

func (s *seqScan) Close() {}

type indexScan struct {
	tableReader
	p      *planner.IndexScanP
	heap   *storage.Heap
	tree   *storage.BTree
	eq     []expr.Fn
	lo, hi expr.Fn
	cur    *storage.Cursor
	prefix []byte
	loKey  []byte // skip keys with this prefix when the lower bound is exclusive
	hiKey  []byte
	stop   []byte // stop at keys >= stop
	done   bool
	buf    []byte
}

func (c *Ctx) newIndexScan(p *planner.IndexScanP) (Iter, error) {
	l := layoutOf(p.Layout())
	f, err := compile(p.Filter, l)
	if err != nil {
		return nil, err
	}
	s := &indexScan{tableReader: tableReader{c: c, t: p.Table, withTID: p.TIDCol != 0, filter: f}, p: p,
		heap: c.Store.Heap(p.Table.Heap), tree: c.Store.BTree(p.Index.Root)}
	if c.Analyze {
		s.stats = planner.Stats(p)
	}
	// Bound expressions do not read the scanned row.
	if s.eq, err = compileAll(p.Eq, nil); err != nil {
		return nil, err
	}
	if s.lo, err = compile(p.Lo, nil); err != nil {
		return nil, err
	}
	if s.hi, err = compile(p.Hi, nil); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *indexScan) Open(ec *expr.Ctx) error {
	s.ec = ec
	s.done = false
	s.prefix = s.prefix[:0]
	s.loKey, s.hiKey, s.stop = nil, nil, nil
	for _, f := range s.eq {
		v, err := f(ec, nil)
		if err != nil {
			return err
		}
		if v.IsNull() {
			s.done = true // col = NULL matches nothing
			return nil
		}
		s.prefix = types.EncodeKey(s.prefix, v)
	}
	start := append([]byte(nil), s.prefix...)
	if s.lo != nil {
		v, err := s.lo(ec, nil)
		if err != nil {
			return err
		}
		if v.IsNull() {
			s.done = true
			return nil
		}
		start = types.EncodeKey(start, v)
		if !s.p.LoIncl {
			s.loKey = append([]byte(nil), start...)
		}
	}
	if s.hi != nil {
		v, err := s.hi(ec, nil)
		if err != nil {
			return err
		}
		if v.IsNull() {
			s.done = true
			return nil
		}
		s.hiKey = types.EncodeKey(append([]byte(nil), s.prefix...), v)
	} else if s.lo != nil {
		// A range without an upper bound stops before the NULLs, which
		// sort last.
		s.stop = append(append([]byte(nil), s.prefix...), 0x02)
	}
	s.cur = s.tree.Seek(start)
	return nil
}

func (s *indexScan) Next() (Row, error) {
	if s.done {
		return nil, nil
	}
	for {
		k, ok, err := s.cur.Next()
		if err != nil {
			return nil, err
		}
		if !ok || !bytes.HasPrefix(k, s.prefix) {
			s.done = true
			return nil, nil
		}
		if s.loKey != nil && bytes.HasPrefix(k, s.loKey) {
			continue
		}
		if s.hiKey != nil {
			if s.p.HiIncl {
				if bytes.Compare(k, s.hiKey) > 0 && !bytes.HasPrefix(k, s.hiKey) {
					s.done = true
					return nil, nil
				}
			} else if bytes.Compare(k, s.hiKey) >= 0 {
				s.done = true
				return nil, nil
			}
		}
		if s.stop != nil && bytes.Compare(k, s.stop) >= 0 {
			s.done = true
			return nil, nil
		}
		tid := tidFromKey(k)
		tuple, found, err := s.heap.Fetch(tid, s.buf)
		if err != nil {
			return nil, err
		}
		if !found {
			continue // removed by VACUUM after the key was read
		}
		s.buf = tuple
		r, keep, err := s.row(tid, tuple)
		if err != nil {
			return nil, err
		}
		if keep {
			return r, nil
		}
	}
}

func (s *indexScan) Close() {}

type vscan struct {
	c      *Ctx
	p      *planner.VScanP
	filter expr.Fn
	rows   [][]types.Value
	i      int
	ec     *expr.Ctx
}

func (c *Ctx) newVScan(p *planner.VScanP) (Iter, error) {
	f, err := compile(p.Filter, layoutOf(p.ColIDs))
	if err != nil {
		return nil, err
	}
	return &vscan{c: c, p: p, filter: f}, nil
}

func (s *vscan) Open(ec *expr.Ctx) error {
	s.ec = ec
	s.i = 0
	if s.rows == nil {
		rows, err := s.p.VT.Rows()
		if err != nil {
			return err
		}
		s.rows = rows
	}
	return nil
}

func (s *vscan) Next() (Row, error) {
	for s.i < len(s.rows) {
		r := s.rows[s.i]
		s.i++
		if s.filter != nil {
			v, err := s.filter(s.ec, r)
			if err != nil {
				return nil, err
			}
			if !expr.Truth(v) {
				continue
			}
		}
		return r, nil
	}
	return nil, nil
}

func (s *vscan) Close() {}

type values struct {
	rows [][]expr.Fn
	i    int
	ec   *expr.Ctx
}

func (c *Ctx) newValues(p *planner.ValuesP) (Iter, error) {
	v := &values{}
	for _, r := range p.Rows {
		fs, err := compileAll(r, nil)
		if err != nil {
			return nil, err
		}
		v.rows = append(v.rows, fs)
	}
	return v, nil
}

func (v *values) Open(ec *expr.Ctx) error { v.ec, v.i = ec, 0; return nil }

func (v *values) Next() (Row, error) {
	if v.i >= len(v.rows) {
		return nil, nil
	}
	fs := v.rows[v.i]
	v.i++
	out := make(Row, len(fs))
	for j, f := range fs {
		val, err := f(v.ec, nil)
		if err != nil {
			return nil, err
		}
		out[j] = val
	}
	return out, nil
}

func (v *values) Close() {}

type funcScan struct {
	c    *Ctx
	p    *planner.FuncScanP
	args []expr.Fn
	ec   *expr.Ctx
	// generate_series state
	cur, stop, step types.Value
	iv              types.Interval
	arr             []types.Value
	i               int
	done            bool
}

func (c *Ctx) newFuncScan(p *planner.FuncScanP) (Iter, error) {
	args, err := compileAll(p.Args, nil)
	if err != nil {
		return nil, err
	}
	return &funcScan{c: c, p: p, args: args}, nil
}

func (f *funcScan) Open(ec *expr.Ctx) error {
	f.ec = ec
	f.done = false
	vals := make([]types.Value, len(f.args))
	for i, a := range f.args {
		v, err := a(ec, nil)
		if err != nil {
			return err
		}
		vals[i] = v
	}
	for _, v := range vals {
		if v.IsNull() {
			f.done = true
			return nil
		}
	}
	switch f.p.Name {
	case "generate_series":
		f.cur, f.stop = vals[0], vals[1]
		if f.p.T.Kind() == types.KInt || f.p.T.Kind() == types.KNumeric {
			f.step = types.NewInt(1)
			if f.p.T.Kind() == types.KNumeric {
				f.step = types.NewNumeric(types.DecimalFromInt(1))
			}
			if len(vals) == 3 {
				f.step = vals[2]
			}
			if (f.step.K == types.KInt && f.step.I == 0) || (f.step.K == types.KNumeric && f.step.Num().Sign() == 0) {
				return pgerr.New(pgerr.InvalidParameterValue, "step size cannot equal zero")
			}
		} else {
			f.iv = vals[2].Interval()
			if f.iv.Months == 0 && f.iv.Days == 0 && f.iv.Micros == 0 {
				return pgerr.New(pgerr.InvalidParameterValue, "step size cannot equal zero")
			}
		}
	case "unnest":
		f.arr = vals[0].Arr().Vals
		f.i = 0
	}
	return nil
}

func (f *funcScan) Next() (Row, error) {
	if f.done {
		return nil, nil
	}
	switch f.p.Name {
	case "unnest":
		if f.i >= len(f.arr) {
			return nil, nil
		}
		v := f.arr[f.i]
		f.i++
		return Row{v}, nil
	}
	switch f.p.T.Kind() {
	case types.KInt:
		step := f.step.I
		if (step > 0 && f.cur.I > f.stop.I) || (step < 0 && f.cur.I < f.stop.I) {
			return nil, nil
		}
		v := f.cur
		next := f.cur.I + step
		if (step > 0 && next < f.cur.I) || (step < 0 && next > f.cur.I) {
			f.done = true // overflow: this was the last value
		}
		f.cur = types.NewInt(next)
		if f.c != nil {
			if err := f.c.check(); err != nil {
				return nil, err
			}
		}
		return Row{v}, nil
	case types.KNumeric:
		step := f.step.Num()
		c := f.cur.Num().Cmp(f.stop.Num())
		if (step.Sign() > 0 && c > 0) || (step.Sign() < 0 && c < 0) {
			return nil, nil
		}
		v := f.cur
		f.cur = types.NewNumeric(f.cur.Num().Add(step))
		return Row{v}, nil
	default:
		neg := f.iv.Months < 0 || f.iv.Days < 0 || f.iv.Micros < 0
		if (!neg && f.cur.I > f.stop.I) || (neg && f.cur.I < f.stop.I) {
			return nil, nil
		}
		v := f.cur
		f.cur = types.Value{K: f.cur.K, I: types.AddIntervalToTimestamp(f.cur.I, f.iv)}
		return Row{v}, nil
	}
}

func (f *funcScan) Close() {}
