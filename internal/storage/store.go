package storage

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sync"
	"sync/atomic"
	"time"

	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/vfs"
)

// Meta page layout (page 0), after the common header.
const (
	metaMagic      = 0x544C5342 // "BSLT"
	metaVersion    = 1
	offMetaMagic   = 16
	offMetaVersion = 20
	offMetaPages   = 24
	offMetaFree    = 28
	offMetaCatalog = 32
	offMetaNextXid = 40
	offMetaNextOid = 48
	offMetaClogN   = 52
	offMetaClogDir = 56
	maxClogPages   = (PageSize - offMetaClogDir) / 4
	freeNextOffset = pageHeader // a free page stores the next free page here
)

// Options configures a Store.
type Options struct {
	FS  vfs.FS
	Dir string
	// PoolPages is the number of buffer pool frames (default 16384, 128 MiB).
	PoolPages int
	// CheckpointInterval triggers a checkpoint periodically (default 30s;
	// negative disables automatic checkpoints).
	CheckpointInterval time.Duration
	// CheckpointWALBytes triggers a checkpoint when this much WAL has been
	// written since the last one (default 512 MiB).
	CheckpointWALBytes int64
}

// Store is an open database file with its WAL.
type Store struct {
	opts Options
	fs   vfs.FS
	file vfs.File
	wal  *WAL
	pool *Pool
	ctl  *control

	// ckpt is held shared by mini-transactions and exclusively by
	// checkpoints.
	ckpt    sync.RWMutex
	redoLSN LSN // pages with LSN <= redoLSN need a full image when next changed

	walSinceCkpt   atomic.Int64
	recoveredPages PageID

	startXid uint64 // first xid handed out in this run

	clog   clogDir
	objMu  sync.Mutex
	heaps  map[PageID]*Heap
	btrees map[PageID]*BTree

	stop             chan struct{}
	stopped          sync.WaitGroup
	closed           atomic.Bool
	ckptMu           sync.Mutex // serializes checkpoints
	Checkpoints      atomic.Uint64
	RecoveredRecords int
}

func dataPath(dir string) string { return vfs.Join(dir, "basalt.db") }
func walDir(dir string) string   { return vfs.Join(dir, "wal") }

// Open opens (creating if necessary) the database in opts.Dir and runs
// crash recovery.
func Open(opts Options) (*Store, error) {
	if opts.FS == nil {
		opts.FS = vfs.OS{}
	}
	if opts.PoolPages <= 0 {
		opts.PoolPages = 16384
	}
	if opts.PoolPages < 64 {
		opts.PoolPages = 64
	}
	if opts.CheckpointInterval == 0 {
		opts.CheckpointInterval = 30 * time.Second
	}
	if opts.CheckpointWALBytes <= 0 {
		opts.CheckpointWALBytes = 512 << 20
	}
	fs := opts.FS
	if err := fs.MkdirAll(opts.Dir); err != nil {
		return nil, err
	}
	if err := fs.MkdirAll(walDir(opts.Dir)); err != nil {
		return nil, err
	}
	ctl, err := openControl(fs, vfs.Join(opts.Dir, "basalt.control"))
	if err != nil {
		return nil, err
	}
	file, err := fs.OpenFile(dataPath(opts.Dir), true)
	if err != nil {
		return nil, err
	}
	s := &Store{opts: opts, fs: fs, file: file, ctl: ctl, stop: make(chan struct{}),
		heaps: map[PageID]*Heap{}, btrees: map[PageID]*BTree{}}
	fresh := ctl.seq == 0

	if fresh {
		if err := s.create(); err != nil {
			file.Close()
			return nil, err
		}
	} else if err := s.recover(); err != nil {
		file.Close()
		return nil, err
	}
	if opts.CheckpointInterval > 0 {
		s.stopped.Add(1)
		go s.checkpointer()
	}
	return s, nil
}

// create initialises a new database: the meta page and a first checkpoint.
func (s *Store) create() error {
	w, err := openWAL(s.fs, walDir(s.opts.Dir), 1<<20)
	if err != nil {
		return err
	}
	s.wal = w
	s.pool = newPool(s.file, w, s.opts.PoolPages)
	s.redoLSN = 1 << 20
	m := s.Begin()
	meta, err := m.fresh(0)
	if err != nil {
		m.Abort()
		return err
	}
	meta[offType] = PageMeta
	put32(meta, offMetaMagic, metaMagic)
	put32(meta, offMetaVersion, metaVersion)
	put32(meta, offMetaPages, 1)
	put64(meta, offMetaNextXid, 3) // xids 1 and 2 are reserved
	put32(meta, offMetaNextOid, 16384)
	if _, err := m.Commit(); err != nil {
		return err
	}
	s.startXid = 3
	return s.Checkpoint()
}

