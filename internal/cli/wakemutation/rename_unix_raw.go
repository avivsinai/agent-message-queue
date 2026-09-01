//go:build darwin || linux

package wakemutation

import "golang.org/x/sys/unix"

func unixRenameAt(fromDirFD int, from string, toDirFD int, to string) error {
	return unix.Renameat(fromDirFD, from, toDirFD, to)
}
