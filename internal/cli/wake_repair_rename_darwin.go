//go:build darwin

package cli

func renameWakeNoReplaceAt(
	fromDirFD int,
	from string,
	toDirFD int,
	to string,
) error {
	return wakeRenameNoReplaceAt(fromDirFD, from, toDirFD, to)
}

func renameWakeRepairNoReplaceAt(fromDirFD int, from string, toDirFD int, to string) error {
	return renameWakeNoReplaceAt(fromDirFD, from, toDirFD, to)
}
