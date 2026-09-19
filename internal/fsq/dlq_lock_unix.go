//go:build darwin || linux || freebsd

package fsq

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func withExclusiveDLQEnvelopeLock(file *os.File, fn func() error) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("acquire DLQ envelope lock: %w", err)
	}
	defer func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }()
	return fn()
}

// WithExclusiveFileLock holds an exclusive advisory lock on file for the
// duration of fn. The file must have been created via OpenLockFile (stable
// name, never replaced, so flock serializes on one inode across processes).
// The lock is kernel-released on close or process crash; there is no stale
// sentinel to clean up.
func WithExclusiveFileLock(file *os.File, fn func() error) error {
	return withExclusiveDLQEnvelopeLock(file, fn)
}
