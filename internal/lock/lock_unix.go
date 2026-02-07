//go:build darwin || linux

package lock

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// WithExclusiveFileLock runs fn while holding an exclusive advisory lock on
// lockPath. The lock is released when fn returns.
//
// The lock is taken on lockPath (not the target file being updated) so callers
// can safely use atomic rename for the real data file without invalidating the
// lock (flock is per-inode).
func WithExclusiveFileLock(lockPath string, fn func() error) error {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()

	return fn()
}
