//go:build !darwin && !linux

package cli

import "os"

func openWakeMetadataFile(path string) (*os.File, error) {
	return os.Open(path)
}
