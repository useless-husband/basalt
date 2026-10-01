package storage

import (
	"encoding/binary"
	"sync"

	"github.com/useless-husband/basalt/internal/pgerr"
)

// TID identifies a tuple: page number and slot.
type TID uint64

// MakeTID builds a TID.
func MakeTID(p PageID, slot uint16) TID { return TID(uint64(p)<<16 | uint64(slot)) }

// Page returns the page of a TID.
func (t TID) Page() PageID { return PageID(t >> 16) }

// Slot returns the slot of a TID.
func (t TID) Slot() uint16 { return uint16(t) }

// Heap page layout after the common header:
//
//	16 nslots u16
//	18 freeStart u16 (end of the slot array)
//	20 freeEnd u16   (start of tuple data)
//	24 next page u32
//	28 last page u32 (first page of a heap only)
//	40 slot array: (offset u16, length u16); offset 0 marks an unused slot
//
// Tuples are stored from the end of the page downwards.
const (
	offHeapNSlots    = 16
	offHeapFreeStart = 18
	offHeapFreeEnd   = 20
	offHeapNext      = 24
	offHeapLast      = 28
	heapSlots        = 40
	// MaxTuple is the largest tuple a heap page can hold.
	MaxTuple = PageSize - heapSlots - 4
)

// Tuple header, at the start of every tuple:
//
//	0  xmin u64   inserting transaction
//	8  xmax u64   deleting or locking transaction, 0 if none
//	16 cmin u32   command id of the insert within xmin
//	20 cmax u32   command id of the delete within xmax
//	24 flags u16
const (
	TupleHeaderSize = 26
	// FlagLockOnly marks an xmax that only locks the row (SELECT FOR UPDATE).
	FlagLockOnly uint16 = 1
	// FlagUpdated marks a tuple whose deleter replaced it with a new version.
	FlagUpdated uint16 = 2
)

// TupleHeader is the decoded MVCC header of a tuple.
type TupleHeader struct {
	Xmin, Xmax uint64
	Cmin, Cmax uint32
	Flags      uint16
}

// DecodeHeader decodes a tuple header.
func DecodeHeader(t []byte) TupleHeader {
	return TupleHeader{
		Xmin:  binary.LittleEndian.Uint64(t[0:]),
		Xmax:  binary.LittleEndian.Uint64(t[8:]),
		Cmin:  binary.LittleEndian.Uint32(t[16:]),
		Cmax:  binary.LittleEndian.Uint32(t[20:]),
		Flags: binary.LittleEndian.Uint16(t[24:]),
	}
}

// EncodeHeader writes a tuple header into t.
func EncodeHeader(t []byte, h TupleHeader) {
	binary.LittleEndian.PutUint64(t[0:], h.Xmin)
	binary.LittleEndian.PutUint64(t[8:], h.Xmax)
	binary.LittleEndian.PutUint32(t[16:], h.Cmin)
	binary.LittleEndian.PutUint32(t[20:], h.Cmax)
	binary.LittleEndian.PutUint16(t[24:], h.Flags)
}

// Heap is a table's chain of slotted pages.
type Heap struct {
	s     *Store
	first PageID

	mu   sync.Mutex // serializes extension; protects last and fsm
	last PageID
	fsm  []PageID // pages that may have free space (filled by vacuum)
}

func initHeapPage(p []byte) {
	p[offType] = PageHeap
	put16(p, offHeapNSlots, 0)
	put16(p, offHeapFreeStart, heapSlots)
	put16(p, offHeapFreeEnd, PageSize)
}

// CreateHeap allocates the first page of a new heap.
func (m *Mtr) CreateHeap() (PageID, error) {
	id, p, err := m.Alloc(PageHeap)
	if err != nil {
		return 0, err
	}
	initHeapPage(p)
	put32(p, offHeapLast, uint32(id))
	return id, nil
}

// Heap returns the heap whose first page is first.
func (s *Store) Heap(first PageID) *Heap {
	s.objMu.Lock()
	defer s.objMu.Unlock()
	h, ok := s.heaps[first]
	if !ok {
		h = &Heap{s: s, first: first}
		s.heaps[first] = h
	}
	return h
}

// First returns the first page.
func (h *Heap) First() PageID { return h.first }

func (h *Heap) lastPage() (PageID, error) {
	if h.last != InvalidPage {
		return h.last, nil
	}
	f, err := h.s.pool.Read(h.first)
	if err != nil {
		return 0, err
	}
	h.last = PageID(u32(f.Data, offHeapLast))
	h.s.pool.Release(f)
	if h.last == InvalidPage {
		h.last = h.first
	}
	return h.last, nil
}

func heapFree(p []byte) (space int, reuseSlot int) {
	free := int(u16(p, offHeapFreeEnd)) - int(u16(p, offHeapFreeStart))
	n := int(u16(p, offHeapNSlots))
	for i := 0; i < n; i++ {
		if u16(p, heapSlots+4*i) == 0 {
			return free, i
		}
	}
	return free - 4, -1
}