// recover replays the WAL from the last checkpoint.
func (s *Store) recover() error {
	redo := s.ctl.redoLSN
	// The pool needs the WAL only for flushing; recovery replays first and
	// opens the log for appending afterwards.
	s.pool = newPool(s.file, nil, s.opts.PoolPages)
	n := 0
	end, err := readWAL(s.fs, walDir(s.opts.Dir), redo, func(r walRecord) error {
		n++
		switch r.typ {
		case RecPages:
			return s.applyPagesRecord(r)
		case RecNoop:
			return nil
		}
		return fmt.Errorf("unknown WAL record type %d at %d", r.typ, r.lsn)
	})
	if err != nil {
		return fmt.Errorf("recovery failed: %w", err)
	}
	s.RecoveredRecords = n
	w, err := openWAL(s.fs, walDir(s.opts.Dir), end)
	if err != nil {
		return err
	}
	s.wal = w
	s.pool.wal = w
	s.redoLSN = redo
	f, err := s.pool.Read(0)
	if err != nil {
		return fmt.Errorf("reading meta page: %w", err)
	}
	ok := u32(f.Data, offMetaMagic) == metaMagic
	s.startXid = u64(f.Data, offMetaNextXid)
	s.pool.Release(f)
	if !ok {
		return pgerr.New(pgerr.DataCorrupted, "%s is not a basalt database", dataPath(s.opts.Dir))
	}
	// Make the recovered state durable and start a fresh log segment.
	return s.Checkpoint()
}

// Checkpoint flushes every dirty page and records a new redo start point.
// It is a sharp checkpoint: mini-transactions are paused while it runs.
func (s *Store) Checkpoint() error {
	s.ckptMu.Lock()
	defer s.ckptMu.Unlock()
	s.ckpt.Lock()
	defer s.ckpt.Unlock()
	if err := s.wal.Err(); err != nil {
		return err
	}
	// All appended records are complete (no mtr is running). Start a new
	// segment at the end of the log; it becomes the redo point.
	lsn, err := s.wal.startSegment()
	if err != nil {
		return err
	}
	if err := s.pool.flushAll(); err != nil {
		return err
	}
	if err := s.file.Sync(); err != nil {
		return err
	}
	if err := s.ctl.write(lsn); err != nil {
		return err
	}
	s.redoLSN = lsn
	s.walSinceCkpt.Store(0)
	s.Checkpoints.Add(1)
	return s.wal.removeBefore(lsn)
}

func (s *Store) checkpointer() {
	defer s.stopped.Done()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
			due := s.walSinceCkpt.Load() >= s.opts.CheckpointWALBytes ||
				(time.Since(last) >= s.opts.CheckpointInterval && s.walSinceCkpt.Load() > 0)
			if due {
				if err := s.Checkpoint(); err == nil {
					last = time.Now()
				}
			}
		}
	}
}

// Close checkpoints and closes the store.
func (s *Store) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(s.stop)
	s.stopped.Wait()
	err := s.Checkpoint()
	if e := s.wal.close(); err == nil {
		err = e
	}
	if e := s.file.Close(); err == nil {
		err = e
	}
	return err
}

// Abandon stops background work without flushing anything, simulating a
// process that was killed. Used by crash tests.
func (s *Store) Abandon() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	close(s.stop)
	s.stopped.Wait()
}

// WAL returns the write-ahead log.
func (s *Store) WAL() *WAL { return s.wal }

// Pool returns the buffer pool.
func (s *Store) Pool() *Pool { return s.pool }

// StartXid returns the first transaction id that may be used in this run.
// Every id below it that is not marked committed belonged to a transaction
// that was running at the time of the crash and is treated as aborted.
func (s *Store) StartXid() uint64 { return s.startXid }

// Flush makes the WAL durable up to lsn (commit).
func (s *Store) Flush(lsn LSN) error { return s.wal.Flush(lsn) }

// ---- page allocation ----

// Alloc allocates a page, from the free list if possible, and returns it
// zeroed and latched in the mini-transaction.
func (m *Mtr) Alloc(typ byte) (PageID, []byte, error) {
	meta, err := m.Page(0)
	if err != nil {
		return 0, nil, err
	}
	var id PageID
	if head := PageID(u32(meta, offMetaFree)); head != InvalidPage {
		p, err := m.Page(head)
		if err != nil {
			return 0, nil, err
		}
		if pageType(p) != PageFree {
			return 0, nil, pgerr.New(pgerr.DataCorrupted, "free list page %d has type %d", head, pageType(p))
		}
		put32(meta, offMetaFree, u32(p, freeNextOffset))
		id = head
	} else {
		id = PageID(u32(meta, offMetaPages))
		if id == ^PageID(0)-1 {
			return 0, nil, pgerr.New(pgerr.ProgramLimitExceeded, "database file is full")
		}
		put32(meta, offMetaPages, uint32(id)+1)
	}
	p, err := m.fresh(id)
	if err != nil {
		return 0, nil, err
	}
	p[offType] = typ
	return id, p, nil
}

