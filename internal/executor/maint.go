package executor

import (
	"errors"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/storage"
	"github.com/useless-husband/basalt/internal/txn"
)

// BuildIndex fills a new index with an entry for every tuple version that
// some snapshot might still see. Uniqueness is checked among live versions.
func (c *Ctx) BuildIndex(t *catalog.Table, ix *catalog.Index) error {
	heap := c.Store.Heap(t.Heap)
	o := &tableOps{c: c, t: t, heap: heap}
	tree := c.Store.BTree(ix.Root)
	m := c.Txn.Manager()
	cur := heap.Scan()
	n := 0
	for {
		tid, tuple, ok, err := cur.Next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if n++; n%1024 == 0 {
			if err := c.check(); err != nil {
				return err
			}
		}
		h := storage.DecodeHeader(tuple)
		if h.Flags&storage.FlagKilled != 0 || m.Status(h.Xmin) == txn.Aborted {
			continue
		}
		vals, err := decodeTuple(t, tuple)
		if err != nil {
			return err
		}
		if s := storedColumns(tuple); s < len(t.Columns) {
			fillMissing(t, vals, s)
		}
		live, wait := c.liveDirty(h)
		if ix.Unique && (live || wait != 0) {
			if _, err := o.insertIndex(ix, vals, tid); err != nil {
				if errors.Is(err, errDup) {
					return pgerr.New(pgerr.UniqueViolation, "could not create unique index \"%s\"", ix.Name).
						WithDetail("Key %s is duplicated.", describeKey(t, ix.Columns, vals))
				}
				return err
			}
			continue
		}
		if err := tree.Insert(indexKey(ix, vals, tid), 0, nil); err != nil {
			return err
		}
	}
}

// ValidateTable checks every visible row against the table's NOT NULL,
// CHECK and foreign key constraints (ALTER TABLE ADD CONSTRAINT).
func (c *Ctx) ValidateTable(t *catalog.Table) error {
	o, err := c.ops(t)
	if err != nil {
		return err
	}
	heap := c.Store.Heap(t.Heap)
	cur := heap.Scan()
	for {
		_, tuple, ok, err := cur.Next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if !c.Txn.Visible(storage.DecodeHeader(tuple)) {
			continue
		}
		vals, err := decodeTuple(t, tuple)
		if err != nil {
			return err
		}
		if s := storedColumns(tuple); s < len(t.Columns) {
			fillMissing(t, vals, s)
		}
		if err := o.validate(vals); err != nil {
			return err
		}
		if err := o.checkOutgoingFKs(vals, nil); err != nil {
			return err
		}
	}
}

// VacuumStats reports what VACUUM did to a table.
type VacuumStats struct {
	Pages, Removed, Kept int
}

// Vacuum removes tuple versions that no snapshot can see any more: those
// inserted by aborted transactions and those deleted by transactions that
// committed before horizon (the oldest xmin of any running snapshot).
// Index entries are removed before the heap slots are freed, so an index
// never points at a reused slot.
func (c *Ctx) Vacuum(t *catalog.Table, horizon uint64) (VacuumStats, error) {
	var st VacuumStats
	heap := c.Store.Heap(t.Heap)
	m := c.Txn.Manager()
	indexes := c.Cat.TableIndexes(t)
	dead := func(tuple []byte) bool {
		h := storage.DecodeHeader(tuple)
		if h.Flags&storage.FlagKilled != 0 {
			return true
		}
		if m.Status(h.Xmin) == txn.Aborted {
			return true
		}
		if h.Xmax != 0 && h.Flags&storage.FlagLockOnly == 0 && h.Xmax < horizon && m.Status(h.Xmax) == txn.Committed {
			return true
		}
		return false
	}
	pages, err := heap.Pages()
	if err != nil {
		return st, err
	}
	for _, pid := range pages {
		if err := c.check(); err != nil {
			return st, err
		}
		st.Pages++
		tuples, err := heap.PageTuples(pid)
		if err != nil {
			return st, err
		}
		var slots []uint16
		for slot, tuple := range tuples {
			if !dead(tuple) {
				st.Kept++
				continue
			}
			vals, err := decodeTuple(t, tuple)
			if err != nil {
				return st, err
			}
			if s := storedColumns(tuple); s < len(t.Columns) {
				fillMissing(t, vals, s)
			}
			tid := storage.MakeTID(pid, slot)
			for _, ix := range indexes {
				if _, err := c.Store.BTree(ix.Root).Delete(indexKey(ix, vals, tid)); err != nil {
					return st, err
				}
			}
			slots = append(slots, slot)
		}
		if len(slots) == 0 {
			continue
		}
		n, err := heap.RemoveTuples(pid, slots, dead)
		if err != nil {
			return st, err
		}
		st.Removed += n
	}
	return st, nil
}
