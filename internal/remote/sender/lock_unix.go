//go:build unix

package sender

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// spoolLock is a stable, spool-level advisory flock. It is held for the
// duration of a short filesystem transaction (Create, read-modify-write
// state transitions, reap) — NOT for the life of the Spool, and NEVER across
// Endpoint.Handle, IPC, or the drainer dispatch loop.
//
// One stable lock file per spool dir avoids unbounded per-key lock-file
// growth. Every Open instance/process that touches the same spool dir
// acquires this same lock for each transaction, so concurrent writers are
// serialized at the OS level even across processes.
//
// This follows internal/remote/requests/lock_unix.go's OS primitive, but is
// a BLOCKING exclusive lock (the request store's owner lock is nonblocking
// and held for the process life). We block because the transaction is short
// and the caller has already chosen to persist; a brief wait is correct.
type spoolLock struct {
	f *os.File
}

var spoolLockMu sync.Mutex // serializes lock-file path creation within a process

// acquireSpoolLock opens (creating if missing) the stable lock file and
// acquires a blocking exclusive flock. The lock is released by release().
// On unsupported platforms (lock_other.go) the caller refuses CodeUnsupported
// before reaching here.
func acquireSpoolLock(path string) (*spoolLock, error) {
	spoolLockMu.Lock()
	defer spoolLockMu.Unlock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return nil, fmt.Errorf("open spool lock: %w", err)
	}
	// Blocking exclusive lock: wait for any other writer to finish its
	// transaction. The transaction is short (read+write+rename), so a brief
	// wait is the correct behavior, not a failure.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EINTR) {
			return nil, fmt.Errorf("interrupted waiting for spool lock: %w", err)
		}
		return nil, fmt.Errorf("lock spool file: %w", err)
	}
	return &spoolLock{f: f}, nil
}

func (l *spoolLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// withLock acquires the spool lock, runs fn, and releases. It is the single
// entry point every filesystem transaction uses. The in-process s.mu is
// still held by callers for memory-safety of s fields; this lock adds
// cross-process safety for the file operations.
func (s *Spool) withLock(fn func() error) error {
	if s.lockPath == "" {
		// Should not happen: Open refuses on unsupported platforms. Treat as
		// unsupported so the caller surfaces the error instead of racing.
		return protocol.Refuse(protocol.CodeUnsupported, "spool locking is not available on this platform")
	}
	lk, err := acquireSpoolLock(s.lockPath)
	if err != nil {
		return err
	}
	defer func() { _ = lk.release() }()
	return fn()
}
