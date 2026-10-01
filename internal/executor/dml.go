package executor

import (
	"errors"
	"strings"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/planner"
	"github.com/useless-husband/basalt/internal/sql"
	"github.com/useless-husband/basalt/internal/storage"
	"github.com/useless-husband/basalt/internal/txn"
	"github.com/useless-husband/basalt/internal/types"
)

var (
	errWait = errors.New("wait")
	errDup  = errors.New("duplicate")
	errSkip = errors.New("skip")
)

// tableOps implements writes to one table: tuples, indexes and
// constraints.
type tableOps struct {
	c       *Ctx
	t       *catalog.Table
	heap    *storage.Heap
	indexes []*catalog.Index
	checks  []checkFn
	checkEc *expr.Ctx
}

func (c *Ctx) ops(t *catalog.Table) (*tableOps, error) {
	o := &tableOps{c: c, t: t, heap: c.Store.Heap(t.Heap), indexes: c.Cat.TableIndexes(t), checkEc: c.Eval}
	if fns, ok := c.checks[t.Oid]; ok {
		o.checks = fns
		return o, nil
	}
	for _, ch := range t.Checks {
		ast, err := sql.ParseExpr(ch.Expr)
		if err != nil {
			return nil, err
		}
		b := planner.NewBinder(c.Cat, nil, nil)
		e, ids, err := b.BindTableExpr(t, ast)
		if err != nil {
			return nil, err
		}
		f, err := expr.Compile(e, layoutOf(ids))
		if err != nil {
			return nil, err
		}
		o.checks = append(o.checks, checkFn{ch.Name, f})
	}
	if c.checks == nil {
		c.checks = map[uint32][]checkFn{}
	}
	c.checks[t.Oid] = o.checks
	return o, nil
}

func describeKey(t *catalog.Table, cols []int, vals Row) string {
	names := make([]string, len(cols))
	vs := make([]string, len(cols))
	for i, c := range cols {
		names[i] = t.Columns[c].Name
		if vals[c].IsNull() {
			vs[i] = "null"
		} else {
			vs[i] = types.ToText(vals[c], t.Columns[c].Type)
		}
	}
	return "(" + strings.Join(names, ", ") + ")=(" + strings.Join(vs, ", ") + ")"
}

// validate enforces NOT NULL and CHECK constraints.
func (o *tableOps) validate(vals Row) error {
	for i, col := range o.t.Columns {
		if col.NotNull && vals[i].IsNull() {
			e := pgerr.New(pgerr.NotNullViolation, "null value in column \"%s\" of relation \"%s\" violates not-null constraint", col.Name, o.t.Name)
			e.Table, e.Column = o.t.Name, col.Name
			return e
		}
	}
	for _, ch := range o.checks {
		v, err := ch.fn(o.checkEc, vals)
		if err != nil {
			return err
		}
		if !v.IsNull() && !v.Bool() {
			e := pgerr.New(pgerr.CheckViolation, "new row for relation \"%s\" violates check constraint \"%s\"", o.t.Name, ch.name)
			e.Table, e.Constraint = o.t.Name, ch.name
			return e
		}
	}
	return nil
}

func sameKey(ix *catalog.Index, a, b Row) bool {
	for _, c := range ix.Columns {
		if !types.Equal(a[c], b[c]) || a[c].K != b[c].K {
			return false
		}
	}
	return true
}

// deadToAll reports whether no snapshot, current or future, can see the
// tuple version (the condition under which VACUUM removes it).
func (c *Ctx) deadToAll(h storage.TupleHeader) bool {
	if h.Flags&storage.FlagKilled != 0 {
		return true
	}
	m := c.Txn.Manager()
	if m.Status(h.Xmin) == txn.Aborted {
		return true
	}
	if h.Xmax == 0 || h.Flags&storage.FlagLockOnly != 0 || m.Status(h.Xmax) != txn.Committed {
		return false
	}
	if c.horizon == 0 {
		c.horizon = m.OldestXmin()
	}
	return h.Xmax < c.horizon
}

