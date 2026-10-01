// Package txn implements transactions: transaction ids, snapshots and
// MVCC visibility (snapshot isolation), the in-memory mirror of the commit
// log, and the lock manager with deadlock detection.
package txn

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/storage"
)

// Transaction status values (mirroring storage).
const (
	InProgress = storage.XidInProgress
	Committed  = storage.XidCommitted
	Aborted    = storage.XidAborted
)

const (
	clogChunkWords = 4096
	clogChunkXids  = clogChunkWords * 16
	xidBatch       = 1024 // xids reserved durably at a time
)

type clogChunk [clogChunkWords]atomic.Uint32

// clogMirror is a lock-free in-memory copy of the commit log, two bits per
// transaction id, consulted on every visibility check.
type clogMirror struct {
	chunks atomic.Pointer[[]*clogChunk]
	grow   sync.Mutex
}

func (c *clogMirror) chunk(xid uint64, create bool) *clogChunk {
	idx := int(xid / clogChunkXids)
	cs := c.chunks.Load()
	if cs != nil && idx < len(*cs) {
		return (*cs)[idx]
	}
	if !create {
		return nil
	}
	c.grow.Lock()
	defer c.grow.Unlock()
	cs = c.chunks.Load()
	var cur []*clogChunk
	if cs != nil {
		cur = *cs
	}
	if idx < len(cur) {
		return cur[idx]
	}
	next := append([]*clogChunk(nil), cur...)
	for len(next) <= idx {
		next = append(next, new(clogChunk))
	}
	c.chunks.Store(&next)
	return next[idx]
}

func (c *clogMirror) get(xid uint64) byte {
	ch := c.chunk(xid, false)
	if ch == nil {
		return InProgress
	}
	i := xid % clogChunkXids
	return byte(ch[i/16].Load()>>((i%16)*2)) & 3
}

func (c *clogMirror) set(xid uint64, st byte) {
	ch := c.chunk(xid, true)
	i := xid % clogChunkXids
	w := &ch[i/16]
	shift := (i % 16) * 2
	for {
		old := w.Load()
		nw := old&^(3<<shift) | uint32(st)<<shift
		if w.CompareAndSwap(old, nw) {
			return
		}
	}
}

// Snapshot is the set of transactions whose effects a statement may see:
// every transaction that committed before the snapshot was taken.
type Snapshot struct {
	Xmin   uint64   // all xids below Xmin had finished
	Xmax   uint64   // xids at or above Xmax had not started
	Active []uint64 // sorted xids running when the snapshot was taken
}

// Running reports whether xid was still running (or not yet started) when
// the snapshot was taken.
func (s *Snapshot) Running(xid uint64) bool {
	if xid >= s.Xmax {
		return true
	}
	if xid < s.Xmin {
		return false
	}
	i := sort.Search(len(s.Active), func(i int) bool { return s.Active[i] >= xid })
	return i < len(s.Active) && s.Active[i] == xid
}

// Manager hands out transaction ids and snapshots.
type Manager struct {
	store *storage.Store
	clog  clogMirror
	Locks *LockManager

	mu       sync.Mutex
	nextXid  uint64
	reserved uint64
	startXid uint64
	nextVxid uint64
	active   map[uint64]*Txn // by xid
	running  map[uint64]*Txn // by vxid

	Commits, Aborts atomic.Uint64
}

// NewManager loads the commit log from the store.
func NewManager(store *storage.Store) (*Manager, error) {
	m := &Manager{store: store, Locks: NewLockManager(), active: map[uint64]*Txn{}, running: map[uint64]*Txn{}}
	raw, err := store.ReadClog()
	if err != nil {
		return nil, err
	}
	for i, b := range raw {
		if b == 0 {
			continue
		}
		for k := 0; k < 4; k++ {
			if st := (b >> (k * 2)) & 3; st != 0 {
				m.clog.set(uint64(i*4+k), st)
			}
		}
	}
	m.startXid = store.StartXid()
	m.nextXid = m.startXid
	m.reserved = m.startXid
	return m, nil
}

// Status returns the status of a transaction id. A transaction that was
// in progress when the server stopped is reported as aborted: its tuples
// stay on disk but are invisible to everyone (no undo pass is needed).
func (m *Manager) Status(xid uint64) byte {
	st := m.clog.get(xid)
	if st == InProgress && xid < m.startXid {
		return Aborted
	}
	return st
}

// Txn is a transaction. Its xid is assigned on the first write, so
// read-only transactions never touch the commit log.
type Txn struct {
	m        *Manager
	Vxid     uint64
	Xid      uint64
	Snap     *Snapshot
	Cid      uint32 // current command id
	ReadOnly bool
	done     bool
	// Ctx is the context of the current statement (cancellation).
	Ctx context.Context
}

// Begin starts a transaction.
func (m *Manager) Begin() *Txn {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextVxid++
	t := &Txn{m: m, Vxid: m.nextVxid, Ctx: context.Background()}
	m.running[t.Vxid] = t
	return t
}

// Manager returns the transaction manager.
func (t *Txn) Manager() *Manager { return t.m }

// Snapshot returns the transaction snapshot, taking it at first use
// (the first statement), as PostgreSQL's REPEATABLE READ does.
func (t *Txn) Snapshot() *Snapshot {
	if t.Snap != nil {
		return t.Snap
	}
	m := t.m
	m.mu.Lock()
	defer m.mu.Unlock()
	s := &Snapshot{Xmax: m.nextXid, Xmin: m.nextXid}
	for xid := range m.active {
		if xid == t.Xid {
			continue
		}
		s.Active = append(s.Active, xid)
		if xid < s.Xmin {
			s.Xmin = xid
		}
	}
	sort.Slice(s.Active, func(i, j int) bool { return s.Active[i] < s.Active[j] })
	t.Snap = s
	return s
}

