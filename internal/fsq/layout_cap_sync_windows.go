//go:build windows

package fsq

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// syncPlatform flushes the pinned directory. The capability's read-only handle
// cannot be flushed, so it is reopened relative to itself with write access.
func (c *layoutDirCapability) syncPlatform() error {
	handle, err := ntOpenDirectoryIn(windows.Handle(c.file.Fd()), "")
	if err != nil {
		return fmt.Errorf("reopen layout directory for sync: %w (%v)", err, windowsClaimError(err))
	}
	return flushDirHandle(handle, "layout directory")
}
