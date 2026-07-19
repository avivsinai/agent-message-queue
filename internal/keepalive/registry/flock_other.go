//go:build !unix

package registry

import "os"

// flockExclusive is a compile-only fallback for platforms without flock
// (e.g. Windows). Cross-process locking is not supported there; the
// in-process mutex in withLock still serializes access within one process.
func flockExclusive(_ *os.File) error {
	return nil
}

// flockRelease matches the Unix implementation's signature.
func flockRelease(_ *os.File) {}