// liveDirty classifies a tuple by the latest committed state (not by the
// snapshot): live, dead, or "wait for this transaction to finish".
func (c *Ctx) liveDirty(h storage.TupleHeader) (bool, uint64) {
	if h.Flags&storage.FlagKilled != 0 {
		return false, 0
	}
	me := c.Txn.Xid
	m := c.Txn.Manager()
	if h.Xmin != me || me == 0 {
		switch m.Status(h.Xmin) {
		case txn.Aborted:
			return false, 0
		case txn.InProgress:
			return false, h.Xmin
		}
	}
	if h.Xmax == 0 || h.Flags&storage.FlagLockOnly != 0 {
		return true, 0
	}
	if h.Xmax == me && me != 0 {
		return false, 0
	}
	switch m.Status(h.Xmax) {
	case txn.Committed:
		return false, 0
	case txn.Aborted:
		return true, 0
	}
	return true, h.Xmax
}

// insertTuple writes a new tuple version and its index entries. With
// onConflict set, a unique violation returns the TID of the conflicting
// live row instead of an error (and the new tuple is withdrawn).
func (o *tableOps) insertTuple(vals Row, onConflict bool) (storage.TID, storage.TID, bool, error) {
	return o.insertVersion(vals, nil, onConflict)
}

// insertVersion is insertTuple for an UPDATE's new version: old holds the
// previous version's values, and unique indexes whose key did not change
// skip the uniqueness check (the locked old version already owns the key,
// so no other live version can hold it).
func (o *tableOps) insertVersion(vals, old Row, onConflict bool) (storage.TID, storage.TID, bool, error) {
	xid, err := o.c.Txn.AssignXid()
	if err != nil {
		return 0, 0, false, err
	}
	tuple := encodeTuple(storage.TupleHeader{Xmin: xid, Cmin: o.c.Txn.Cid}, vals)
	tid, err := o.heap.Insert(tuple)
	if err != nil {
		return 0, 0, false, err
	}
	o.c.touched = append(o.c.touched, touch{heap: o.heap, tid: tid})
	for _, ix := range o.indexes {
		var conflict storage.TID
		var err error
		if old != nil && ix.Unique && sameKey(ix, old, vals) {
			err = o.c.Store.BTree(ix.Root).Insert(indexKey(ix, vals, tid), 0, nil)
		} else {
			conflict, err = o.insertIndex(ix, vals, tid)
		}
		if err != nil {
			if errors.Is(err, errDup) {
				if onConflict {
					if _, err := o.heap.ModifyHeader(tid, func(h *storage.TupleHeader) error {
						h.Flags |= storage.FlagKilled
						return nil
					}); err != nil {
						return 0, 0, false, err
					}
					return tid, conflict, true, nil
				}
				name := ix.Name
				e := pgerr.New(pgerr.UniqueViolation, "duplicate key value violates unique constraint \"%s\"", name).
					WithDetail("Key %s already exists.", describeKey(o.t, ix.Columns, vals))
				e.Table, e.Constraint = o.t.Name, name
				return 0, 0, false, e
			}
			return 0, 0, false, err
		}
	}
	return tid, 0, false, nil
}

// insertIndex adds the entry for tid to ix, enforcing uniqueness. On a
// unique violation it returns errDup and the conflicting TID.
func (o *tableOps) insertIndex(ix *catalog.Index, vals Row, tid storage.TID) (storage.TID, error) {
	tree := o.c.Store.BTree(ix.Root)
	key := indexKey(ix, vals, tid)
	unique := ix.Unique
	for _, c := range ix.Columns {
		if vals[c].IsNull() {
			unique = false // NULLs are distinct
		}
	}
	if !unique {
		return 0, tree.Insert(key, 0, nil)
	}
	var buf []byte
	for {
		var wait uint64
		var conflict storage.TID
		err := tree.Insert(key, len(key)-8, func(existing []byte) error {
			etid := tidFromKey(existing)
			if etid == tid {
				return nil
			}
			tuple, found, err := o.heap.Fetch(etid, buf)
			if err != nil {
				return err
			}
			if !found {
				return nil
			}
			buf = tuple
			h := storage.DecodeHeader(tuple)
			live, w := o.c.liveDirty(h)
			if w != 0 {
				wait = w
				return errWait
			}
			if live {
				conflict = etid
				return errDup
			}
			if o.c.deadToAll(h) {
				return storage.ErrRemoveKey
			}
			return nil
		})
		if errors.Is(err, errWait) {
			if err := o.c.Txn.WaitFor(wait); err != nil {
				return 0, err
			}
			continue
		}
		return conflict, err
	}
}

