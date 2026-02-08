//go:build !darwin && !linux

package lock

// WithExclusiveFileLock is a best-effort no-op on unsupported platforms.
//
// Swarm interop is primarily used on macOS/Linux; keep non-unix builds compiling
// without introducing platform-specific locking code.
func WithExclusiveFileLock(_ string, fn func() error) error {
	return fn()
}
