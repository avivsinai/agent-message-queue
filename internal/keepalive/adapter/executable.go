package adapter

import (
	"os"
	"runtime"
)

// isExecutableRegularFile applies the host's executable-file contract.
// Windows does not expose Unix execute permission bits through FileMode; a
// regular .exe selected by LookPath is executable by construction.
func isExecutableRegularFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0)
}

func platformExecutableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
