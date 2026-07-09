//go:build unix

package gc

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// errLockHeld is returned by tryLockExclusive when another process holds
// the lock.
var errLockHeld = errors.New("lock already held")

// tryLockExclusive attempts to acquire an exclusive POSIX advisory
// lock on path (fcntl F_SETLK), matching Bazel's own FileSystemLock
// mechanism so we observe each other's locks. The lock file is created
// (mode 0o600) if it doesn't exist.
//
// The returned unlock closes the fd and releases the lock. POSIX file
// locks are per-process and are released when *any* fd to the file
// closes, so callers must not open the lock file separately while
// holding the lock.
func tryLockExclusive(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	lk := &syscall.Flock_t{
		Type:   syscall.F_WRLCK,
		Whence: 0,
		Start:  0,
		Len:    0, // whole file
	}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, lk); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) {
			return nil, errLockHeld
		}
		return nil, fmt.Errorf("fcntl F_SETLK: %w", err)
	}
	return func() {
		lk.Type = syscall.F_UNLCK
		_ = syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, lk)
		_ = f.Close()
	}, nil
}
