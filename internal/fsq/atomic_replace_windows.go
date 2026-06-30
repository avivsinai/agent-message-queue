//go:build windows

package fsq

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func replaceFile(tmpPath, finalPath string) error {
	from, err := windows.UTF16PtrFromString(tmpPath)
	if err != nil {
		return fmt.Errorf("prepare atomic replace source path: %w", err)
	}
	to, err := windows.UTF16PtrFromString(finalPath)
	if err != nil {
		return fmt.Errorf("prepare atomic replace destination path: %w", err)
	}
	flags := uint32(windows.MOVEFILE_REPLACE_EXISTING | windows.MOVEFILE_WRITE_THROUGH)
	if err := windows.MoveFileEx(from, to, flags); err != nil {
		return fmt.Errorf("atomic replace %s: %w", finalPath, err)
	}
	return nil
}
