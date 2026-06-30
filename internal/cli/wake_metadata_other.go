//go:build !darwin && !linux

package cli

import "os"

func openWakeMetadataFile(path string) (*os.File, error) {
	return os.Open(path)
}

func openWakeMetadataTempFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}
