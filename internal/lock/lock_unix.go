//go:build darwin || linux

package lock

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// ErrStopped is returned by WithExclusiveFileLockUntil when stopped reported
// true before the lock was acquired; fn did not run.
var ErrStopped = errors.New("stopped before the lock was acquired")

// lockRetry is the pause between non-blocking lock attempts.
const lockRetry = 20 * time.Millisecond

// WithExclusiveFileLockUntil is WithExclusiveFileLock that gives up, without
// running fn, once stopped reports true. A caller whose work can be cancelled
// or can run out of time never waits on the lock past that point.
func WithExclusiveFileLockUntil(lockPath string, stopped func() bool, fn func() error) error {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer func() { _ = f.Close() }()
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("acquire lock: %w", err)
		}
		if stopped() {
			return ErrStopped
		}
		time.Sleep(lockRetry)
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	return fn()
}

// AdvisoryLockAvailable reports whether WithExclusiveFileLock holds a real
// interprocess lock. On other platforms the helper runs fn with no lock.
func AdvisoryLockAvailable() bool { return true }

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
