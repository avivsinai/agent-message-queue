//go:build !darwin && !linux

package cli

import "os"

func wakeLockHasMultipleLinks(os.FileInfo) bool {
	return false
}
