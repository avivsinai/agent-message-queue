//go:build linux

package cli

import "golang.org/x/sys/unix"

func exchangeWakeRepairDirectoriesForTest(first, second string) error {
	return unix.Renameat2(
		unix.AT_FDCWD,
		first,
		unix.AT_FDCWD,
		second,
		unix.RENAME_EXCHANGE,
	)
}
