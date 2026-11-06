//go:build darwin

package wakemutation

import "golang.org/x/sys/unix"

func unixRenameExchangeAt(fromDirFD int, from string, toDirFD int, to string) error {
	return unix.RenameatxNp(fromDirFD, from, toDirFD, to, unix.RENAME_SWAP)
}