// lockRow marks the tuple at tid as deleted (or updated) by the current
// transaction, waiting for a concurrent writer to finish first. Under
// snapshot isolation a row changed by a transaction that committed after
// our snapshot cannot be changed again: that is a serialization failure
// (first updater wins). ok is false if this statement already changed the
// row (a join produced it twice).
func (o *tableOps) lockRow(tid storage.TID, update bool) (bool, error) {
	xid, err := o.c.Txn.AssignXid()
	if err != nil {
		return false, err
	}
	m := o.c.Txn.Manager()
	for {
		var wait uint64
		var serr error
		_, err := o.heap.ModifyHeader(tid, func(h *storage.TupleHeader) error {
			if h.Xmax != 0 {
				lockOnly := h.Flags&storage.FlagLockOnly != 0
				if h.Xmax == xid {
					if !lockOnly {
						return errSkip
					}
				} else {
					switch m.Status(h.Xmax) {
					case txn.Committed:
						if !lockOnly {
							what := "update"
							if h.Flags&storage.FlagUpdated == 0 {
								what = "delete"
							}
							serr = pgerr.New(pgerr.SerializationFailure, "could not serialize access due to concurrent %s", what)
							if o.c.Txn.ReadCommitted {
								serr = &RestartError{Err: serr}
							}
							return serr
						}
					case txn.InProgress:
						wait = h.Xmax
						return errWait
					}
				}
			}
			h.Xmax = xid
			h.Cmax = o.c.Txn.Cid
			h.Flags &^= storage.FlagLockOnly | storage.FlagUpdated
			if update {
				h.Flags |= storage.FlagUpdated
			}
			return nil
		})
		switch {
		case errors.Is(err, errSkip):
			return false, nil
		case serr != nil:
			return false, serr
		case err == nil:
			o.c.touched = append(o.c.touched, touch{heap: o.heap, tid: tid, locked: true})
		case errors.Is(err, errWait):
			if err := o.c.Txn.WaitFor(wait); err != nil {
				return false, err
			}
			continue
		case err != nil:
			return false, err
		}
		return true, nil
	}
}

// ---- foreign keys ----

func uniqueIndexOn(cat *catalog.Catalog, t *catalog.Table, cols []int) *catalog.Index {
	for _, ix := range cat.TableIndexes(t) {
		if !ix.Unique || len(ix.Columns) != len(cols) {
			continue
		}
		match := true
		for i := range cols {
			if ix.Columns[i] != cols[i] {
				match = false
			}
		}
		if match {
			return ix
		}
	}
	return nil
}

// fkKey encodes the referencing values in the parent's column types.
func fkKey(parent *catalog.Table, refCols []int, vals []types.Value, from []types.T) ([]byte, error) {
	var k []byte
	for i, c := range refCols {
		v, err := types.Cast(vals[i], from[i], parent.Columns[c].Type, false)
		if err != nil {
			return nil, err
		}
		k = types.EncodeKey(k, v)
	}
	return k, nil
}

// existsDirty reports whether a live row has an index key starting with
// prefix, waiting for in-progress inserters as needed.
func (c *Ctx) existsDirty(t *catalog.Table, ix *catalog.Index, prefix []byte) (bool, error) {
	heap := c.Store.Heap(t.Heap)
	for {
		cur := c.Store.BTree(ix.Root).Seek(prefix)
		var wait uint64
		var buf []byte
		for {
			k, ok, err := cur.Next()
			if err != nil {
				return false, err
			}
			if !ok || !strings.HasPrefix(string(k), string(prefix)) {
				break
			}
			tuple, found, err := heap.Fetch(tidFromKey(k), buf)
			if err != nil {
				return false, err
			}
			if !found {
				continue
			}
			buf = tuple
			h := storage.DecodeHeader(tuple)
			live, w := c.liveDirty(h)
			if live {
				return true, nil
			}
			if w != 0 && w == h.Xmin && wait == 0 {
				wait = w
			}
		}
		if wait == 0 {
			return false, nil
		}
		if err := c.Txn.WaitFor(wait); err != nil {
			return false, err
		}
	}
}

