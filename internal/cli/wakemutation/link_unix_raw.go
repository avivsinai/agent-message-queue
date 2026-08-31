//go:build darwin || linux

package wakemutation

import "golang.org/x/sys/unix"

func unixLinkAt(fromDirFD int, from string, toDirFD int, to string, flags int) error {
	return unix.Linkat(fromDirFD, from, toDirFD, to, flags)
}
