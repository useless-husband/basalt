package storage

import (
	"bytes"
	"errors"
	"sync"

	"github.com/useless-husband/basalt/internal/pgerr"
)

// B+tree node layout after the common header:
//
//	16 nkeys u16
//	18 freeStart u16 (end of the slot array)
//	20 freeEnd u16   (start of entry data)
//	22 level u16     (0 for leaves)
//	24 right sibling u32
//	28 leftmost child u32 (internal nodes)
//	32 slot array: u16 offsets of entries, in key order
//
// A leaf entry is (klen u16, key). An internal entry is (child u32, klen
// u16, key); its child holds the keys >= key and < the next entry's key,
// while the leftmost child holds the keys below the first entry.
//
// Keys are unique byte strings compared with bytes.Compare. Index keys
// end with the TID of the row, which makes them unique even for
// non-unique indexes and lets a delete find exactly one entry.
const (
	offBtNKeys     = 16
	offBtFreeStart = 18
	offBtFreeEnd   = 20
	offBtLevel     = 22
	offBtRight     = 24
	offBtLeftmost  = 28
	btSlots        = 32
	btCapacity     = PageSize - btSlots
	// MaxKeySize is the largest key a B+tree accepts.
	MaxKeySize = 1024
)

// BTree is a B+tree index. The root page never moves: when the root
// splits, its contents move to two new children.
//
// Concurrency: a tree-level RWMutex. Writers (insert, delete) hold it
// exclusively for the duration of one operation; cursors hold it shared
// only while copying a batch of keys and re-descend for the next batch, so
// a long scan never blocks writers for long. This is simpler than latch
// coupling at the cost of serializing writers on the same index.
type BTree struct {
	s    *Store
	root PageID
	mu   sync.RWMutex
}

// CreateBTree allocates the root (an empty leaf) of a new tree.
func (m *Mtr) CreateBTree() (PageID, error) {
	id, p, err := m.Alloc(PageBtree)
	if err != nil {
		return 0, err
	}
	btInit(p, 0)
	return id, nil
}

// BTree returns the tree rooted at root.
func (s *Store) BTree(root PageID) *BTree {
	s.objMu.Lock()
	defer s.objMu.Unlock()
	t, ok := s.btrees[root]
	if !ok {
		t = &BTree{s: s, root: root}
		s.btrees[root] = t
	}
	return t
}

func btInit(p []byte, level int) {
	clear(p[pageHeader:])
	p[offType] = PageBtree
	put16(p, offBtFreeStart, btSlots)
	put16(p, offBtFreeEnd, PageSize)
	put16(p, offBtLevel, uint16(level))
}

func btN(p []byte) int           { return int(u16(p, offBtNKeys)) }
func btLevel(p []byte) int       { return int(u16(p, offBtLevel)) }
func btIsLeaf(p []byte) bool     { return btLevel(p) == 0 }
func btRight(p []byte) PageID    { return PageID(u32(p, offBtRight)) }
func btLeftmost(p []byte) PageID { return PageID(u32(p, offBtLeftmost)) }

func btEntryOff(p []byte, i int) int { return int(u16(p, btSlots+2*i)) }

func btKey(p []byte, i int) []byte {
	off := btEntryOff(p, i)
	if !btIsLeaf(p) {
		off += 4
	}
	l := int(u16(p, off))
	return p[off+2 : off+2+l]
}

func btChild(p []byte, i int) PageID {
	return PageID(u32(p, btEntryOff(p, i)))
}

// btChildAt returns child number c: 0 is the leftmost, c>0 is entry c-1's.
func btChildAt(p []byte, c int) PageID {
	if c == 0 {
		return btLeftmost(p)
	}
	return btChild(p, c-1)
}

func entrySize(leaf bool, key []byte) int {
	n := 2 + 2 + len(key) // slot + klen + key
	if !leaf {
		n += 4
	}
	return n
}

