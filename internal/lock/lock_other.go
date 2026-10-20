//go:build !darwin && !linux

package lock

import "errors"

// AdvisoryLockAvailable reports whether WithExclusiveFileLock holds a real
// interprocess lock. This platform does not.
func AdvisoryLockAvailable() bool { return false }

// ErrStopped mirrors the unix value; it is never returned here.
var ErrStopped = errors.New("stopped before the lock was acquired")

// ErrUnsupported is returned by TryExclusive on this platform.
var ErrUnsupported = errors.New("advisory locks are not available on this platform")

// TryExclusive holds no lock on unsupported platforms; it returns
// ErrUnsupported.
func TryExclusive(_ string) (release func(), ok bool, err error) {
	return nil, false, ErrUnsupported
}

// WithExclusiveFileLockUntil runs fn with no lock on unsupported platforms.
func WithExclusiveFileLockUntil(_ string, _ func() bool, fn func() error) error {
	return fn()
}

// WithExclusiveFileLock is a best-effort no-op on unsupported platforms.
//
// Swarm interop is primarily used on macOS/Linux; keep non-unix builds compiling
// without introducing platform-specific locking code.
func WithExclusiveFileLock(_ string, fn func() error) error {
	return fn()
}
