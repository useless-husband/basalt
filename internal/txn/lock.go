package txn

import (
	"context"
	"fmt"
	"sync"

	"github.com/useless-husband/basalt/internal/pgerr"
)

// Mode is a lock mode. The set and the conflict table are a subset of
// PostgreSQL's table-level lock modes.
type Mode uint8

const (
	// AccessShare is taken by SELECT.
	AccessShare Mode = iota + 1
	// RowExclusive is taken by INSERT, UPDATE and DELETE.
	RowExclusive
	// Share is taken by CREATE INDEX (blocks writers, not readers) and by
	// foreign key checks on the referenced key.
	Share
	// Exclusive is taken on a referenced key by DELETE/UPDATE of the
	// parent row, and by a transaction on its own transaction id.
	Exclusive
	// AccessExclusive is taken by DROP, TRUNCATE, ALTER and VACUUM FULL.
	AccessExclusive
	numModes
)

func (m Mode) String() string {
	return [...]string{"", "AccessShareLock", "RowExclusiveLock", "ShareLock", "ExclusiveLock", "AccessExclusiveLock"}[m]
}

func bit(m Mode) uint8 { return 1 << m }

var conflictTable = [numModes]uint8{
	AccessShare:     bit(AccessExclusive),
	RowExclusive:    bit(Share) | bit(Exclusive) | bit(AccessExclusive),
	Share:           bit(RowExclusive) | bit(Exclusive) | bit(AccessExclusive),
	Exclusive:       bit(RowExclusive) | bit(Share) | bit(Exclusive) | bit(AccessExclusive),
	AccessExclusive: bit(AccessShare) | bit(RowExclusive) | bit(Share) | bit(Exclusive) | bit(AccessExclusive),
}

// Conflicts reports whether a and b conflict.
func Conflicts(a, b Mode) bool { return conflictTable[a]&bit(b) != 0 }

// TagKind distinguishes lockable object kinds.
type TagKind uint8

const (
	// TagXid locks a transaction id; waiting on it waits for the
	// transaction to finish.
	TagXid TagKind = iota + 1
	// TagRelation locks a table or index by OID.
	TagRelation
	// TagKey locks a key value in an index (foreign key checks).
	TagKey
)

// Tag names a lockable object.
type Tag struct {
	Kind TagKind
	ID   uint64
	Key  string
}

func (t Tag) String() string {
	switch t.Kind {
	case TagXid:
		return fmt.Sprintf("transaction %d", t.ID)
	case TagRelation:
		return fmt.Sprintf("relation %d", t.ID)
	}
	return fmt.Sprintf("key %q of relation %d", t.Key, t.ID)
}

type waiter struct {
	owner uint64
	mode  Mode
	tag   Tag
	ready chan error
}

type lockEntry struct {
	holders map[uint64]uint8 // owner -> bit set of modes held
	queue   []*waiter
}

// LockManager grants locks held until the end of a transaction and
// detects deadlocks. Owners are virtual transaction ids, which every
// transaction has (read-only ones included).
//
// Deadlock detection runs when a request has to wait: the wait-for graph
// (waiter -> holders and earlier queued requests it conflicts with) is
// searched from the requester, and if it leads back to the requester the
// request fails with SQLSTATE 40P01. The requester is the victim, which
// is cheap to decide and what the client that closed the cycle expects.
type LockManager struct {
	mu      sync.Mutex
	locks   map[Tag]*lockEntry
	held    map[uint64]map[Tag]struct{}
	waiting map[uint64]*waiter

	Deadlocks uint64
}

// NewLockManager returns an empty lock manager.
func NewLockManager() *LockManager {
	return &LockManager{locks: map[Tag]*lockEntry{}, held: map[uint64]map[Tag]struct{}{}, waiting: map[uint64]*waiter{}}
}

func (lm *LockManager) grantable(e *lockEntry, owner uint64, mode Mode, checkQueue bool) bool {
	for h, modes := range e.holders {
		if h == owner {
			continue
		}
		if conflictTable[mode]&modes != 0 {
			return false
		}
	}
	if checkQueue {
		for _, w := range e.queue {
			if w.owner != owner && Conflicts(mode, w.mode) {
				return false
			}
		}
	}
	return true
}

func (lm *LockManager) grant(e *lockEntry, tag Tag, owner uint64, mode Mode) {
	e.holders[owner] |= bit(mode)
	h := lm.held[owner]
	if h == nil {
		h = map[Tag]struct{}{}
		lm.held[owner] = h
	}
	h[tag] = struct{}{}
}