// Free returns a page to the free list.
func (m *Mtr) Free(id PageID) error {
	if id == InvalidPage {
		return pgerr.Internal("attempt to free the meta page")
	}
	// Latch the page before the meta page: the meta page is always the
	// last page a mini-transaction latches (apart from fresh pages).
	p, err := m.Page(id)
	if err != nil {
		return err
	}
	meta, err := m.Page(0)
	if err != nil {
		return err
	}
	clear(p[pageHeader:])
	p[offType] = PageFree
	put32(p, freeNextOffset, u32(meta, offMetaFree))
	put32(meta, offMetaFree, uint32(id))
	return nil
}

// ---- meta fields ----

func (s *Store) readMeta(fn func(meta []byte)) error {
	f, err := s.pool.Read(0)
	if err != nil {
		return err
	}
	fn(f.Data)
	s.pool.Release(f)
	return nil
}

// PageCount returns the number of pages in the data file.
func (s *Store) PageCount() (n uint32) {
	_ = s.readMeta(func(m []byte) { n = u32(m, offMetaPages) })
	return
}

// FreePages returns the length of the free list.
func (s *Store) FreePages() (int, error) {
	n := 0
	var head PageID
	if err := s.readMeta(func(m []byte) { head = PageID(u32(m, offMetaFree)) }); err != nil {
		return 0, err
	}
	for head != InvalidPage {
		f, err := s.pool.Read(head)
		if err != nil {
			return 0, err
		}
		head = PageID(u32(f.Data, freeNextOffset))
		s.pool.Release(f)
		n++
	}
	return n, nil
}

// ReserveXids durably raises the xid high-water mark to upto, so that after
// a crash no xid below it is ever handed out again.
func (s *Store) ReserveXids(upto uint64) error {
	m := s.Begin()
	meta, err := m.Page(0)
	if err != nil {
		m.Abort()
		return err
	}
	if u64(meta, offMetaNextXid) < upto {
		put64(meta, offMetaNextXid, upto)
	}
	_, err = m.Commit()
	return err
}

// NextOid allocates n object ids.
func (m *Mtr) NextOid() (uint32, error) {
	meta, err := m.Page(0)
	if err != nil {
		return 0, err
	}
	oid := u32(meta, offMetaNextOid)
	put32(meta, offMetaNextOid, oid+1)
	return oid, nil
}

// CatalogRoot returns the first page of the serialized catalog.
func (s *Store) CatalogRoot() (root PageID, err error) {
	err = s.readMeta(func(m []byte) { root = PageID(u32(m, offMetaCatalog)) })
	return
}

// SetCatalogRoot updates the catalog pointer in the meta page.
func (m *Mtr) SetCatalogRoot(root PageID) error {
	meta, err := m.Page(0)
	if err != nil {
		return err
	}
	put32(meta, offMetaCatalog, uint32(root))
	return nil
}

// ---- control file ----

// control is a tiny file holding the redo start LSN of the last checkpoint.
// It has two 512-byte slots written alternately, each with a sequence
// number and a checksum, so a torn write can never destroy both.
type control struct {
	f       vfs.File
	seq     uint64
	redoLSN LSN
}

const ctlMagic = 0x54435342 // "BSCT"

func openControl(fs vfs.FS, path string) (*control, error) {
	f, err := fs.OpenFile(path, true)
	if err != nil {
		return nil, err
	}
	c := &control{f: f}
	buf := make([]byte, 1024)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return nil, err
	}
	for slot := 0; slot < 2; slot++ {
		b := buf[slot*512:]
		if binary.LittleEndian.Uint32(b) != ctlMagic {
			continue
		}
		if crc32.Checksum(b[:20], castagnoli) != binary.LittleEndian.Uint32(b[20:]) {
			continue
		}
		seq := binary.LittleEndian.Uint64(b[4:])
		if seq > c.seq {
			c.seq = seq
			c.redoLSN = LSN(binary.LittleEndian.Uint64(b[12:]))
		}
	}
	return c, nil
}

func (c *control) write(redo LSN) error {
	seq := c.seq + 1
	b := make([]byte, 512)
	binary.LittleEndian.PutUint32(b, ctlMagic)
	binary.LittleEndian.PutUint64(b[4:], seq)
	binary.LittleEndian.PutUint64(b[12:], uint64(redo))
	binary.LittleEndian.PutUint32(b[20:], crc32.Checksum(b[:20], castagnoli))
	if _, err := c.f.WriteAt(b, int64(seq%2)*512); err != nil {
		return err
	}
	if err := c.f.Sync(); err != nil {
		return err
	}
	c.seq, c.redoLSN = seq, redo
	return nil
}

// IsClosed reports whether Close or Abandon was called.
func (s *Store) IsClosed() bool { return s.closed.Load() }

// CatalogRoot reads the catalog pointer through the mini-transaction (the
// meta page may already be latched by it).
func (m *Mtr) CatalogRoot() (PageID, error) {
	meta, err := m.Page(0)
	if err != nil {
		return 0, err
	}
	return PageID(u32(meta, offMetaCatalog)), nil
}
