//go:build unix

package requests

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// ownerLock is the process-wide single-writer lock. It is an advisory flock
// held for the life of the Store, not a distributed lease.
type ownerLock struct {
	f *os.File
}

func acquireOwnerLock(path string) (*ownerLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return nil, fmt.Errorf("open owner lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, protocol.Refuse(protocol.CodeEndpointAlreadyRunning, "another endpoint owns %s", path)
		}
		return nil, fmt.Errorf("lock owner file: %w", err)
	}
	return &ownerLock{f: f}, nil
}

func (l *ownerLock) release() error {
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

func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
