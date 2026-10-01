// Package vfs abstracts the file system so that the storage engine can run
// on the real disk (with F_FULLFSYNC on macOS) or on a fault-injecting
// in-memory file system in tests.
package vfs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// File is a random-access file.
type File interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	// Sync makes all previous writes durable. On darwin this issues
	// F_FULLFSYNC, because fsync(2) there does not flush the drive cache.
	Sync() error
	Truncate(size int64) error
	Size() (int64, error)
	Close() error
}

// FS is a file system.
type FS interface {
	// OpenFile opens a file for reading and writing, creating it if create
	// is set.
	OpenFile(name string, create bool) (File, error)
	Remove(name string) error
	// List returns the names of the entries of a directory, sorted.
	List(dir string) ([]string, error)
	MkdirAll(dir string) error
	// SyncDir makes directory entries (created or removed files) durable.
	SyncDir(dir string) error
	Exists(name string) bool
}

// OS is the real file system.
type OS struct{}

type osFile struct{ f *os.File }

// OpenFile implements FS.
func (OS) OpenFile(name string, create bool) (File, error) {
	flag := os.O_RDWR
	if create {
		flag |= os.O_CREATE
	}
	f, err := os.OpenFile(name, flag, 0o644)
	if err != nil {
		return nil, err
	}
	return &osFile{f}, nil
}

// Remove implements FS.
func (OS) Remove(name string) error { return os.Remove(name) }

// List implements FS.
func (OS) List(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// MkdirAll implements FS.
func (OS) MkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

// SyncDir implements FS.
func (OS) SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := fullSync(d); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

// Exists implements FS.
func (OS) Exists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}

func (f *osFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.f.ReadAt(p, off)
	if err == io.EOF && n < len(p) {
		// Reading past the end returns zeros, like a sparse file.
		for i := n; i < len(p); i++ {
			p[i] = 0
		}
		return len(p), nil
	}
	return n, err
}

func (f *osFile) WriteAt(p []byte, off int64) (int, error) { return f.f.WriteAt(p, off) }
func (f *osFile) Sync() error                              { return fullSync(f.f) }
func (f *osFile) Truncate(size int64) error                { return f.f.Truncate(size) }
func (f *osFile) Close() error                             { return f.f.Close() }

func (f *osFile) Size() (int64, error) {
	st, err := f.f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Join is filepath.Join, re-exported so callers need not import both.
func Join(elem ...string) string { return filepath.Join(elem...) }
