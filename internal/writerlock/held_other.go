//go:build !darwin && !linux && !windows

package writerlock

import (
	"context"
	"fmt"
	"runtime"
)

// Held is unsupported on this platform and always returns an error.
func Held(_ context.Context, _ string) (bool, error) {
	return false, fmt.Errorf("codex-queue writer-lock inspection is unsupported on %s; run on darwin or linux", runtime.GOOS)
}
