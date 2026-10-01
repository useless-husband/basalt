// Package engine ties the storage engine, transactions, catalog, planner
// and executor together into a database with sessions. The wire protocol
// server and the embedded tests both talk to it.
package engine

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/storage"
	"github.com/useless-husband/basalt/internal/txn"
	"github.com/useless-husband/basalt/internal/vfs"
)

// Version is basalt's version string.
const Version = "0.1.0"

// ServerVersion is the PostgreSQL version basalt reports to clients.
const ServerVersion = "16.4"

// ServerVersionNum is ServerVersion as a number.
const ServerVersionNum = "160004"

// Options configures a database.
type Options struct {
	Dir                string
	FS                 vfs.FS
	PoolPages          int
	CheckpointInterval time.Duration
	// AutovacuumInterval enables background VACUUM (0 disables).
	AutovacuumInterval time.Duration
}

// DB is an open database.
type DB struct {
	opts   Options
	store  *storage.Store
	txns   *txn.Manager
	cat    atomic.Pointer[catalog.Catalog]
	ddlMu  sync.Mutex
	seqMu  sync.Mutex
	seqs   map[uint32]*seqState
	vacMu  sync.Mutex
	start  time.Time
	pids   atomic.Int64
	stop   chan struct{}
	wg     sync.WaitGroup
	closed atomic.Bool

	sessMu   sync.Mutex
	sessions map[int64]*Session

	changes sync.Map // table oid -> *tableChanges
}

type tableChanges struct {
	inserted, deleted atomic.Int64
}

type seqState struct {
	mu     sync.Mutex
	loaded bool
	next   int64 // next value to hand out
	bound  int64 // values below bound are covered by the persisted state
	called bool
}

// Open opens or creates a database and runs recovery.
func Open(opts Options) (*DB, error) {
	if opts.FS == nil {
		opts.FS = vfs.OS{}
	}
	st, err := storage.Open(storage.Options{FS: opts.FS, Dir: opts.Dir, PoolPages: opts.PoolPages, CheckpointInterval: opts.CheckpointInterval})
	if err != nil {
		return nil, err
	}
	tm, err := txn.NewManager(st)
	if err != nil {
		st.Close()
		return nil, err
	}
	db := &DB{opts: opts, store: st, txns: tm, seqs: map[uint32]*seqState{}, start: time.Now(),
		stop: make(chan struct{}), sessions: map[int64]*Session{}}
	db.pids.Store(int64(10000))
	root, err := st.CatalogRoot()
	if err != nil {
		st.Close()
		return nil, err
	}
	cat := catalog.New()
	if root != storage.InvalidPage {
		b, err := st.ReadBlob(root)
		if err != nil {
			st.Close()
			return nil, fmt.Errorf("reading catalog: %w", err)
		}
		if cat, err = catalog.Decode(b); err != nil {
			st.Close()
			return nil, fmt.Errorf("decoding catalog: %w", err)
		}
	}
	db.cat.Store(cat)
	if opts.AutovacuumInterval > 0 {
		db.wg.Add(1)
		go db.autovacuum(opts.AutovacuumInterval)
	}
	return db, nil
}

// Close checkpoints and closes the database.
func (db *DB) Close() error {
	if !db.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(db.stop)
	db.wg.Wait()
	return db.store.Close()
}

// Abandon stops the database without a final checkpoint (tests).
func (db *DB) Abandon() {
	if !db.closed.CompareAndSwap(false, true) {
		return
	}
	close(db.stop)
	db.wg.Wait()
	db.store.Abandon()
}

// Catalog returns the current catalog version.
func (db *DB) Catalog() *catalog.Catalog { return db.cat.Load() }

// Store returns the storage engine.
func (db *DB) Store() *storage.Store { return db.store }

// Txns returns the transaction manager.
func (db *DB) Txns() *txn.Manager { return db.txns }

// heapPages returns the number of pages of a table's heap.
func (db *DB) heapPages(t *catalog.Table) int {
	return db.store.Heap(t.Heap).PageCount()
}

