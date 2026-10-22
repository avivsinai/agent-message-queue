//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package lock

import "os"

// AdvisoryLockAvailable reports whether this platform has a real
// interprocess file lock. This one does not.
func AdvisoryLockAvailable() bool { return false }

// Lock returns ErrUnsupported: this platform has no interprocess file lock.
func Lock(*os.File) error { return ErrUnsupported }

// TryLock returns ErrUnsupported: this platform has no interprocess file lock.
func TryLock(*os.File) (bool, error) { return false, ErrUnsupported }

// Unlock returns ErrUnsupported: this platform has no interprocess file lock.
func Unlock(*os.File) error { return ErrUnsupported }
