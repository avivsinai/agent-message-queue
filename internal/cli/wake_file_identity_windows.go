//go:build windows

package cli

import "os"

func sameWakeFileIdentity(a, b os.FileInfo) bool { return os.SameFile(a, b) }
