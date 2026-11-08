//go:build darwin

package cli

import (
	"os"
	"path/filepath"
)

func wakeRunsFromRestartStage() bool {
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	return err == nil && darwinWakeRestartStagePath(filepath.Clean(executable))
}