// checkOutgoingFKs verifies that each foreign key of the row references
// an existing parent row, and locks the parent key against deletion.
func (o *tableOps) checkOutgoingFKs(vals Row, old Row) error {
	for _, fk := range o.t.FKs {
		kv := make([]types.Value, len(fk.Columns))
		from := make([]types.T, len(fk.Columns))
		null, changed := false, old == nil
		for i, c := range fk.Columns {
			kv[i] = vals[c]
			from[i] = o.t.Columns[c].Type
			if kv[i].IsNull() {
				null = true
			}
			if old != nil && !types.Equal(old[c], vals[c]) {
				changed = true
			}
		}
		if null || !changed {
			continue
		}
		parent := o.c.Cat.Tables[fk.RefTable]
		if parent == nil {
			return pgerr.Internal("foreign key %s references a missing table", fk.Name)
		}
		pix := uniqueIndexOn(o.c.Cat, parent, fk.RefColumns)
		if pix == nil {
			return pgerr.Internal("foreign key %s has no unique index on the referenced columns", fk.Name)
		}
		prefix, err := fkKey(parent, fk.RefColumns, kv, from)
		if err != nil {
			return err
		}
		if err := o.c.Txn.Lock(txn.Tag{Kind: txn.TagKey, ID: uint64(pix.Oid), Key: string(prefix)}, txn.Share); err != nil {
			return err
		}
		found, err := o.c.existsDirty(parent, pix, prefix)
		if err != nil {
			return err
		}
		if !found {
			e := pgerr.New(pgerr.ForeignKeyViolation, "insert or update on table \"%s\" violates foreign key constraint \"%s\"", o.t.Name, fk.Name).
				WithDetail("Key %s is not present in table \"%s\".", describeKey(o.t, fk.Columns, vals), parent.Name)
			e.Table, e.Constraint = o.t.Name, fk.Name
			return e
		}
	}
	return nil
}

// checkIncomingFKs handles rows of other tables that reference a row being
// deleted (newVals nil) or whose key is being updated.
func (o *tableOps) checkIncomingFKs(old Row, newVals Row, depth int) error {
	if depth > 64 {
		return pgerr.New(pgerr.StatementTooComplex, "foreign key cascade is too deep")
	}
	for _, ref := range o.c.Cat.ReferencedBy(o.t.Oid) {
		fk := ref.FK
		kv := make([]types.Value, len(fk.RefColumns))
		from := make([]types.T, len(fk.RefColumns))
		null, changed := false, newVals == nil
		for i, c := range fk.RefColumns {
			kv[i] = old[c]
			from[i] = o.t.Columns[c].Type
			if kv[i].IsNull() {
				null = true
			}
			if newVals != nil && !types.Equal(old[c], newVals[c]) {
				changed = true
			}
		}
		if null || !changed {
			continue
		}
		pix := uniqueIndexOn(o.c.Cat, o.t, fk.RefColumns)
		if pix == nil {
			continue
		}
		prefix, err := fkKey(o.t, fk.RefColumns, kv, from)
		if err != nil {
			return err
		}
		if err := o.c.Txn.Lock(txn.Tag{Kind: txn.TagKey, ID: uint64(pix.Oid), Key: string(prefix)}, txn.Exclusive); err != nil {
			return err
		}
		child := ref.Table
		rows, err := o.c.referencingRows(child, fk, kv, from)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			continue
		}
		action := fk.OnDelete
		if newVals != nil {
			action = fk.OnUpdate
		}
		cops, err := o.c.ops(child)
		if err != nil {
			return err
		}
		switch action {
		case "CASCADE":
			for _, r := range rows {
				if newVals == nil {
					if _, err := cops.deleteRow(r.tid, r.vals, depth+1); err != nil {
						return err
					}
				} else {
					nv := append(Row(nil), r.vals...)
					for i, c := range fk.Columns {
						nv[c] = newVals[fk.RefColumns[i]]
					}
					if _, err := cops.updateRow(r.tid, r.vals, nv, depth+1); err != nil {
						return err
					}
				}
			}
		case "SET NULL", "SET DEFAULT":
			for _, r := range rows {
				nv := append(Row(nil), r.vals...)
				for _, c := range fk.Columns {
					nv[c] = types.Null
				}
				if _, err := cops.updateRow(r.tid, r.vals, nv, depth+1); err != nil {
					return err
				}
			}
		default:
			e := pgerr.New(pgerr.ForeignKeyViolation, "update or delete on table \"%s\" violates foreign key constraint \"%s\" on table \"%s\"", o.t.Name, fk.Name, child.Name).
				WithDetail("Key %s is still referenced from table \"%s\".", describeKey(o.t, fk.RefColumns, old), child.Name)
			e.Table, e.Constraint = child.Name, fk.Name
			return e
		}
	}
	return nil
}

