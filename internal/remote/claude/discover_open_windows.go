//go:build windows

package claude

import (
	"fmt"
	"os"
)

// openSessionsDir opens the sessions directory for listing. Windows has no
// FIFO at a filesystem path, so the concern the unix variant closes does not
// arise; the directory type is still checked on the opened handle.
func openSessionsDir(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || !fi.IsDir() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a directory", path)
	}
	return f, nil
}