// tryInsert inserts into page p if it fits, returning the slot.
func tryInsert(p []byte, tuple []byte) (int, bool) {
	space, slot := heapFree(p)
	if space < len(tuple) {
		return 0, false
	}
	end := int(u16(p, offHeapFreeEnd)) - len(tuple)
	copy(p[end:], tuple)
	put16(p, offHeapFreeEnd, uint16(end))
	if slot < 0 {
		slot = int(u16(p, offHeapNSlots))
		put16(p, offHeapNSlots, uint16(slot+1))
		put16(p, offHeapFreeStart, uint16(heapSlots+4*(slot+1)))
	}
	put16(p, heapSlots+4*slot, uint16(end))
	put16(p, heapSlots+4*slot+2, uint16(len(tuple)))
	return slot, true
}

// Insert stores a tuple (header included) and returns its TID.
func (h *Heap) Insert(tuple []byte) (TID, error) {
	if len(tuple) > MaxTuple {
		return 0, pgerr.New(pgerr.ProgramLimitExceeded, "row is too big: size %d, maximum size %d", len(tuple), MaxTuple)
	}
	for attempt := 0; ; attempt++ {
		h.mu.Lock()
		last, err := h.lastPage()
		if err != nil {
			h.mu.Unlock()
			return 0, err
		}
		target := last
		fromFSM := false
		if len(h.fsm) > 0 {
			target = h.fsm[len(h.fsm)-1]
			fromFSM = true
		}
		h.mu.Unlock()

		m := h.s.Begin()
		p, err := m.Page(target)
		if err != nil {
			m.Abort()
			return 0, err
		}
		if slot, ok := tryInsert(p, tuple); ok {
			if _, err := m.Commit(); err != nil {
				return 0, err
			}
			return MakeTID(target, uint16(slot)), nil
		}
		m.Abort()
		if fromFSM {
			h.mu.Lock()
			if n := len(h.fsm); n > 0 && h.fsm[n-1] == target {
				h.fsm = h.fsm[:n-1]
			}
			h.mu.Unlock()
			continue
		}
		if tid, ok, err := h.extend(target, tuple); err != nil || ok {
			return tid, err
		}
	}
}

// extend appends a page after last and inserts the tuple there. It returns
// ok=false if another session extended the heap first (retry).
func (h *Heap) extend(last PageID, tuple []byte) (TID, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, err := h.lastPage(); err != nil || cur != last {
		return 0, false, err
	}
	m := h.s.Begin()
	lp, err := m.Page(last)
	if err != nil {
		m.Abort()
		return 0, false, err
	}
	// It may have gained space since we looked (vacuum); try once more.
	if slot, ok := tryInsert(lp, tuple); ok {
		if _, err := m.Commit(); err != nil {
			return 0, false, err
		}
		return MakeTID(last, uint16(slot)), true, nil
	}
	id, np, err := m.Alloc(PageHeap)
	if err != nil {
		m.Abort()
		return 0, false, err
	}
	initHeapPage(np)
	put32(lp, offHeapNext, uint32(id))
	fp, err := m.Page(h.first)
	if err != nil {
		m.Abort()
		return 0, false, err
	}
	put32(fp, offHeapLast, uint32(id))
	slot, _ := tryInsert(np, tuple)
	if _, err := m.Commit(); err != nil {
		return 0, false, err
	}
	h.last = id
	return MakeTID(id, uint16(slot)), true, nil
}

func slotTuple(p []byte, slot uint16) ([]byte, bool) {
	if int(slot) >= int(u16(p, offHeapNSlots)) {
		return nil, false
	}
	off := u16(p, heapSlots+4*int(slot))
	if off == 0 {
		return nil, false
	}
	l := u16(p, heapSlots+4*int(slot)+2)
	return p[off : int(off)+int(l)], true
}

// Fetch copies the tuple at tid into buf (reused if large enough).
func (h *Heap) Fetch(tid TID, buf []byte) ([]byte, bool, error) {
	f, err := h.s.pool.Read(tid.Page())
	if err != nil {
		return nil, false, err
	}
	defer h.s.pool.Release(f)
	if pageType(f.Data) != PageHeap {
		return nil, false, nil
	}
	t, ok := slotTuple(f.Data, tid.Slot())
	if !ok {
		return nil, false, nil
	}
	return append(buf[:0], t...), true, nil
}

// ErrRetry may be returned by a ModifyHeader callback to leave the tuple
// unchanged.
var ErrRetry = pgerr.New(pgerr.InternalError, "retry")

// ModifyHeader runs fn on the header of the tuple at tid under an exclusive
// page latch and logs any change. fn returns an error to abort the change.
func (h *Heap) ModifyHeader(tid TID, fn func(hdr *TupleHeader) error) (LSN, error) {
	m := h.s.Begin()
	p, err := m.Page(tid.Page())
	if err != nil {
		m.Abort()
		return 0, err
	}
	t, ok := slotTuple(p, tid.Slot())
	if !ok {
		m.Abort()
		return 0, pgerr.Internal("tuple %d/%d does not exist", tid.Page(), tid.Slot())
	}
	hdr := DecodeHeader(t)
	if err := fn(&hdr); err != nil {
		m.Abort()
		return 0, err
	}
	EncodeHeader(t, hdr)
	return m.Commit()
}

