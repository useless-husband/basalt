package storage

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/vfs"
)

// Frame is a buffer pool slot holding one page. A frame is pinned while in
// use (it cannot be evicted) and its latch protects the page contents:
// readers hold it shared, a mini-transaction holds it exclusive.
type Frame struct {
	Latch sync.RWMutex
	id    PageID
	Data  []byte
	pin   atomic.Int32
	dirty atomic.Bool
	ref   atomic.Bool
	err   error // load error, set before the latch is released after loading
}

// ID returns the page held by the frame.
func (f *Frame) ID() PageID { return f.id }

// Pool is the buffer pool: a fixed number of frames shared by all
// sessions, with a clock (second chance) eviction policy.
type Pool struct {
	mu     sync.Mutex
	frames []*Frame
	table  map[PageID]*Frame
	hand   int
	file   vfs.File
	wal    *WAL

	Reads, Writes, Hits atomic.Uint64
}

func newPool(file vfs.File, wal *WAL, nframes int) *Pool {
	p := &Pool{file: file, wal: wal, table: make(map[PageID]*Frame, nframes)}
	buf := make([]byte, nframes*PageSize)
	p.frames = make([]*Frame, nframes)
	for i := range p.frames {
		p.frames[i] = &Frame{Data: buf[i*PageSize : (i+1)*PageSize : (i+1)*PageSize], id: InvalidPage}
		p.frames[i].id = ^PageID(0)
	}
	return p
}

var errPoolFull = pgerr.New(pgerr.ConfigurationLimitExceeded, "no unpinned buffers available")

// victim finds an unpinned frame using the clock algorithm. Called with mu.
// Capacity returns the number of frames.
func (p *Pool) Capacity() int { return len(p.frames) }

func (p *Pool) victim() (*Frame, error) {
	n := len(p.frames)
	for i := 0; i < 3*n; i++ {
		f := p.frames[p.hand]
		p.hand = (p.hand + 1) % n
		if f.pin.Load() != 0 {
			continue
		}
		if f.ref.Load() {
			f.ref.Store(false)
			continue
		}
		return f, nil
	}
	return nil, errPoolFull
}

// writeFrame writes a frame's page to disk, honouring the WAL rule: the log
// must be durable up to the page LSN before the page may be written.
// The caller guarantees nobody modifies the frame concurrently.
func (p *Pool) writeFrame(f *Frame) error {
	img := make([]byte, PageSize)
	copy(img, f.Data)
	if lsn := pageLSN(img); p.wal != nil && lsn > p.wal.FlushedLSN() {
		if err := p.wal.Flush(lsn); err != nil {
			return err
		}
	}
	setChecksum(img)
	if _, err := p.file.WriteAt(img, int64(f.id)*PageSize); err != nil {
		return err
	}
	p.Writes.Add(1)
	f.dirty.Store(false)
	return nil
}

// fetch returns the frame for page id, pinned. If load is false the page
// is not read from disk and the frame is zeroed (for new pages and for
// full-page images during recovery).
func (p *Pool) fetch(id PageID, load bool) (*Frame, error) {
	p.mu.Lock()
	if f, ok := p.table[id]; ok {
		f.pin.Add(1)
		f.ref.Store(true)
		p.mu.Unlock()
		p.Hits.Add(1)
		if !load {
			return f, nil
		}
		// Wait for a concurrent load to finish.
		f.Latch.RLock()
		err := f.err
		f.Latch.RUnlock()
		if err != nil {
			p.unpin(f)
			return nil, err
		}
		return f, nil
	}
	f, err := p.victim()
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	if f.dirty.Load() {
		// Write the old page while holding the pool lock; nobody can
		// reach the frame because it is unpinned and we hold mu.
		if err := p.writeFrame(f); err != nil {
			p.mu.Unlock()
			return nil, err
		}
	}
	if f.id != ^PageID(0) {
		delete(p.table, f.id)
	}
	f.id = id
	f.err = nil
	f.pin.Store(1)
	f.ref.Store(true)
	p.table[id] = f
	f.Latch.Lock() // held until the page is loaded
	p.mu.Unlock()

	if !load {
		clear(f.Data)
		f.Latch.Unlock()
		return f, nil
	}
	if _, err := p.file.ReadAt(f.Data, int64(id)*PageSize); err != nil {
		f.err = err
	} else if !verifyChecksum(f.Data) {
		f.err = pgerr.New(pgerr.DataCorrupted, "invalid page in block %d: checksum mismatch", id)
	}
	p.Reads.Add(1)
	err = f.err
	f.Latch.Unlock()
	if err != nil {
		p.mu.Lock()
		if p.table[id] == f {
			delete(p.table, id)
			f.id = ^PageID(0)
		}
		p.mu.Unlock()
		p.unpin(f)
		return nil, err
	}
	return f, nil
}

func (p *Pool) unpin(f *Frame) {
	if f.pin.Add(-1) < 0 {
		panic(fmt.Sprintf("buffer pool: unpin of unpinned page %d", f.id))
	}
}

// Read pins page id and latches it shared. Call Release when done.
func (p *Pool) Read(id PageID) (*Frame, error) {
	f, err := p.fetch(id, true)
	if err != nil {
		return nil, err
	}
	f.Latch.RLock()
	return f, nil
}

// Release unlatches (shared) and unpins a frame returned by Read.
func (p *Pool) Release(f *Frame) {
	f.Latch.RUnlock()
	p.unpin(f)
}

// flushAll writes every dirty page. Pages may be read concurrently (we take
// the latch shared while copying) but must not be modified: the caller
// holds the checkpoint lock exclusively.
func (p *Pool) flushAll() error {
	p.mu.Lock()
	frames := make([]*Frame, 0, len(p.table))
	for _, f := range p.table {
		if f.dirty.Load() {
			f.pin.Add(1)
			frames = append(frames, f)
		}
	}
	p.mu.Unlock()
	var firstErr error
	for _, f := range frames {
		f.Latch.RLock()
		err := p.writeFrame(f)
		f.Latch.RUnlock()
		p.unpin(f)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ErrClosed is returned after the store has been closed.
var ErrClosed = errors.New("storage: store is closed")
