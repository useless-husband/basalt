// Package executor runs physical plans as a tree of pull-based (Volcano)
// iterators, and implements INSERT, UPDATE and DELETE with MVCC, row
// locking, unique, check and foreign key constraints.
package executor

import (
	"encoding/binary"
	"time"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/planner"
	"github.com/useless-husband/basalt/internal/storage"
	"github.com/useless-husband/basalt/internal/txn"
	"github.com/useless-husband/basalt/internal/types"
)

// Row is a tuple of values in an operator's layout.
type Row = []types.Value

// Iter is a pull-based operator. Open may be called again to restart it
// (for example the inner side of a nested loop) with a new context.
type Iter interface {
	Open(ec *expr.Ctx) error
	Next() (Row, error)
	Close()
}

// Ctx is the execution context of one statement.
type Ctx struct {
	Txn     *txn.Txn
	Store   *storage.Store
	Cat     *catalog.Catalog
	Eval    *expr.Ctx
	Analyze bool
	// Stats counters for the statement.
	RowsAffected int64
	subIters     map[int]Iter
	checks       map[uint32][]checkFn
	// TableChanged is called after rows of a table are modified.
	TableChanged func(oid uint32, inserted, deleted int64)
	// touched records the tuple changes of the statement so that a READ
	// COMMITTED statement can be undone and restarted.
	touched []touch
	// horizon caches OldestXmin for deadToAll during one statement.
	horizon uint64
}

type touch struct {
	heap   *storage.Heap
	tid    storage.TID
	locked bool // xmax set on an existing version (else: new version)
}

// RestartError tells the caller that a READ COMMITTED statement met a row
// changed by a transaction that committed after the statement's snapshot;
// the statement must be undone (Undo) and run again with a new snapshot.
type RestartError struct{ Err error }

func (e *RestartError) Error() string { return e.Err.Error() }

