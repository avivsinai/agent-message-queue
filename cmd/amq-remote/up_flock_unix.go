//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"os"

	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// flockLifetime takes an exclusive non-blocking advisory lock held for the
// process lifetime. A busy lock means another up owns the slot.
func flockLifetime(f *os.File) error {
	ok, err := lock.TryLock(f)
	if err != nil {
		return err
	}
	if !ok {
		return errLifetimeHeld
	}
	return nil
}
