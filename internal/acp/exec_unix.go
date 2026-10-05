//go:build !windows

package acp

import "golang.org/x/sys/unix"

// canExecute reports execute access for this process's effective identity.
func canExecute(path string) bool {
	return unix.Access(path, unix.X_OK) == nil
}
