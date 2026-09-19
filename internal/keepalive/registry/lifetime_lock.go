package registry

import (
	"errors"
	"os"
	"syscall"
)

// LifetimeLockPath derives the process-lifetime lock path for a registry
// entry. amq-remote up takes this lock for its whole life (611.13.2 D); a
// row whose lock exists but is NOT held is a phantom — its owner died hard
// (kill -9) without running its deferred Forget — and may be reclaimed by
// the next up and marked stale by doctor --ops.
//
// One derivation lives here because both consumers (up reclaim, doctor
// --ops) must probe the same file. The previous spelling lived in
// cmd/amq-remote and was invisible to the keepalive side.
func LifetimeLockPath(regPath, entryID string) string {
	return regPath + ".up-" + entryID + ".lock"
}

// errLockProbeFailed wraps a probe error that is neither held nor free.
var errLockProbeFailed = errors.New("registry: lifetime lock probe failed")

// ProbeLifetimeLock reports whether the process-lifetime lock for entryID is
// currently HELD by a live process. Three outcomes:
//
//   - held=true, nil: a live up owns the row — never reclaim, never stale.
//   - held=false, nil: nobody holds the lock. The lock file may exist
//     (phantom from a hard kill) or not (no up ever ran for it). Either way
//     the row has no living supervisor.
//   - held=false, err: the probe itself failed (permissions, I/O). Callers
//     must treat this as NOT reclaimable — fail closed, keep the row.
//
// The probe takes a non-blocking exclusive flock on the lock file and
// releases it immediately; it never blocks and never creates the file if
// it does not exist.
func ProbeLifetimeLock(regPath, entryID string) (bool, error) {
	lockPath := LifetimeLockPath(regPath, entryID)
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No lock file: no up has ever claimed this slot (or the whole
			// registry directory was cleaned). Not held.
			return false, nil
		}
		return false, errors.Join(errLockProbeFailed, err)
	}
	defer func() { _ = f.Close() }()
	// Non-blocking probe: EWOULDBLOCK/EAGAIN = held by a live process.
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	return false, errors.Join(errLockProbeFailed, err)
}