type refRow struct {
	tid  storage.TID
	vals Row
}

// referencingRows finds live rows of child whose foreign key equals kv.
func (c *Ctx) referencingRows(child *catalog.Table, fk *catalog.ForeignKey, kv []types.Value, from []types.T) ([]refRow, error) {
	want := make([]types.Value, len(fk.Columns))
	for i, col := range fk.Columns {
		v, err := types.Cast(kv[i], from[i], child.Columns[col].Type, false)
		if err != nil {
			return nil, err
		}
		want[i] = v
	}
	heap := c.Store.Heap(child.Heap)
	matches := func(vals Row) bool {
		for i, col := range fk.Columns {
			if vals[col].IsNull() || types.Compare(vals[col], want[i]) != 0 {
				return false
			}
		}
		return true
	}
	for {
		var out []refRow
		var wait uint64
		visit := func(tid storage.TID, tuple []byte) error {
			h := storage.DecodeHeader(tuple)
			live, w := c.liveDirty(h)
			if !live && w == 0 {
				return nil
			}
			vals, err := decodeTuple(child, tuple)
			if err != nil {
				return err
			}
			if !matches(vals) {
				return nil
			}
			if w != 0 {
				if wait == 0 {
					wait = w
				}
				return nil
			}
			out = append(out, refRow{tid, vals})
			return nil
		}
		// Use an index whose leading columns are the foreign key.
		var ix *catalog.Index
		for _, cand := range c.Cat.TableIndexes(child) {
			if len(cand.Columns) >= len(fk.Columns) {
				ok := true
				for i := range fk.Columns {
					if cand.Columns[i] != fk.Columns[i] {
						ok = false
					}
				}
				if ok {
					ix = cand
					break
				}
			}
		}
		if ix != nil {
			var prefix []byte
			for _, v := range want {
				prefix = types.EncodeKey(prefix, v)
			}
			cur := c.Store.BTree(ix.Root).Seek(prefix)
			var buf []byte
			for {
				k, ok, err := cur.Next()
				if err != nil {
					return nil, err
				}
				if !ok || !strings.HasPrefix(string(k), string(prefix)) {
					break
				}
				tid := tidFromKey(k)
				tuple, found, err := heap.Fetch(tid, buf)
				if err != nil {
					return nil, err
				}
				if !found {
					continue
				}
				buf = tuple
				if err := visit(tid, tuple); err != nil {
					return nil, err
				}
			}
		} else {
			cur := heap.Scan()
			for {
				tid, tuple, ok, err := cur.Next()
				if err != nil {
					return nil, err
				}
				if !ok {
					break
				}
				if err := visit(tid, tuple); err != nil {
					return nil, err
				}
			}
		}
		if wait == 0 {
			return out, nil
		}
		if err := c.Txn.WaitFor(wait); err != nil {
			return nil, err
		}
	}
}

// deleteRow deletes one row version (with foreign key actions). It
// returns false if this statement already deleted the row.
func (o *tableOps) deleteRow(tid storage.TID, old Row, depth int) (bool, error) {
	ok, err := o.lockRow(tid, false)
	if err != nil || !ok {
		return false, err
	}
	if err := o.checkIncomingFKs(old, nil, depth); err != nil {
		return false, err
	}
	if o.c.TableChanged != nil {
		o.c.TableChanged(o.t.Oid, 0, 1)
	}
	return true, nil
}

// updateRow replaces a row version with a new one. It returns false if
// this statement already updated the row.
func (o *tableOps) updateRow(tid storage.TID, old, newVals Row, depth int) (bool, error) {
	if err := o.validate(newVals); err != nil {
		return false, err
	}
	ok, err := o.lockRow(tid, true)
	if err != nil || !ok {
		return false, err
	}
	if err := o.checkOutgoingFKs(newVals, old); err != nil {
		return false, err
	}
	// The new version goes in before referencing rows are cascaded, so
	// that their foreign key checks find the new key.
	if _, _, _, err := o.insertVersion(newVals, old, false); err != nil {
		return false, err
	}
	if err := o.checkIncomingFKs(old, newVals, depth); err != nil {
		return false, err
	}
	if o.c.TableChanged != nil {
		o.c.TableChanged(o.t.Oid, 1, 1)
	}
	return true, nil
}

