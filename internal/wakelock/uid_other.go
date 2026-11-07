//go:build !darwin && !linux

package wakelock

import "os"

// CurrentUID reports the effective uid; false means the platform cannot tell.
func CurrentUID() (int, bool) { return 0, false }

// FileOwnerUID reports the owner uid of info; false means the platform cannot
// tell.
func FileOwnerUID(os.FileInfo) (int, bool) { return 0, false }
