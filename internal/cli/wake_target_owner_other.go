//go:build !darwin && !linux

package cli

import "os"

var (
	wakeTargetCurrentUID   = func() (int, bool) { return 0, false }
	wakeTargetFileOwnerUID = func(info os.FileInfo) (int, bool) { return 0, false }
)