// Acquire takes a lock, waiting if necessary.
func (lm *LockManager) Acquire(ctx context.Context, owner uint64, tag Tag, mode Mode) error {
	lm.mu.Lock()
	e := lm.locks[tag]
	if e == nil {
		e = &lockEntry{holders: map[uint64]uint8{}}
		lm.locks[tag] = e
	}
	if e.holders[owner]&bit(mode) != 0 {
		lm.mu.Unlock()
		return nil
	}
	_, upgrading := e.holders[owner]
	if lm.grantable(e, owner, mode, !upgrading) {
		lm.grant(e, tag, owner, mode)
		lm.mu.Unlock()
		return nil
	}
	w := &waiter{owner: owner, mode: mode, tag: tag, ready: make(chan error, 1)}
	e.queue = append(e.queue, w)
	lm.waiting[owner] = w
	if lm.cycleFrom(owner) {
		lm.removeWaiter(e, w)
		lm.Deadlocks++
		lm.mu.Unlock()
		return pgerr.New(pgerr.DeadlockDetected, "deadlock detected").
			WithDetail("Process %d waits for %s on %s; the wait would close a cycle.", owner, mode, tag).
			WithHint("See server log for query details.")
	}
	lm.mu.Unlock()
	select {
	case err := <-w.ready:
		return err
	case <-ctx.Done():
		lm.mu.Lock()
		defer lm.mu.Unlock()
		select {
		case err := <-w.ready:
			// Granted just before we gave up; keep it (released at end).
			return err
		default:
		}
		lm.removeWaiter(e, w)
		lm.wake(e, tag)
		return ctxError(ctx)
	}
}

func ctxError(ctx context.Context) error {
	if err, ok := context.Cause(ctx).(*pgerr.Error); ok {
		return err
	}
	return pgerr.New(pgerr.QueryCanceled, "canceling statement due to user request")
}

func (lm *LockManager) removeWaiter(e *lockEntry, w *waiter) {
	for i, q := range e.queue {
		if q == w {
			e.queue = append(e.queue[:i], e.queue[i+1:]...)
			break
		}
	}
	if lm.waiting[w.owner] == w {
		delete(lm.waiting, w.owner)
	}
}

// wake grants queued requests in order while they are compatible.
func (lm *LockManager) wake(e *lockEntry, tag Tag) {
	for len(e.queue) > 0 {
		w := e.queue[0]
		if !lm.grantable(e, w.owner, w.mode, false) {
			break
		}
		e.queue = e.queue[1:]
		delete(lm.waiting, w.owner)
		lm.grant(e, tag, w.owner, w.mode)
		w.ready <- nil
	}
	if len(e.holders) == 0 && len(e.queue) == 0 {
		delete(lm.locks, tag)
	}
}

// cycleFrom reports whether the wait-for graph has a path from start back
// to start. Called with mu held.
func (lm *LockManager) cycleFrom(start uint64) bool {
	visited := map[uint64]bool{}
	var visit func(o uint64) bool
	visit = func(o uint64) bool {
		w := lm.waiting[o]
		if w == nil {
			return false
		}
		e := lm.locks[w.tag]
		if e == nil {
			return false
		}
		var next []uint64
		for h, modes := range e.holders {
			if h != o && conflictTable[w.mode]&modes != 0 {
				next = append(next, h)
			}
		}
		for _, q := range e.queue {
			if q == w {
				break
			}
			if q.owner != o && Conflicts(w.mode, q.mode) {
				next = append(next, q.owner)
			}
		}
		for _, n := range next {
			if n == start {
				return true
			}
			if visited[n] {
				continue
			}
			visited[n] = true
			if visit(n) {
				return true
			}
		}
		return false
	}
	return visit(start)
}

// Release drops one lock held by owner (all modes).
func (lm *LockManager) Release(owner uint64, tag Tag) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.releaseLocked(owner, tag)
	if h := lm.held[owner]; h != nil {
		delete(h, tag)
		if len(h) == 0 {
			delete(lm.held, owner)
		}
	}
}

func (lm *LockManager) releaseLocked(owner uint64, tag Tag) {
	e := lm.locks[tag]
	if e == nil {
		return
	}
	delete(e.holders, owner)
	lm.wake(e, tag)
}

// ReleaseAll drops every lock held by owner.
func (lm *LockManager) ReleaseAll(owner uint64) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	for tag := range lm.held[owner] {
		lm.releaseLocked(owner, tag)
	}
	delete(lm.held, owner)
}

// Holds reports whether owner holds tag in a mode at least as strong as
// mode (exact mode bit).
func (lm *LockManager) Holds(owner uint64, tag Tag, mode Mode) bool {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	e := lm.locks[tag]
	return e != nil && e.holders[owner]&bit(mode) != 0
}

// LockInfo describes a granted or awaited lock, for pg_locks.
type LockInfo struct {
	Owner   uint64
	Tag     Tag
	Mode    Mode
	Granted bool
}

// Snapshot lists all locks.
func (lm *LockManager) Snapshot() []LockInfo {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	var out []LockInfo
	for tag, e := range lm.locks {
		for owner, modes := range e.holders {
			for m := AccessShare; m < numModes; m++ {
				if modes&bit(m) != 0 {
					out = append(out, LockInfo{owner, tag, m, true})
				}
			}
		}
		for _, w := range e.queue {
			out = append(out, LockInfo{w.owner, tag, w.mode, false})
		}
	}
	return out
}
