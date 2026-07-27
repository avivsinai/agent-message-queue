//go:build darwin

package cli

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func currentTTYPath(_ *os.File) string {
	tdev, ok := currentDarwinControllingTerminalDevice()
	if !ok {
		return ""
	}
	paths, err := filepath.Glob("/dev/ttys*")
	if err != nil {
		return ""
	}
	paths = append(paths, "/dev/console")
	return findDarwinTTYPath(tdev, paths, unix.Stat)
}

func sameWakeTerminalAsCurrent(inspection wakeLockInspection) bool {
	if inspection.Process.ControllingTerminalKnown &&
		inspection.Process.HasControllingTerminal {
		if current, ok := currentDarwinControllingTerminalDevice(); ok {
			return current == inspection.Process.ControllingTerminalDevice
		}
	}
	return sameWakeTTYPathAsCurrent(inspection)
}

func currentDarwinControllingTerminalDevice() (int32, bool) {
	process, err := readDarwinKinfoProc("kern.proc.pid", os.Getpid())
	if err != nil || process == nil || process.Eproc.Tdev == darwinNoControllingTerminal {
		return 0, false
	}
	return process.Eproc.Tdev, true
}

func findDarwinTTYPath(
	rdev int32,
	paths []string,
	stat func(string, *unix.Stat_t) error,
) string {
	for _, path := range paths {
		var candidate unix.Stat_t
		if err := stat(path, &candidate); err != nil {
			continue
		}
		if candidate.Mode&unix.S_IFMT == unix.S_IFCHR && candidate.Rdev == rdev {
			return path
		}
	}
	return ""
}
