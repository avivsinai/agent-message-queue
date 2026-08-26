//go:build darwin

package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// platformWriterLockInspector uses F_GETLK without taking the lock. Live check
// on macOS (2026-08-26): a Codex GUI-held flock on
// ~/.codex/thread-writer-locks/<uuid>.lock is visible to F_GETLK as Type=F_WRLCK
// with Pid=-1, and LOCK_EX|LOCK_NB on the same fd returns EAGAIN. An idle file
// returns Type=F_UNLCK. Inspection errors fail closed; lsof is not a lock
// inspector (an open fd is not a held flock). Linux uses /proc/locks instead:
// flock and fcntl do not interoperate there.
type platformWriterLockInspector struct{}

func (platformWriterLockInspector) Held(_ context.Context, path string) (bool, error) {
	return fcntlLockHeld(path)
}

func fcntlLockHeld(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: int16(io.SeekStart)}
	if err := unix.FcntlFlock(f.Fd(), unix.F_GETLK, &lk); err != nil {
		return false, fmt.Errorf("F_GETLK %s: %w", path, err)
	}
	return lk.Type != unix.F_UNLCK, nil
}
