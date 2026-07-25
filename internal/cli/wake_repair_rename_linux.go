//go:build linux

package cli

import "golang.org/x/sys/unix"

func renameWakeRepairNoReplaceAt(
	fromDirFD int,
	from string,
	toDirFD int,
	to string,
) error {
	return unix.Renameat2(fromDirFD, from, toDirFD, to, unix.RENAME_NOREPLACE)
}
