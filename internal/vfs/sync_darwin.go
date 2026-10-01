//go:build darwin

package vfs

import (
	"os"
	"syscall"
)

// fullSync issues fcntl(F_FULLFSYNC). On macOS fsync(2) only pushes data to
// the drive, which may keep it in a volatile cache; F_FULLFSYNC asks the
// drive to flush that cache too. If the file system does not support it we
// fall back to fsync.
func fullSync(f *os.File) error {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_FULLFSYNC, 0)
	if errno == 0 {
		return nil
	}
	return f.Sync()
}

// FullFsync reports whether this platform uses F_FULLFSYNC.
const FullFsync = true