// HeapCursor scans a heap page by page. Each page is copied under a shared
// latch, so the scan never holds a latch while the caller works.
type HeapCursor struct {
	h    *Heap
	next PageID
	page []byte
	cur  PageID
	slot int
	n    int
}

// Scan starts a sequential scan.
func (h *Heap) Scan() *HeapCursor {
	return &HeapCursor{h: h, next: h.first, page: make([]byte, PageSize)}
}

// Next returns the next tuple (a slice into the cursor's page copy, valid
// until the following call).
func (c *HeapCursor) Next() (TID, []byte, bool, error) {
	for {
		for c.slot < c.n {
			s := c.slot
			c.slot++
			if t, ok := slotTuple(c.page, uint16(s)); ok {
				return MakeTID(c.cur, uint16(s)), t, true, nil
			}
		}
		if c.next == InvalidPage {
			return 0, nil, false, nil
		}
		f, err := c.h.s.pool.Read(c.next)
		if err != nil {
			return 0, nil, false, err
		}
		copy(c.page, f.Data)
		c.h.s.pool.Release(f)
		c.cur = c.next
		c.next = PageID(u32(c.page, offHeapNext))
		c.slot = 0
		c.n = int(u16(c.page, offHeapNSlots))
	}
}

// Pages returns the page chain of the heap.
func (h *Heap) Pages() ([]PageID, error) {
	var out []PageID
	for id := h.first; id != InvalidPage; {
		f, err := h.s.pool.Read(id)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
		id = PageID(u32(f.Data, offHeapNext))
		h.s.pool.Release(f)
	}
	return out, nil
}

// PageTuples copies the live slots of one page (used by VACUUM).
func (h *Heap) PageTuples(id PageID) (map[uint16][]byte, error) {
	f, err := h.s.pool.Read(id)
	if err != nil {
		return nil, err
	}
	defer h.s.pool.Release(f)
	out := map[uint16][]byte{}
	n := int(u16(f.Data, offHeapNSlots))
	for s := 0; s < n; s++ {
		if t, ok := slotTuple(f.Data, uint16(s)); ok {
			out[uint16(s)] = append([]byte(nil), t...)
		}
	}
	return out, nil
}

// RemoveTuples frees the given slots of a page (after their index entries
// have been deleted) if still is true for them under the page latch, and
// compacts the page. It returns the number of slots freed.
func (h *Heap) RemoveTuples(id PageID, slots []uint16, still func(t []byte) bool) (int, error) {
	m := h.s.Begin()
	p, err := m.Page(id)
	if err != nil {
		m.Abort()
		return 0, err
	}
	removed := 0
	for _, s := range slots {
		t, ok := slotTuple(p, s)
		if !ok || !still(t) {
			continue
		}
		put16(p, heapSlots+4*int(s), 0)
		put16(p, heapSlots+4*int(s)+2, 0)
		removed++
	}
	if removed > 0 {
		compactHeapPage(p)
	}
	if _, err := m.Commit(); err != nil {
		return 0, err
	}
	if removed > 0 {
		h.mu.Lock()
		h.fsm = append(h.fsm, id)
		h.mu.Unlock()
	}
	return removed, nil
}

// compactHeapPage moves tuples to the end of the page, removing holes, and
// trims trailing unused slots.
func compactHeapPage(p []byte) {
	n := int(u16(p, offHeapNSlots))
	for n > 0 && u16(p, heapSlots+4*(n-1)) == 0 {
		n--
	}
	put16(p, offHeapNSlots, uint16(n))
	put16(p, offHeapFreeStart, uint16(heapSlots+4*n))
	tmp := make([]byte, PageSize)
	end := PageSize
	for s := 0; s < n; s++ {
		off := u16(p, heapSlots+4*s)
		if off == 0 {
			continue
		}
		l := int(u16(p, heapSlots+4*s+2))
		end -= l
		copy(tmp[end:], p[off:int(off)+l])
		put16(p, heapSlots+4*s, uint16(end))
	}
	copy(p[end:], tmp[end:])
	clear(p[heapSlots+4*n : end])
	put16(p, offHeapFreeEnd, uint16(end))
}

// FreeHeap frees every page of a heap (DROP TABLE, TRUNCATE). Pages are
// freed in batches of separate mini-transactions; the caller has already
// removed the heap from the catalog, so a crash part-way only leaks pages.
func (s *Store) FreeHeap(first PageID) error {
	h := s.Heap(first)
	pages, err := h.Pages()
	if err != nil {
		return err
	}
	if err := s.freePages(pages); err != nil {
		return err
	}
	s.objMu.Lock()
	delete(s.heaps, first)
	s.objMu.Unlock()
	return nil
}

func (s *Store) freePages(pages []PageID) error {
	for len(pages) > 0 {
		n := min(len(pages), 32)
		m := s.Begin()
		for _, id := range pages[:n] {
			if err := m.Free(id); err != nil {
				m.Abort()
				return err
			}
		}
		if _, err := m.Commit(); err != nil {
			return err
		}
		pages = pages[n:]
	}
	return nil
}
