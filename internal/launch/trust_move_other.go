//go:build !windows

package launch

import (
	"errors"
	"syscall"
)

func isCrossDeviceRename(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}
