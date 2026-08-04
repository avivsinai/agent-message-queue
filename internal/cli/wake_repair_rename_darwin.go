//go:build darwin

package cli

import "golang.org/x/sys/unix"

func renameWakeNoReplaceAt(
	fromDirFD int,
	from string,
	toDirFD int,
	to string,
) error {
	return unix.RenameatxNp(fromDirFD, from, toDirFD, to, unix.RENAME_EXCL)
}

func renameWakeRepairNoReplaceAt(fromDirFD int, from string, toDirFD int, to string) error {
	return renameWakeNoReplaceAt(fromDirFD, from, toDirFD, to)
}
