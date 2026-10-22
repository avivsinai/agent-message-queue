//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package registry

import (
	"os"

	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// flockExclusive takes an exclusive advisory lock on the open lock file,
// blocking until it is available.
func flockExclusive(f *os.File) error {
	return lock.Lock(f)
}

// flockTryExclusive attempts the same lock without blocking. A busy lock is a
// normal false result so context-aware callers can poll and honor cancellation.
func flockTryExclusive(f *os.File) (bool, error) {
	return lock.TryLock(f)
}

func flockRelease(f *os.File) {
	_ = lock.Unlock(f)
}
