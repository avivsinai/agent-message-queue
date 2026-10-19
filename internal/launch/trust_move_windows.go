//go:build windows

package launch

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isCrossDeviceRename reports a rename across volumes, which Windows returns
// as ERROR_NOT_SAME_DEVICE rather than EXDEV.
func isCrossDeviceRename(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE)
}
