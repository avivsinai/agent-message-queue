//go:build windows

package sharestate

import "os"

// openLeaf opens a key-directory leaf. Windows has no filesystem FIFOs;
// readLeaf's same-file check on the opened handle refuses a leaf replaced
// after the lstat gate.
func openLeaf(path string) (*os.File, error) {
	return os.Open(path)
}
