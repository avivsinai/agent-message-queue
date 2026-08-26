//go:build !darwin && !linux

package adapter

import "context"

// platformWriterLockInspector falls back to lsof on platforms without a
// native flock inspector.
type platformWriterLockInspector struct {
	Runner CommandRunner
}

func (i platformWriterLockInspector) Held(ctx context.Context, path string) (bool, error) {
	return lsofLockHeld(ctx, i.runner(), path)
}

func (i platformWriterLockInspector) runner() CommandRunner {
	if i.Runner != nil {
		return i.Runner
	}
	return ExecRunner{}
}
