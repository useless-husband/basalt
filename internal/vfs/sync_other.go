//go:build !darwin

package vfs

import "os"

// fullSync is fsync(2) (or fdatasync where Go maps Sync to it).
func fullSync(f *os.File) error { return f.Sync() }

// FullFsync reports whether this platform uses F_FULLFSYNC.
const FullFsync = false
