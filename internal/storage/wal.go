package storage

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/useless-husband/basalt/internal/vfs"
)

// WAL record types.
const (
	RecPages byte = 1 // a mini-transaction: page images and page diffs
	RecNoop  byte = 2 // used by tests
)

const walRecHeader = 9 // len u32, crc u32, type u8

// WAL is the write-ahead log. It is a sequence of segment files named by
// the LSN of their first byte; a new segment is started at every
// checkpoint so that older segments can simply be deleted.
//
// Appends go to an in-memory buffer. Flush writes the buffer and syncs it.
// Concurrent committers that call Flush while another Flush is syncing wait
// for it and then usually find their record already durable: this is group
// commit, and it is what lets many clients share one F_FULLFSYNC.
type WAL struct {
	fs  vfs.FS
	dir string

	mu       sync.Mutex
	buf      []byte
	bufStart LSN
	err      error // sticky write error

	flushMu  sync.Mutex
	file     vfs.File
	segStart LSN
	flushed  atomic.Uint64

	Syncs atomic.Uint64 // number of syncs, for statistics
}

func segName(start LSN) string { return fmt.Sprintf("%016x.wal", uint64(start)) }

func parseSegName(name string) (LSN, bool) {
	if !strings.HasSuffix(name, ".wal") || len(name) != 20 {
		return 0, false
	}
	v, err := strconv.ParseUint(name[:16], 16, 64)
	if err != nil {
		return 0, false
	}
	return LSN(v), true
}

// segments lists WAL segment start LSNs in order.
func segments(fs vfs.FS, dir string) ([]LSN, error) {
	names, err := fs.List(dir)
	if err != nil {
		return nil, err
	}
	var out []LSN
	for _, n := range names {
		if l, ok := parseSegName(n); ok {
			out = append(out, l)
		}
	}
	return out, nil
}

func walCRC(lsn LSN, typ byte, payload []byte) uint32 {
	var h [9]byte
	binary.LittleEndian.PutUint64(h[:], uint64(lsn))
	h[8] = typ
	c := crc32.Update(0, castagnoli, h[:])
	return crc32.Update(c, castagnoli, payload)
}

// openWAL opens the log for appending at end, which must be the end of the
// last valid record (as found by recovery). Garbage after end is cut off.
func openWAL(fs vfs.FS, dir string, end LSN) (*WAL, error) {
	if err := fs.MkdirAll(dir); err != nil {
		return nil, err
	}
	segs, err := segments(fs, dir)
	if err != nil {
		return nil, err
	}
	w := &WAL{fs: fs, dir: dir}
	// Find the segment containing end; remove any later ones.
	var seg LSN
	found := false
	for _, s := range segs {
		if s <= end {
			seg, found = s, true
		}
	}
	for _, s := range segs {
		if s > end {
			if err := fs.Remove(vfs.Join(dir, segName(s))); err != nil {
				return nil, err
			}
		}
	}
	if !found {
		seg = end
	}
	f, err := fs.OpenFile(vfs.Join(dir, segName(seg)), true)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(int64(end - seg)); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if !found {
		if err := fs.SyncDir(dir); err != nil {
			return nil, err
		}
	}
	w.file = f
	w.segStart = seg
	w.bufStart = end
	w.flushed.Store(uint64(end))
	return w, nil
}

