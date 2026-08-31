//go:build darwin || linux

package wakemutation

import "golang.org/x/sys/unix"

func unixUnlinkAt(dirfd int, name string, flags int) error {
	return unix.Unlinkat(dirfd, name, flags)
}
