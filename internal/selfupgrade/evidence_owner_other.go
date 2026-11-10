//go:build !darwin && !linux

package selfupgrade

import "os"

// FileOwnerUID reports the owner uid of info; false means the platform cannot
// tell.
func FileOwnerUID(os.FileInfo) (int, bool) { return 0, false }

// CurrentUID reports the effective uid; false means the platform cannot tell.
func CurrentUID() (int, bool) { return 0, false }