// ---- INSERT ----

type insertIter struct {
	c         *Ctx
	p         *planner.InsertP
	src       Iter
	exprs     []expr.Fn
	returning []expr.Fn
	ops       *tableOps
	ec        *expr.Ctx
	// ON CONFLICT DO UPDATE
	setFns  map[int]expr.Fn
	whereFn expr.Fn
}

func (c *Ctx) newInsert(p *planner.InsertP) (Iter, error) {
	src, err := c.Build(p.Source)
	if err != nil {
		return nil, err
	}
	ins := p.Ins
	it := &insertIter{c: c, p: p, src: src}
	if it.exprs, err = compileAll(ins.Exprs, layoutOf(p.Source.Layout())); err != nil {
		return nil, err
	}
	if it.returning, err = compileAll(ins.Returning, layoutOf(ins.ReturningCols)); err != nil {
		return nil, err
	}
	if ins.ConflictSet != nil {
		l := layoutOf(append(append([]expr.ColumnID(nil), ins.ExistingCols...), ins.ExcludedCols...))
		it.setFns = map[int]expr.Fn{}
		for i, e := range ins.ConflictSet {
			f, err := expr.Compile(e, l)
			if err != nil {
				return nil, err
			}
			it.setFns[i] = f
		}
		if it.whereFn, err = compile(ins.ConflictWhere, l); err != nil {
			return nil, err
		}
	}
	if it.ops, err = c.ops(ins.Table); err != nil {
		return nil, err
	}
	return it, nil
}

func (it *insertIter) Open(ec *expr.Ctx) error { it.ec = ec; return it.src.Open(ec) }

func (it *insertIter) Next() (Row, error) {
	for {
		r, err := it.src.Next()
		if err != nil || r == nil {
			return nil, err
		}
		if err := it.c.check(); err != nil {
			return nil, err
		}
		vals := make(Row, len(it.exprs))
		for i, f := range it.exprs {
			v, err := f(it.ec, r)
			if err != nil {
				return nil, err
			}
			vals[i] = v
		}
		if err := it.ops.validate(vals); err != nil {
			return nil, err
		}
		if err := it.ops.checkOutgoingFKs(vals, nil); err != nil {
			return nil, err
		}
		onConflict := it.p.Ins.OnConflictDoNothing || it.setFns != nil
		_, conflict, hit, err := it.ops.insertTuple(vals, onConflict)
		if err != nil {
			return nil, err
		}
		if hit {
			if it.setFns == nil {
				continue // DO NOTHING
			}
			out, done, err := it.upsert(conflict, vals)
			if err != nil {
				return nil, err
			}
			if !done {
				continue
			}
			vals = out
		} else {
			it.c.RowsAffected++
			if it.c.TableChanged != nil {
				it.c.TableChanged(it.p.Ins.Table.Oid, 1, 0)
			}
		}
		if len(it.returning) == 0 {
			continue
		}
		return evalAll(it.returning, it.ec, vals)
	}
}

// upsert applies ON CONFLICT DO UPDATE to the conflicting row.
func (it *insertIter) upsert(tid storage.TID, excluded Row) (Row, bool, error) {
	o := it.ops
	tuple, found, err := o.heap.Fetch(tid, nil)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, pgerr.Internal("conflicting row disappeared")
	}
	existing, err := decodeTuple(o.t, tuple)
	if err != nil {
		return nil, false, err
	}
	both := concat(existing, excluded)
	if it.whereFn != nil {
		v, err := it.whereFn(it.ec, both)
		if err != nil {
			return nil, false, err
		}
		if !expr.Truth(v) {
			return nil, false, nil
		}
	}
	nv := append(Row(nil), existing...)
	for i, f := range it.setFns {
		v, err := f(it.ec, both)
		if err != nil {
			return nil, false, err
		}
		nv[i] = v
	}
	h := storage.DecodeHeader(tuple)
	if h.Xmax == it.c.Txn.Xid && h.Cmax == it.c.Txn.Cid && it.c.Txn.Xid != 0 {
		return nil, false, pgerr.New(pgerr.CardinalityViolation, "ON CONFLICT DO UPDATE command cannot affect row a second time").
			WithHint("Ensure that no rows proposed for insertion within the same command have duplicate constrained values.")
	}
	ok, err := o.updateRow(tid, existing, nv, 0)
	if err != nil {
		return nil, false, err
	}
	if ok {
		it.c.RowsAffected++
	}
	return nv, ok, nil
}