// saveCatalog writes cat inside the mini-transaction m (which may also
// carry the storage changes of the DDL) and publishes it after commit.
// The caller holds ddlMu.
func (db *DB) saveCatalog(m *storage.Mtr, cat *catalog.Catalog) error {
	cat.Version = db.Catalog().Version + 1
	b, err := cat.Encode()
	if err != nil {
		m.Abort()
		return err
	}
	old, err := m.CatalogRoot()
	if err != nil {
		m.Abort()
		return err
	}
	root, err := m.WriteBlob(b)
	if err != nil {
		m.Abort()
		return err
	}
	if err := m.SetCatalogRoot(root); err != nil {
		m.Abort()
		return err
	}
	if old != storage.InvalidPage {
		if err := m.FreeBlob(old); err != nil {
			m.Abort()
			return err
		}
	}
	lsn, err := m.Commit()
	if err != nil {
		return err
	}
	if err := db.store.Flush(lsn); err != nil {
		return err
	}
	db.cat.Store(cat)
	return nil
}

// ---- sequences ----

const seqCache = 32

func (db *DB) sequence(name string) (*catalog.Sequence, *seqState, error) {
	cat := db.Catalog()
	s := cat.SequenceByName(name)
	if s == nil {
		return nil, nil, pgerr.New(pgerr.UndefinedTable, "relation \"%s\" does not exist", name)
	}
	db.seqMu.Lock()
	st := db.seqs[s.Oid]
	if st == nil {
		st = &seqState{}
		db.seqs[s.Oid] = st
	}
	db.seqMu.Unlock()
	st.mu.Lock()
	if !st.loaded {
		v, err := db.store.ReadSeq(s.Page)
		if err != nil {
			st.mu.Unlock()
			return nil, nil, err
		}
		// The page holds the first value not yet covered by a persisted
		// reservation; after a crash the cached values are skipped.
		st.next, st.bound, st.loaded = v, v, true
	}
	st.mu.Unlock()
	return s, st, nil
}

// Nextval advances a sequence. Values are reserved in batches that are
// logged (not flushed): a crash may skip values but never repeats one that
// a committed transaction used, because its commit flushed the log past
// the reservation.
func (db *DB) Nextval(name string) (int64, error) {
	s, st, err := db.sequence(name)
	if err != nil {
		return 0, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	v := st.next
	if (s.Increment > 0 && v >= st.bound) || (s.Increment < 0 && v <= st.bound) {
		nb := v + seqCache*s.Increment
		m := db.store.Begin()
		if err := m.SetSeq(s.Page, nb); err != nil {
			m.Abort()
			return 0, err
		}
		if _, err := m.Commit(); err != nil {
			return 0, err
		}
		st.bound = nb
	}
	st.next = v + s.Increment
	st.called = true
	return v, nil
}

// Setval sets a sequence's next value.
func (db *DB) Setval(name string, v int64, isCalled bool) (int64, error) {
	s, st, err := db.sequence(name)
	if err != nil {
		return 0, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	next := v
	if isCalled {
		next = v + s.Increment
	}
	m := db.store.Begin()
	if err := m.SetSeq(s.Page, next); err != nil {
		m.Abort()
		return 0, err
	}
	if _, err := m.Commit(); err != nil {
		return 0, err
	}
	st.next, st.bound, st.called = next, next, isCalled
	return v, nil
}

func (db *DB) noteChange(oid uint32, ins, del int64) {
	v, _ := db.changes.LoadOrStore(oid, &tableChanges{})
	c := v.(*tableChanges)
	c.inserted.Add(ins)
	c.deleted.Add(del)
}

// Checkpoint forces a checkpoint.
func (db *DB) Checkpoint() error { return db.store.Checkpoint() }

func (db *DB) register(s *Session) {
	db.sessMu.Lock()
	db.sessions[s.pid] = s
	db.sessMu.Unlock()
}

func (db *DB) unregister(s *Session) {
	db.sessMu.Lock()
	delete(db.sessions, s.pid)
	db.sessMu.Unlock()
}

// Cancel requests cancellation of the statement running in the session
// with the given backend key.
func (db *DB) Cancel(pid int64, secret int32) bool {
	db.sessMu.Lock()
	s := db.sessions[pid]
	db.sessMu.Unlock()
	if s == nil || s.secret != secret {
		return false
	}
	s.cancelRequested.Store(true)
	return true
}