// Undo reverts the tuple changes made so far by the current statement:
// its new versions become invisible to everyone and the versions it
// locked are released.
func (c *Ctx) Undo() error {
	xid, cid := c.Txn.Xid, c.Txn.Cid
	for i := len(c.touched) - 1; i >= 0; i-- {
		t := c.touched[i]
		_, err := t.heap.ModifyHeader(t.tid, func(h *storage.TupleHeader) error {
			if t.locked {
				if h.Xmax == xid && h.Cmax == cid {
					h.Xmax, h.Cmax = 0, 0
					h.Flags &^= storage.FlagUpdated | storage.FlagLockOnly
				}
			} else if h.Xmin == xid && h.Cmin == cid {
				h.Flags |= storage.FlagKilled
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	c.touched = nil
	c.RowsAffected = 0
	return nil
}

type checkFn struct {
	name string
	fn   expr.Fn
}

// New returns an execution context and wires the subquery runner.
func New(t *txn.Txn, st *storage.Store, cat *catalog.Catalog, ec *expr.Ctx) *Ctx {
	c := &Ctx{Txn: t, Store: st, Cat: cat, Eval: ec, subIters: map[int]Iter{}}
	ec.Sub = c
	return c
}

func layoutOf(ids []expr.ColumnID) expr.Layout {
	l := make(expr.Layout, len(ids))
	for i, id := range ids {
		l[id] = i
	}
	return l
}

func compile(e expr.Expr, l expr.Layout) (expr.Fn, error) {
	if e == nil {
		return nil, nil
	}
	return expr.Compile(e, l)
}

func compileAll(es []expr.Expr, l expr.Layout) ([]expr.Fn, error) {
	out := make([]expr.Fn, len(es))
	for i, e := range es {
		f, err := expr.Compile(e, l)
		if err != nil {
			return nil, err
		}
		out[i] = f
	}
	return out, nil
}

// check reports a pending cancellation.
func (c *Ctx) check() error {
	if c.Eval.Check != nil {
		return c.Eval.Check()
	}
	return nil
}

// RunSubquery implements expr.SubqueryRunner.
func (c *Ctx) RunSubquery(ec *expr.Ctx, sq *expr.Subquery, outer map[expr.ColumnID]types.Value, limit int) ([][]types.Value, error) {
	it, ok := c.subIters[sq.ID]
	if !ok {
		p, isPlan := sq.Plan.(planner.Plan)
		if !isPlan {
			return nil, pgerr.Internal("subquery %d was not planned", sq.ID)
		}
		var err error
		if it, err = c.Build(p); err != nil {
			return nil, err
		}
		c.subIters[sq.ID] = it
	}
	sub := ec.WithOuter(outer)
	if err := it.Open(sub); err != nil {
		return nil, err
	}
	var rows [][]types.Value
	for limit <= 0 || len(rows) < limit {
		r, err := it.Next()
		if err != nil {
			return nil, err
		}
		if r == nil {
			break
		}
		rows = append(rows, append(Row(nil), r...))
	}
	return rows, nil
}

// Close releases cached subquery iterators.
func (c *Ctx) Close() {
	for _, it := range c.subIters {
		it.Close()
	}
}

// Build constructs the iterator tree for a plan.
func (c *Ctx) Build(p planner.Plan) (Iter, error) {
	it, err := c.build(p)
	if err != nil {
		return nil, err
	}
	if c.Analyze {
		return &instrumented{in: it, st: planner.Stats(p)}, nil
	}
	return it, nil
}

func (c *Ctx) build(p planner.Plan) (Iter, error) {
	switch x := p.(type) {
	case *planner.SeqScanP:
		return c.newSeqScan(x)
	case *planner.IndexScanP:
		return c.newIndexScan(x)
	case *planner.VScanP:
		return c.newVScan(x)
	case *planner.ValuesP:
		return c.newValues(x)
	case *planner.FuncScanP:
		return c.newFuncScan(x)
	case *planner.FilterP:
		return c.newFilter(x)
	case *planner.ProjectP:
		return c.newProject(x)
	case *planner.NestLoopP:
		return c.newNestLoop(x)
	case *planner.HashJoinP:
		return c.newHashJoin(x)
	case *planner.MergeJoinP:
		return c.newMergeJoin(x)
	case *planner.AggP:
		return c.newAgg(x)
	case *planner.SortP:
		return c.newSort(x)
	case *planner.LimitP:
		return c.newLimit(x)
	case *planner.DistinctP:
		return c.newDistinct(x)
	case *planner.SetOpP:
		return c.newSetOp(x)
	case *planner.MaterializeP:
		in, err := c.Build(x.Input)
		if err != nil {
			return nil, err
		}
		return &materialize{in: in}, nil
	case *planner.InsertP:
		return c.newInsert(x)
	case *planner.UpdateP:
		return c.newUpdate(x)
	case *planner.DeleteP:
		return c.newDelete(x)
	}
	return nil, pgerr.Internal("no executor for %T", p)
}

// instrumented counts rows and time for EXPLAIN ANALYZE.
type instrumented struct {
	in Iter
	st *planner.NodeStats
}

func (i *instrumented) Open(ec *expr.Ctx) error {
	start := time.Now()
	i.st.Loops++
	err := i.in.Open(ec)
	i.st.Time += time.Since(start)
	return err
}

func (i *instrumented) Next() (Row, error) {
	start := time.Now()
	r, err := i.in.Next()
	i.st.Time += time.Since(start)
	if r != nil {
		i.st.Rows++
	}
	return r, err
}

func (i *instrumented) Close() { i.in.Close() }

// Drain runs an iterator to completion and returns copies of its rows.
func Drain(it Iter, ec *expr.Ctx) ([]Row, error) {
	if err := it.Open(ec); err != nil {
		return nil, err
	}
	defer it.Close()
	var rows []Row
	for {
		r, err := it.Next()
		if err != nil {
			return nil, err
		}
		if r == nil {
			return rows, nil
		}
		rows = append(rows, append(Row(nil), r...))
	}
}

// ---- tuples ----

// decodeTuple decodes the row part of a heap tuple for table t into dst.
func decodeTuple(t *catalog.Table, tuple []byte) (Row, error) {
	vals, err := types.DecodeRow(tuple[storage.TupleHeaderSize:], len(t.Columns))
	if err != nil {
		return nil, pgerr.New(pgerr.DataCorrupted, "corrupt tuple in table %s: %v", t.Name, err)
	}
	if len(vals) > len(t.Columns) {
		vals = vals[:len(t.Columns)]
	}
	return vals, nil
}

// fillMissing supplies values for columns added after a row was written.
func fillMissing(t *catalog.Table, vals Row, stored int) {
	for i := stored; i < len(t.Columns) && i < len(vals); i++ {
		if c := t.Columns[i]; c.HasMissing {
			if v, err := types.Parse(c.Missing, c.Type); err == nil {
				vals[i] = v
			}
		}
	}
}

// storedColumns returns how many columns a tuple stores.
func storedColumns(tuple []byte) int {
	n, _ := binary.Uvarint(tuple[storage.TupleHeaderSize:])
	return int(n)
}

func encodeTuple(h storage.TupleHeader, vals Row) []byte {
	buf := make([]byte, storage.TupleHeaderSize, storage.TupleHeaderSize+16*len(vals))
	storage.EncodeHeader(buf, h)
	return types.EncodeRow(buf, vals)
}

// indexKey builds the B+tree key of a row for index ix (with the TID).
func indexKey(ix *catalog.Index, vals Row, tid storage.TID) []byte {
	var k []byte
	for _, c := range ix.Columns {
		k = types.EncodeKey(k, vals[c])
	}
	return appendTID(k, tid)
}

func appendTID(k []byte, tid storage.TID) []byte {
	v := uint64(tid)
	return append(k, byte(v>>56), byte(v>>48), byte(v>>40), byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func tidFromKey(k []byte) storage.TID {
	b := k[len(k)-8:]
	return storage.TID(uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7]))
}
