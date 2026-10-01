package vfs

import (
	"errors"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
)

// FaultFS is an in-memory file system that models what survives a power
// failure. Every file keeps the content that has been made durable by Sync
// and the list of writes issued since. Crash builds the file system a
// machine would see after rebooting: each unsynced write is independently
// kept or lost, and one of the kept writes may be torn (only a prefix of it
// reaches the disk).
//
// File creation and removal are treated as immediately durable; basalt
// syncs the directory after creating a WAL segment anyway.
type FaultFS struct {
	mu    sync.Mutex
	files map[string]*memFile
	// ErrAfterWrites, when positive, makes every write after that many
	// writes fail with EIO (to test error paths).
	ErrAfterWrites int
	writes         int
}

type pendingWrite struct {
	off  int64
	data []byte
}

type memFile struct {
	fs      *FaultFS
	name    string
	data    []byte // what reads see
	durable []byte // what survives a crash
	pending []pendingWrite
	closed  bool
}

// NewFaultFS returns an empty fault-injecting file system.
func NewFaultFS() *FaultFS { return &FaultFS{files: map[string]*memFile{}} }

// ErrInjected is returned by writes after ErrAfterWrites.
var ErrInjected = errors.New("injected I/O error")

// OpenFile implements FS.
func (fs *FaultFS) OpenFile(name string, create bool) (File, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	f, ok := fs.files[name]
	if !ok {
		if !create {
			return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
		}
		f = &memFile{fs: fs, name: name}
		fs.files[name] = f
	}
	return &memHandle{f: f}, nil
}

// Remove implements FS.
func (fs *FaultFS) Remove(name string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, ok := fs.files[name]; !ok {
		return &os.PathError{Op: "remove", Path: name, Err: os.ErrNotExist}
	}
	delete(fs.files, name)
	return nil
}

// List implements FS.
func (fs *FaultFS) List(dir string) ([]string, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	prefix := strings.TrimSuffix(dir, "/") + "/"
	var out []string
	for name := range fs.files {
		if strings.HasPrefix(name, prefix) && !strings.Contains(name[len(prefix):], "/") {
			out = append(out, name[len(prefix):])
		}
	}
	sort.Strings(out)
	return out, nil
}

// MkdirAll implements FS (directories are implicit).
func (fs *FaultFS) MkdirAll(string) error { return nil }

// SyncDir implements FS.
func (fs *FaultFS) SyncDir(string) error { return nil }

// Exists implements FS.
func (fs *FaultFS) Exists(name string) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	_, ok := fs.files[name]
	return ok
}

// CrashOptions controls Crash.
type CrashOptions struct {
	// KeepProbability is the chance that each unsynced write survives.
	KeepProbability float64
	// TornProbability is the chance that one surviving write is torn.
	TornProbability float64
	// SectorSize is the granularity of torn writes (default 512).
	SectorSize int
}

// Crash returns the file system as it would look after a power failure.
// The receiver is left unchanged so that a still-running process writing
// to it cannot affect the crashed image.
func (fs *FaultFS) Crash(r *rand.Rand, opt CrashOptions) *FaultFS {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if opt.SectorSize == 0 {
		opt.SectorSize = 512
	}
	out := NewFaultFS()
	for name, f := range fs.files {
		img := append([]byte(nil), f.durable...)
		var kept []pendingWrite
		for _, w := range f.pending {
			if r.Float64() < opt.KeepProbability {
				kept = append(kept, w)
			}
		}
		torn := -1
		if len(kept) > 0 && r.Float64() < opt.TornProbability {
			torn = r.Intn(len(kept))
		}
		for i, w := range kept {
			data := w.data
			if i == torn && len(data) > 1 {
				sectors := (len(data) + opt.SectorSize - 1) / opt.SectorSize
				cut := r.Intn(sectors) * opt.SectorSize
				if cut == 0 {
					cut = r.Intn(len(data))
				}
				data = data[:cut]
			}
			img = writeInto(img, w.off, data)
		}
		out.files[name] = &memFile{fs: out, name: name, data: img, durable: append([]byte(nil), img...)}
	}
	return out
}

// Snapshot returns a copy of the current (not crashed) state, as if the
// process had exited cleanly and the OS had flushed everything.
func (fs *FaultFS) Snapshot() *FaultFS {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := NewFaultFS()
	for name, f := range fs.files {
		img := append([]byte(nil), f.data...)
		out.files[name] = &memFile{fs: out, name: name, data: img, durable: append([]byte(nil), img...)}
	}
	return out
}

func writeInto(buf []byte, off int64, p []byte) []byte {
	end := int(off) + len(p)
	if end > len(buf) {
		buf = append(buf, make([]byte, end-len(buf))...)
	}
	copy(buf[off:], p)
	return buf
}

type memHandle struct {
	f *memFile
}

func (h *memHandle) ReadAt(p []byte, off int64) (int, error) {
	fs := h.f.fs
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if h.f.closed {
		return 0, os.ErrClosed
	}
	n := 0
	if off < int64(len(h.f.data)) {
		n = copy(p, h.f.data[off:])
	}
	for i := n; i < len(p); i++ {
		p[i] = 0
	}
	return len(p), nil
}

func (h *memHandle) WriteAt(p []byte, off int64) (int, error) {
	fs := h.f.fs
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if h.f.closed {
		return 0, os.ErrClosed
	}
	fs.writes++
	if fs.ErrAfterWrites > 0 && fs.writes > fs.ErrAfterWrites {
		return 0, ErrInjected
	}
	h.f.data = writeInto(h.f.data, off, p)
	h.f.pending = append(h.f.pending, pendingWrite{off, append([]byte(nil), p...)})
	return len(p), nil
}

func (h *memHandle) Sync() error {
	fs := h.f.fs
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if h.f.closed {
		return os.ErrClosed
	}
	h.f.durable = append(h.f.durable[:0], h.f.data...)
	h.f.pending = nil
	return nil
}

func (h *memHandle) Truncate(size int64) error {
	fs := h.f.fs
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if int64(len(h.f.data)) > size {
		h.f.data = h.f.data[:size]
	} else {
		h.f.data = append(h.f.data, make([]byte, size-int64(len(h.f.data)))...)
	}
	// Truncation is applied to the durable image immediately; basalt only
	// truncates to cut garbage off the end of the WAL during recovery and
	// syncs right after.
	if int64(len(h.f.durable)) > size {
		h.f.durable = h.f.durable[:size]
	}
	var kept []pendingWrite
	for _, w := range h.f.pending {
		if w.off < size {
			if w.off+int64(len(w.data)) > size {
				w.data = w.data[:size-w.off]
			}
			kept = append(kept, w)
		}
	}
	h.f.pending = kept
	return nil
}

func (h *memHandle) Size() (int64, error) {
	fs := h.f.fs
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return int64(len(h.f.data)), nil
}

func (h *memHandle) Close() error { return nil }
