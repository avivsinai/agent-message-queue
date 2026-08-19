//go:build !windows

package fsq

import "os"

func replaceFile(tmpPath, finalPath string) error {
	return os.Rename(tmpPath, finalPath)
}
