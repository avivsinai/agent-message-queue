//go:build !darwin && !linux

package cli

import "os"

// wakeFileIdentity keeps the platform FileInfo needed by os.SameFile for local
// baseline filtering.
type wakeFileIdentity struct {
	fileInfo os.FileInfo
}
