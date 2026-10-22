package lock

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrStopped is returned by WithExclusiveFileLockUntil when stopped reported
// true before the lock was acquired; fn did not run.
var ErrStopped = errors.New("stopped before the lock was acquired")

// ErrUnsupported is returned on a platform with no interprocess file lock.
var ErrUnsupported = errors.New("advisory locks are not available on this platform")

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
		ok, err := TryLock(f)
		if err != nil {
			return fmt.Errorf("acquire lock: %w", err)
		}
		if ok {
			break
		}
		if stopped() {
			return ErrStopped
		}
		time.Sleep(lockRetry)
	}
	defer func() { _ = Unlock(f) }()
	return fn()
}

// TryExclusive takes an exclusive lock on lockPath without waiting. ok is
// false, with no error, when another holder has it. On success the lock stays
// held until release is called or the process exits.
func TryExclusive(lockPath string) (release func(), ok bool, err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open lock file: %w", err)
	}
	ok, err = TryLock(f)
	if err != nil || !ok {
		_ = f.Close()
		if err != nil {
			return nil, false, fmt.Errorf("acquire lock: %w", err)
		}
		return nil, false, nil
	}
	return func() {
		_ = Unlock(f)
		_ = f.Close()
	}, true, nil
}

// WithExclusiveFileLock runs fn while holding an exclusive lock on lockPath.
// The lock is released when fn returns.
//
// The lock is taken on lockPath, not on the data file being updated, so
// callers can replace the data file by atomic rename without invalidating the
// lock.
func WithExclusiveFileLock(lockPath string, fn func() error) error {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if err := Lock(f); err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer func() { _ = Unlock(f) }()

	return fn()
}
