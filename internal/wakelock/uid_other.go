//go:build !darwin && !linux

package wakelock

import "os"

// CurrentUID and FileOwnerUID report the effective uid and a file owner uid;
// false means the platform cannot tell.
var (
	CurrentUID   = func() (int, bool) { return 0, false }
	FileOwnerUID = func(info os.FileInfo) (int, bool) { return 0, false }
)