func evalAll(fs []expr.Fn, ec *expr.Ctx, row Row) (Row, error) {
	out := make(Row, len(fs))
	for i, f := range fs {
		v, err := f(ec, row)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (it *insertIter) Close() { it.src.Close() }

// ---- UPDATE ----

type updateIter struct {
	c         *Ctx
	p         *planner.UpdateP
	src       Iter
	tidPos    int
	colPos    []int
	set       map[int]expr.Fn
	returning []expr.Fn
	ops       *tableOps
	ec        *expr.Ctx
}

func (c *Ctx) newUpdate(p *planner.UpdateP) (Iter, error) {
	src, err := c.Build(p.Source)
	if err != nil {
		return nil, err
	}
	up := p.Upd
	l := layoutOf(p.Source.Layout())
	it := &updateIter{c: c, p: p, src: src, set: map[int]expr.Fn{}}
	it.tidPos = l[up.Scan.TIDCol]
	for _, id := range up.Scan.ColIDs {
		it.colPos = append(it.colPos, l[id])
	}
	for i, e := range up.Set {
		f, err := expr.Compile(e, l)
		if err != nil {
			return nil, err
		}
		it.set[i] = f
	}
	if len(up.Returning) > 0 {
		rl := layoutOf(append(append([]expr.ColumnID(nil), up.NewCols...), p.Source.Layout()...))
		if it.returning, err = compileAll(up.Returning, rl); err != nil {
			return nil, err
		}
	}
	if it.ops, err = c.ops(up.Table); err != nil {
		return nil, err
	}
	return it, nil
}

func (it *updateIter) Open(ec *expr.Ctx) error { it.ec = ec; return it.src.Open(ec) }

func (it *updateIter) Next() (Row, error) {
	for {
		r, err := it.src.Next()
		if err != nil || r == nil {
			return nil, err
		}
		if err := it.c.check(); err != nil {
			return nil, err
		}
		tid := storage.TID(r[it.tidPos].I)
		old := make(Row, len(it.colPos))
		for i, p := range it.colPos {
			old[i] = r[p]
		}
		nv := append(Row(nil), old...)
		for i, f := range it.set {
			v, err := f(it.ec, r)
			if err != nil {
				return nil, err
			}
			nv[i] = v
		}
		ok, err := it.ops.updateRow(tid, old, nv, 0)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		it.c.RowsAffected++
		if len(it.returning) == 0 {
			continue
		}
		return evalAll(it.returning, it.ec, concat(nv, r))
	}
}

func (it *updateIter) Close() { it.src.Close() }

// ---- DELETE ----

type deleteIter struct {
	c         *Ctx
	p         *planner.DeleteP
	src       Iter
	tidPos    int
	colPos    []int
	returning []expr.Fn
	ops       *tableOps
	ec        *expr.Ctx
}

func (c *Ctx) newDelete(p *planner.DeleteP) (Iter, error) {
	src, err := c.Build(p.Source)
	if err != nil {
		return nil, err
	}
	del := p.Del
	l := layoutOf(p.Source.Layout())
	it := &deleteIter{c: c, p: p, src: src, tidPos: l[del.Scan.TIDCol]}
	for _, id := range del.Scan.ColIDs {
		it.colPos = append(it.colPos, l[id])
	}
	if it.returning, err = compileAll(del.Returning, l); err != nil {
		return nil, err
	}
	if it.ops, err = c.ops(del.Table); err != nil {
		return nil, err
	}
	return it, nil
}

func (it *deleteIter) Open(ec *expr.Ctx) error { it.ec = ec; return it.src.Open(ec) }

func (it *deleteIter) Next() (Row, error) {
	for {
		r, err := it.src.Next()
		if err != nil || r == nil {
			return nil, err
		}
		if err := it.c.check(); err != nil {
			return nil, err
		}
		tid := storage.TID(r[it.tidPos].I)
		old := make(Row, len(it.colPos))
		for i, p := range it.colPos {
			old[i] = r[p]
		}
		ok, err := it.ops.deleteRow(tid, old, 0)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		it.c.RowsAffected++
		if len(it.returning) == 0 {
			continue
		}
		return evalAll(it.returning, it.ec, r)
	}
}

func (it *deleteIter) Close() { it.src.Close() }
