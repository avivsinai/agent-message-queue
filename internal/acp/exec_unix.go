//go:build !windows

package acp

import "golang.org/x/sys/unix"

// canExecute reports execute access for this process's effective uid/gid
// (AT_EACCESS), not the real ones that plain access(2) checks. A test cannot
// split real and effective ids without privileges, so none covers this.
func canExecute(path string) bool {
	return unix.Faccessat(unix.AT_FDCWD, path, unix.X_OK, unix.AT_EACCESS) == nil
}