// btSearch returns the first index whose key is >= key, and whether it is
// an exact match.
func btSearch(p []byte, key []byte) (int, bool) {
	lo, hi := 0, btN(p)
	for lo < hi {
		mid := (lo + hi) / 2
		if bytes.Compare(btKey(p, mid), key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < btN(p) && bytes.Equal(btKey(p, lo), key)
}

// btChildFor returns the child index (0 = leftmost) to follow for key.
func btChildFor(p []byte, key []byte) int {
	// Number of entries with key_i <= key.
	lo, hi := 0, btN(p)
	for lo < hi {
		mid := (lo + hi) / 2
		if bytes.Compare(btKey(p, mid), key) <= 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func btUsed(p []byte) int {
	used := 0
	leaf := btIsLeaf(p)
	for i := 0; i < btN(p); i++ {
		used += entrySize(leaf, btKey(p, i))
	}
	return used
}

func btCompact(p []byte) {
	type ent struct {
		child PageID
		key   []byte
	}
	n := btN(p)
	leaf := btIsLeaf(p)
	ents := make([]ent, n)
	for i := 0; i < n; i++ {
		e := ent{key: append([]byte(nil), btKey(p, i)...)}
		if !leaf {
			e.child = btChild(p, i)
		}
		ents[i] = e
	}
	end := PageSize
	for i, e := range ents {
		sz := 2 + len(e.key)
		if !leaf {
			sz += 4
		}
		end -= sz
		off := end
		if !leaf {
			put32(p, off, uint32(e.child))
			off += 4
		}
		put16(p, off, uint16(len(e.key)))
		copy(p[off+2:], e.key)
		put16(p, btSlots+2*i, uint16(end))
	}
	clear(p[btSlots+2*n : end])
	put16(p, offBtFreeEnd, uint16(end))
}

// btInsertAt inserts an entry at index i, compacting if needed. It returns
// false if the node has no room.
func btInsertAt(p []byte, i int, key []byte, child PageID) bool {
	leaf := btIsLeaf(p)
	need := entrySize(leaf, key)
	free := int(u16(p, offBtFreeEnd)) - int(u16(p, offBtFreeStart))
	if free < need {
		if btCapacity-btUsed(p) < need {
			return false
		}
		btCompact(p)
	}
	n := btN(p)
	dataLen := need - 2
	end := int(u16(p, offBtFreeEnd)) - dataLen
	off := end
	if !leaf {
		put32(p, off, uint32(child))
		off += 4
	}
	put16(p, off, uint16(len(key)))
	copy(p[off+2:], key)
	// Shift slots right.
	copy(p[btSlots+2*(i+1):btSlots+2*(n+1)], p[btSlots+2*i:btSlots+2*n])
	put16(p, btSlots+2*i, uint16(end))
	put16(p, offBtNKeys, uint16(n+1))
	put16(p, offBtFreeStart, uint16(btSlots+2*(n+1)))
	put16(p, offBtFreeEnd, uint16(end))
	return true
}

func btRemoveAt(p []byte, i int) {
	n := btN(p)
	copy(p[btSlots+2*i:btSlots+2*(n-1)], p[btSlots+2*(i+1):btSlots+2*n])
	put16(p, btSlots+2*(n-1), 0)
	put16(p, offBtNKeys, uint16(n-1))
	put16(p, offBtFreeStart, uint16(btSlots+2*(n-1)))
}

type btEnt struct {
	key   []byte
	child PageID
}

func btEntries(p []byte) []btEnt {
	n := btN(p)
	leaf := btIsLeaf(p)
	out := make([]btEnt, n)
	for i := 0; i < n; i++ {
		out[i].key = append([]byte(nil), btKey(p, i)...)
		if !leaf {
			out[i].child = btChild(p, i)
		}
	}
	return out
}

// btWrite rebuilds a node with the given entries.
func btWrite(p []byte, level int, right, leftmost PageID, ents []btEnt) {
	btInit(p, level)
	put32(p, offBtRight, uint32(right))
	put32(p, offBtLeftmost, uint32(leftmost))
	for i, e := range ents {
		if !btInsertAt(p, i, e.key, e.child) {
			panic("btree: node overflow while rebuilding")
		}
	}
}

func errKeyTooLarge(n int) error {
	return pgerr.New(pgerr.ProgramLimitExceeded, "index row size %d exceeds btree maximum, %d", n, MaxKeySize)
}

// descend walks from the root to the leaf for key using shared latches,
// returning the page path and the child index taken at each internal node.
// The caller holds the tree lock.
func (t *BTree) descend(key []byte) ([]PageID, []int, error) {
	var path []PageID
	var idx []int
	id := t.root
	for {
		f, err := t.s.pool.Read(id)
		if err != nil {
			return nil, nil, err
		}
		if pageType(f.Data) != PageBtree {
			t.s.pool.Release(f)
			return nil, nil, pgerr.New(pgerr.DataCorrupted, "page %d is not a btree page", id)
		}
		path = append(path, id)
		if btIsLeaf(f.Data) {
			t.s.pool.Release(f)
			return path, idx, nil
		}
		c := btChildFor(f.Data, key)
		next := btChildAt(f.Data, c)
		t.s.pool.Release(f)
		idx = append(idx, c)
		id = next
	}
}

// ErrRemoveKey may be returned by an Insert check callback: the existing
// key points at a row version no snapshot can see, and is deleted.
var ErrRemoveKey = errors.New("remove this index entry")

// Insert adds a key. check, if not nil, is called first for every existing
// key that starts with key[:prefixLen] (used for unique constraints); if it
// returns an error nothing is inserted, except for ErrRemoveKey, which
// deletes that existing key and carries on.
func (t *BTree) Insert(key []byte, prefixLen int, check func(existing []byte) error) error {
	if len(key) > MaxKeySize {
		return errKeyTooLarge(len(key))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if check != nil {
		prefix := key[:prefixLen]
		var dead [][]byte
		err := t.scanPrefixLocked(prefix, func(k []byte) error {
			if err := check(k); err != nil {
				if err != ErrRemoveKey {
					return err
				}
				dead = append(dead, k)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range dead {
			if _, err := t.deleteLocked(k); err != nil {
				return err
			}
		}
	}
	path, idx, err := t.descend(key)
	if err != nil {
		return err
	}
	m := t.s.Begin()
	if err := t.insertLevel(m, path, idx, len(path)-1, key, 0); err != nil {
		m.Abort()
		return err
	}
	_, err = m.Commit()
	return err
}

func (t *BTree) scanPrefixLocked(prefix []byte, fn func([]byte) error) error {
	path, _, err := t.descend(prefix)
	if err != nil {
		return err
	}
	id := path[len(path)-1]
	first := true
	for id != InvalidPage {
		f, err := t.s.pool.Read(id)
		if err != nil {
			return err
		}
		i := 0
		if first {
			i, _ = btSearch(f.Data, prefix)
			first = false
		}
		var keys [][]byte
		stop := false
		for ; i < btN(f.Data); i++ {
			k := btKey(f.Data, i)
			if !bytes.HasPrefix(k, prefix) {
				stop = true
				break
			}
			keys = append(keys, append([]byte(nil), k...))
		}
		next := btRight(f.Data)
		t.s.pool.Release(f)
		for _, k := range keys {
			if err := fn(k); err != nil {
				return err
			}
		}
		if stop {
			return nil
		}
		id = next
	}
	return nil
}

// insertLevel inserts (key, child) into node path[level], splitting as
// needed.
func (t *BTree) insertLevel(m *Mtr, path []PageID, idx []int, level int, key []byte, child PageID) error {
	id := path[level]
	p, err := m.Page(id)
	if err != nil {
		return err
	}
	pos, found := btSearch(p, key)
	if found && btIsLeaf(p) {
		return pgerr.Internal("btree: duplicate key")
	}
	if btInsertAt(p, pos, key, child) {
		return nil
	}
	// Split: gather all entries including the new one.
	ents := btEntries(p)
	ents = append(ents, btEnt{})
	copy(ents[pos+1:], ents[pos:])
	ents[pos] = btEnt{key: append([]byte(nil), key...), child: child}
	lvl := btLevel(p)
	leaf := lvl == 0
	total := 0
	for _, e := range ents {
		total += entrySize(leaf, e.key)
	}
	// Split point: first index where the left half reaches half the bytes.
	acc, mid := 0, 0
	for i, e := range ents {
		acc += entrySize(leaf, e.key)
		if acc >= total/2 {
			mid = i + 1
			break
		}
	}
	if mid >= len(ents) {
		mid = len(ents) - 1
	}
	if mid < 1 {
		mid = 1
	}
	var leftEnts, rightEnts []btEnt
	var sep []byte
	var rightLeftmost PageID
	if leaf {
		leftEnts, rightEnts = ents[:mid], ents[mid:]
		sep = rightEnts[0].key
	} else {
		// The middle entry moves up; its child becomes the right node's
		// leftmost child.
		if mid >= len(ents)-1 {
			mid = len(ents) - 2
		}
		leftEnts, rightEnts = ents[:mid], ents[mid+1:]
		sep = ents[mid].key
		rightLeftmost = ents[mid].child
	}
	if level == 0 {
		// Root split: move both halves to new pages, the root becomes
		// their parent.
		lid, lp, err := m.Alloc(PageBtree)
		if err != nil {
			return err
		}
		rid, rp, err := m.Alloc(PageBtree)
		if err != nil {
			return err
		}
		btWrite(lp, lvl, rid, btLeftmost(p), leftEnts)
		btWrite(rp, lvl, InvalidPage, rightLeftmost, rightEnts)
		if !leaf {
			put32(lp, offBtRight, 0)
		}
		btWrite(p, lvl+1, InvalidPage, lid, []btEnt{{key: sep, child: rid}})
		return nil
	}
	rid, rp, err := m.Alloc(PageBtree)
	if err != nil {
		return err
	}
	oldRight := btRight(p)
	if leaf {
		btWrite(rp, lvl, oldRight, InvalidPage, rightEnts)
		btWrite(p, lvl, rid, InvalidPage, leftEnts)
	} else {
		btWrite(rp, lvl, InvalidPage, rightLeftmost, rightEnts)
		btWrite(p, lvl, InvalidPage, btLeftmost(p), leftEnts)
	}
	return t.insertLevel(m, path, idx, level-1, sep, rid)
}

// Delete removes a key, merging underfull nodes with a sibling when the
// two fit in one page. It reports whether the key was present.
func (t *BTree) Delete(key []byte) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.deleteLocked(key)
}

func (t *BTree) deleteLocked(key []byte) (bool, error) {
	path, idx, err := t.descend(key)
	if err != nil {
		return false, err
	}
	m := t.s.Begin()
	leaf, err := m.Page(path[len(path)-1])
	if err != nil {
		m.Abort()
		return false, err
	}
	pos, found := btSearch(leaf, key)
	if !found {
		m.Abort()
		return false, nil
	}
	btRemoveAt(leaf, pos)
	if err := t.rebalance(m, path, idx, len(path)-1); err != nil {
		m.Abort()
		return false, err
	}
	_, err = m.Commit()
	return err == nil, err
}

func (t *BTree) rebalance(m *Mtr, path []PageID, idx []int, level int) error {
	id := path[level]
	p, err := m.Page(id)
	if err != nil {
		return err
	}
	if level == 0 {
		// Collapse a root with a single child.
		if !btIsLeaf(p) && btN(p) == 0 {
			child := btLeftmost(p)
			cp, err := m.Page(child)
			if err != nil {
				return err
			}
			copy(p[pageHeader:], cp[pageHeader:])
			return m.Free(child)
		}
		return nil
	}
	if btUsed(p) >= btCapacity/4 {
		return nil
	}
	parent, err := m.Page(path[level-1])
	if err != nil {
		return err
	}
	c := idx[level-1]
	n := btN(parent)
	var leftID, rightID PageID
	var sepIdx int
	switch {
	case c < n:
		leftID, rightID, sepIdx = id, btChildAt(parent, c+1), c
	case c > 0:
		leftID, rightID, sepIdx = btChildAt(parent, c-1), id, c-1
	default:
		return nil
	}
	lp, err := m.Page(leftID)
	if err != nil {
		return err
	}
	rp, err := m.Page(rightID)
	if err != nil {
		return err
	}
	leaf := btIsLeaf(lp)
	sep := append([]byte(nil), btKey(parent, sepIdx)...)
	merged := btUsed(lp) + btUsed(rp)
	if !leaf {
		merged += entrySize(false, sep)
	}
	if merged > btCapacity {
		return nil // siblings too full to merge; leave the node underfull
	}
	ents := btEntries(lp)
	if !leaf {
		ents = append(ents, btEnt{key: sep, child: btLeftmost(rp)})
	}
	ents = append(ents, btEntries(rp)...)
	right := btRight(rp)
	if !leaf {
		right = InvalidPage
	}
	btWrite(lp, btLevel(lp), right, btLeftmost(lp), ents)
	btRemoveAt(parent, sepIdx)
	if err := m.Free(rightID); err != nil {
		return err
	}
	return t.rebalance(m, path, idx, level-1)
}

// Cursor iterates over keys in order, starting at a lower bound.
type Cursor struct {
	t     *BTree
	lo    []byte
	after []byte
	buf   [][]byte
	i     int
	done  bool
}

// Seek returns a cursor positioned at the first key >= lo.
func (t *BTree) Seek(lo []byte) *Cursor {
	return &Cursor{t: t, lo: append([]byte(nil), lo...)}
}

// Next returns the next key. The returned slice stays valid.
func (c *Cursor) Next() ([]byte, bool, error) {
	if c.i < len(c.buf) {
		k := c.buf[c.i]
		c.i++
		return k, true, nil
	}
	if c.done {
		return nil, false, nil
	}
	if err := c.refill(); err != nil {
		return nil, false, err
	}
	if len(c.buf) == 0 {
		c.done = true
		return nil, false, nil
	}
	k := c.buf[0]
	c.i = 1
	return k, true, nil
}

const cursorBatch = 128

func (c *Cursor) refill() error {
	t := c.t
	t.mu.RLock()
	defer t.mu.RUnlock()
	c.buf = c.buf[:0]
	c.i = 0
	path, _, err := t.descend(c.lo)
	if err != nil {
		return err
	}
	id := path[len(path)-1]
	first := true
	for id != InvalidPage && len(c.buf) < cursorBatch {
		f, err := t.s.pool.Read(id)
		if err != nil {
			return err
		}
		i := 0
		if first {
			i, _ = btSearch(f.Data, c.lo)
			first = false
		}
		for ; i < btN(f.Data); i++ {
			k := btKey(f.Data, i)
			if c.after != nil && bytes.Compare(k, c.after) <= 0 {
				continue
			}
			c.buf = append(c.buf, append([]byte(nil), k...))
		}
		id = btRight(f.Data)
		t.s.pool.Release(f)
	}
	if len(c.buf) > 0 {
		last := c.buf[len(c.buf)-1]
		c.after = last
		c.lo = last
	}
	if id == InvalidPage && len(c.buf) < cursorBatch {
		// Reached the end of the tree in this batch; the next call stops
		// unless the batch was cut short.
		if len(c.buf) == 0 {
			c.done = true
		}
	}
	return nil
}

// Stats returns the height and number of leaf pages of the tree.
func (t *BTree) Stats() (height, leaves int, err error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	id := t.root
	for {
		f, err := t.s.pool.Read(id)
		if err != nil {
			return 0, 0, err
		}
		height++
		leaf := btIsLeaf(f.Data)
		next := btLeftmost(f.Data)
		t.s.pool.Release(f)
		if leaf {
			break
		}
		id = next
	}
	for id != InvalidPage {
		f, err := t.s.pool.Read(id)
		if err != nil {
			return 0, 0, err
		}
		leaves++
		next := btRight(f.Data)
		t.s.pool.Release(f)
		id = next
	}
	return height, leaves, nil
}

// pages lists every page of the tree.
func (t *BTree) pages() ([]PageID, error) {
	var out []PageID
	queue := []PageID{t.root}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		out = append(out, id)
		f, err := t.s.pool.Read(id)
		if err != nil {
			return nil, err
		}
		if !btIsLeaf(f.Data) {
			queue = append(queue, btLeftmost(f.Data))
			for i := 0; i < btN(f.Data); i++ {
				queue = append(queue, btChild(f.Data, i))
			}
		}
		t.s.pool.Release(f)
	}
	return out, nil
}

// FreeBTree frees all pages of a tree (DROP INDEX). Like FreeHeap it is
// called after the catalog no longer references the tree.
func (s *Store) FreeBTree(root PageID) error {
	t := s.BTree(root)
	t.mu.Lock()
	pages, err := t.pages()
	t.mu.Unlock()
	if err != nil {
		return err
	}
	if err := s.freePages(pages); err != nil {
		return err
	}
	s.objMu.Lock()
	delete(s.btrees, root)
	s.objMu.Unlock()
	return nil
}

// Check verifies the tree's invariants: keys sorted within and across
// nodes, separators bounding their subtrees, uniform leaf depth, and a
// leaf chain that visits every key in order. It returns the number of keys.
func (t *BTree) Check() (int, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	leafDepth := -1
	count := 0
	var leaves []PageID
	var walk func(id PageID, lo, hi []byte, depth int) error
	walk = func(id PageID, lo, hi []byte, depth int) error {
		f, err := t.s.pool.Read(id)
		if err != nil {
			return err
		}
		p := append([]byte(nil), f.Data...)
		t.s.pool.Release(f)
		n := btN(p)
		for i := 0; i < n; i++ {
			k := btKey(p, i)
			if i > 0 && bytes.Compare(btKey(p, i-1), k) >= 0 {
				return pgerr.Internal("btree page %d: keys out of order at %d", id, i)
			}
			if lo != nil && bytes.Compare(k, lo) < 0 {
				return pgerr.Internal("btree page %d: key below lower bound", id)
			}
			if hi != nil && bytes.Compare(k, hi) >= 0 {
				return pgerr.Internal("btree page %d: key above upper bound", id)
			}
		}
		if btIsLeaf(p) {
			if leafDepth < 0 {
				leafDepth = depth
			} else if leafDepth != depth {
				return pgerr.Internal("btree: leaves at different depths")
			}
			count += n
			leaves = append(leaves, id)
			return nil
		}
		for c := 0; c <= n; c++ {
			clo, chi := lo, hi
			if c > 0 {
				clo = btKey(p, c-1)
			}
			if c < n {
				chi = btKey(p, c)
			}
			if err := walk(btChildAt(p, c), clo, chi, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(t.root, nil, nil, 0); err != nil {
		return 0, err
	}
	for i := 0; i+1 < len(leaves); i++ {
		f, err := t.s.pool.Read(leaves[i])
		if err != nil {
			return 0, err
		}
		r := btRight(f.Data)
		t.s.pool.Release(f)
		if r != leaves[i+1] {
			return 0, pgerr.Internal("btree: leaf chain broken at page %d", leaves[i])
		}
	}
	return count, nil
}