// Append adds a record and returns the LSN just past its end.
func (w *WAL) Append(typ byte, payload []byte) (LSN, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	lsn := w.bufStart + LSN(len(w.buf))
	var h [walRecHeader]byte
	binary.LittleEndian.PutUint32(h[0:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(h[4:], walCRC(lsn, typ, payload))
	h[8] = typ
	w.buf = append(w.buf, h[:]...)
	w.buf = append(w.buf, payload...)
	return w.bufStart + LSN(len(w.buf)), nil
}

// CurrentLSN returns the end of the last appended record.
func (w *WAL) CurrentLSN() LSN {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bufStart + LSN(len(w.buf))
}

// FlushedLSN returns the durable end of the log.
func (w *WAL) FlushedLSN() LSN { return LSN(w.flushed.Load()) }

// Flush makes the log durable up to at least upto.
func (w *WAL) Flush(upto LSN) error {
	if LSN(w.flushed.Load()) >= upto {
		return nil
	}
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	if LSN(w.flushed.Load()) >= upto {
		return nil // a concurrent flush covered us
	}
	w.mu.Lock()
	if w.err != nil {
		w.mu.Unlock()
		return w.err
	}
	data := w.buf
	start := w.bufStart
	w.buf = make([]byte, 0, max(cap(data), 64<<10))
	w.bufStart += LSN(len(data))
	end := w.bufStart
	w.mu.Unlock()

	if len(data) > 0 {
		if _, err := w.file.WriteAt(data, int64(start-w.segStart)); err != nil {
			return w.fail(err)
		}
	}
	if err := w.file.Sync(); err != nil {
		return w.fail(err)
	}
	w.Syncs.Add(1)
	w.flushed.Store(uint64(end))
	return nil
}

func (w *WAL) fail(err error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = fmt.Errorf("write-ahead log write failed, database must be restarted: %w", err)
	}
	return w.err
}

// Err returns the sticky write error, if any.
func (w *WAL) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// startSegment begins a new segment at the current end of the log. The
// caller must ensure no appends are in progress (checkpoint lock).
func (w *WAL) startSegment() (LSN, error) {
	end := w.CurrentLSN()
	if err := w.Flush(end); err != nil {
		return 0, err
	}
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	if end == w.segStart {
		return end, nil
	}
	f, err := w.fs.OpenFile(vfs.Join(w.dir, segName(end)), true)
	if err != nil {
		return 0, w.fail(err)
	}
	if err := f.Sync(); err != nil {
		return 0, w.fail(err)
	}
	if err := w.fs.SyncDir(w.dir); err != nil {
		return 0, w.fail(err)
	}
	old := w.file
	w.file = f
	w.segStart = end
	_ = old.Close()
	return end, nil
}

// removeBefore deletes segments that end at or before lsn.
func (w *WAL) removeBefore(lsn LSN) error {
	w.flushMu.Lock()
	cur := w.segStart
	w.flushMu.Unlock()
	segs, err := segments(w.fs, w.dir)
	if err != nil {
		return err
	}
	for i, s := range segs {
		if s == cur {
			break
		}
		next := cur
		if i+1 < len(segs) {
			next = segs[i+1]
		}
		if next <= lsn {
			if err := w.fs.Remove(vfs.Join(w.dir, segName(s))); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *WAL) close() error {
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

// walRecord is a decoded record handed to recovery.
type walRecord struct {
	lsn, end LSN
	typ      byte
	payload  []byte
}

// readWAL calls fn for every valid record at or after from, in order. It
// stops at the first record that is incomplete or fails its checksum (the
// torn tail of the log) and returns the end of the last valid record.
func readWAL(fs vfs.FS, dir string, from LSN, fn func(walRecord) error) (LSN, error) {
	segs, err := segments(fs, dir)
	if err != nil {
		return 0, err
	}
	end := from
	for i, s := range segs {
		segEnd := LSN(1<<63 - 1)
		if i+1 < len(segs) {
			segEnd = segs[i+1]
		}
		if segEnd <= from {
			continue
		}
		f, err := fs.OpenFile(vfs.Join(dir, segName(s)), false)
		if err != nil {
			return 0, err
		}
		size, err := f.Size()
		if err != nil {
			f.Close()
			return 0, err
		}
		data := make([]byte, size)
		if size > 0 {
			if _, err := f.ReadAt(data, 0); err != nil {
				f.Close()
				return 0, err
			}
		}
		f.Close()
		pos := LSN(0)
		if from > s {
			pos = from - s
		}
		if s > end && end != from {
			// A gap between segments: the log is broken here.
			return end, nil
		}
		for {
			if int(pos)+walRecHeader > len(data) {
				break
			}
			h := data[pos:]
			n := binary.LittleEndian.Uint32(h[0:])
			crc := binary.LittleEndian.Uint32(h[4:])
			typ := h[8]
			if int(pos)+walRecHeader+int(n) > len(data) {
				return end, nil
			}
			payload := h[walRecHeader : walRecHeader+int(n)]
			lsn := s + pos
			if walCRC(lsn, typ, payload) != crc {
				return end, nil
			}
			recEnd := lsn + walRecHeader + LSN(n)
			if err := fn(walRecord{lsn: lsn, end: recEnd, typ: typ, payload: payload}); err != nil {
				return 0, err
			}
			end = recEnd
			pos += walRecHeader + LSN(n)
		}
		if i+1 < len(segs) && s+LSN(len(data)) != segs[i+1] {
			// This segment ended early (torn); later segments cannot be
			// trusted.
			return end, nil
		}
		if i+1 == len(segs) {
			return end, nil
		}
		end = max(end, segs[i+1])
	}
	return end, nil
}
