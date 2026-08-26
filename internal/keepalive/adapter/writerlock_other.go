//go:build !darwin && !linux

package adapter

import (
	"context"
	"fmt"
	"runtime"
)

type platformWriterLockInspector struct{}

func (platformWriterLockInspector) Held(_ context.Context, _ string) (bool, error) {
	return false, fmt.Errorf("codex-queue writer-lock inspection is unsupported on %s; run on darwin or linux", runtime.GOOS)
}