// AssignXid gives the transaction an xid if it has none.
func (t *Txn) AssignXid() (uint64, error) {
	if t.Xid != 0 {
		return t.Xid, nil
	}
	if t.ReadOnly {
		return 0, pgerr.New(pgerr.ReadOnlySQLTransaction, "cannot execute this statement in a read-only transaction")
	}
	m := t.m
	m.mu.Lock()
	if m.nextXid >= m.reserved {
		upto := m.nextXid + xidBatch
		if err := m.store.ReserveXids(upto); err != nil {
			m.mu.Unlock()
			return 0, err
		}
		m.reserved = upto
	}
	xid := m.nextXid
	m.nextXid++
	m.active[xid] = t
	t.Xid = xid
	m.mu.Unlock()
	// Other transactions wait for this one by waiting on this lock.
	if err := m.Locks.Acquire(context.Background(), t.Vxid, Tag{Kind: TagXid, ID: xid}, Exclusive); err != nil {
		return 0, err
	}
	return xid, nil
}

// CommandCounterIncrement starts a new command: rows written by earlier
// commands of this transaction become visible.
func (t *Txn) CommandCounterIncrement() { t.Cid++ }

// Done reports whether the transaction has committed or aborted.
func (t *Txn) Done() bool { return t.done }

// Commit makes the transaction's writes durable and visible.
func (t *Txn) Commit() error {
	if t.done {
		return nil
	}
	t.done = true
	m := t.m
	if t.Xid != 0 {
		lsn, err := m.store.SetXidStatus(t.Xid, Committed)
		if err == nil {
			err = m.store.Flush(lsn)
		}
		if err != nil {
			m.clog.set(t.Xid, Aborted)
			m.finish(t)
			return pgerr.New(pgerr.IOError, "could not commit: %v", err)
		}
		m.clog.set(t.Xid, Committed)
		m.Commits.Add(1)
	}
	m.finish(t)
	return nil
}

// Abort rolls the transaction back. Its tuples remain until VACUUM but are
// invisible because its status is aborted.
func (t *Txn) Abort() {
	if t.done {
		return
	}
	t.done = true
	m := t.m
	if t.Xid != 0 {
		// No flush needed: if this record is lost the crash makes the
		// transaction aborted anyway.
		_, _ = m.store.SetXidStatus(t.Xid, Aborted)
		m.clog.set(t.Xid, Aborted)
		m.Aborts.Add(1)
	}
	m.finish(t)
}

func (m *Manager) finish(t *Txn) {
	m.mu.Lock()
	if t.Xid != 0 {
		delete(m.active, t.Xid)
	}
	delete(m.running, t.Vxid)
	m.mu.Unlock()
	m.Locks.ReleaseAll(t.Vxid)
}

// sees reports whether xid's effects are visible to the snapshot.
func (t *Txn) sees(xid uint64) bool {
	if t.Snap.Running(xid) {
		return false
	}
	return t.m.Status(xid) == Committed
}

// Visible applies the snapshot isolation visibility rules to a tuple.
func (t *Txn) Visible(h storage.TupleHeader) bool {
	if t.Snap == nil {
		t.Snapshot()
	}
	if h.Flags&storage.FlagKilled != 0 {
		return false
	}
	if h.Xmin == t.Xid && t.Xid != 0 {
		if h.Cmin >= t.Cid {
			return false // inserted by this or a later command
		}
	} else if !t.sees(h.Xmin) {
		return false
	}
	if h.Xmax == 0 || h.Flags&storage.FlagLockOnly != 0 {
		return true
	}
	if h.Xmax == t.Xid && t.Xid != 0 {
		return h.Cmax >= t.Cid // deleted by a later command: still visible
	}
	return !t.sees(h.Xmax)
}

// WaitFor blocks until transaction xid has finished. It participates in
// deadlock detection.
func (t *Txn) WaitFor(xid uint64) error {
	m := t.m
	m.mu.Lock()
	_, running := m.active[xid]
	m.mu.Unlock()
	if !running {
		return nil
	}
	tag := Tag{Kind: TagXid, ID: xid}
	if err := m.Locks.Acquire(t.Ctx, t.Vxid, tag, Share); err != nil {
		return err
	}
	m.Locks.Release(t.Vxid, tag)
	return nil
}

// Lock takes a lock held until the end of the transaction.
func (t *Txn) Lock(tag Tag, mode Mode) error {
	return t.m.Locks.Acquire(t.Ctx, t.Vxid, tag, mode)
}

// IsActive reports whether xid is a running transaction.
func (m *Manager) IsActive(xid uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.active[xid]
	return ok
}

// OldestXmin returns the oldest xid that some running transaction might
// still need to see as "not yet committed"; tuple versions deleted by
// transactions that committed before it can be removed by VACUUM.
func (m *Manager) OldestXmin() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	oldest := m.nextXid
	for _, t := range m.running {
		if t.Snap != nil && t.Snap.Xmin < oldest {
			oldest = t.Snap.Xmin
		}
		if t.Xid != 0 && t.Xid < oldest {
			oldest = t.Xid
		}
	}
	return oldest
}

// NextXid returns the next xid to be assigned.
func (m *Manager) NextXid() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextXid
}

// ActiveCount returns the number of transactions with an xid.
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

// Store returns the storage engine.
func (m *Manager) Store() *storage.Store { return m.store }
