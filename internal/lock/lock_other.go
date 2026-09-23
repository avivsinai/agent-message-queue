//go:build !darwin && !linux

package lock

// AdvisoryLockAvailable reports whether WithExclusiveFileLock holds a real
// interprocess lock. This platform does not.
func AdvisoryLockAvailable() bool { return false }

// WithExclusiveFileLock is a best-effort no-op on unsupported platforms.
//
// Swarm interop is primarily used on macOS/Linux; keep non-unix builds compiling
// without introducing platform-specific locking code.
func WithExclusiveFileLock(_ string, fn func() error) error {
	return fn()
}
