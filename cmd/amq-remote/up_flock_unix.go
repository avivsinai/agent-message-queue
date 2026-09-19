//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"errors"
	"os"
	"syscall"
)

// flockLifetime takes an exclusive non-blocking advisory lock held for the
// process lifetime. EWOULDBLOCK/EAGAIN mean another up owns the slot.
func flockLifetime(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errLifetimeOwned
	}
	return err
}

var errLifetimeOwned = errors.New("lifetime lock already held by another up process")
