//go:build !darwin && !linux

package selfupgrade

import "os"

func openImageMetadataFile(path string) (*os.File, error) { return os.Open(path) }
