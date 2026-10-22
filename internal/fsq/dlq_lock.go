package fsq

import (
	"fmt"
	"os"

	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// tryExclusiveFileLock holds an exclusive lock on file for the duration of fn
// without waiting. It returns ErrLockBusy when another holder has the lock.
func tryExclusiveFileLock(file *os.File, fn func() error) error {
	ok, err := lock.TryLock(file)
	if err != nil {
		return fmt.Errorf("acquire file lock: %w", err)
	}
	if !ok {
		return ErrLockBusy
	}
	defer func() { _ = lock.Unlock(file) }()
	return fn()
}

func withExclusiveDLQEnvelopeLock(file *os.File, fn func() error) error {
	if err := lock.Lock(file); err != nil {
		return fmt.Errorf("acquire DLQ envelope lock: %w", err)
	}
	defer func() { _ = lock.Unlock(file) }()
	return fn()
}

// WithExclusiveFileLock holds an exclusive lock on file for the duration of
// fn. The file must have been created via OpenLockFile (stable name, never
// replaced, so the lock serializes on one file across processes). The lock is
// kernel-released on close or process crash; there is no stale sentinel to
// clean up.
func WithExclusiveFileLock(file *os.File, fn func() error) error {
	return withExclusiveDLQEnvelopeLock(file, fn)
}
