//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package registry

import (
	"errors"
	"os"
)

var errCrossProcessLockUnsupported = errors.New("cross-process registry locking is unsupported on this platform")

// These fail-closed fallbacks keep unsupported platforms compilable without
// silently weakening the registry's cross-process ownership guarantees.
func flockExclusive(_ *os.File) error {
	return errCrossProcessLockUnsupported
}

func flockTryExclusive(_ *os.File) (bool, error) {
	return false, errCrossProcessLockUnsupported
}

func flockRelease(_ *os.File) {}
