//go:build !darwin

package adapter

import (
	"fmt"
	"runtime"
)

// ttyLiveOwnerCount is unsupported off darwin. The cmux adapter already refuses
// to run on non-darwin platforms (requireCmuxPlatform), so this exists only to
// keep the package building; it is never reached through Inventory.
func ttyLiveOwnerCount(string) (int, error) {
	return 0, fmt.Errorf("cmux tty liveness is unsupported on %s", runtime.GOOS)
}
