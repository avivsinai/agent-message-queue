//go:build unix

package launch

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func validateWrapperExecutable(path string, mode os.FileMode) error {
	if mode.Perm()&0o111 == 0 {
		return fmt.Errorf("executable must have an execute bit")
	}
	if err := unix.Access(path, unix.X_OK); err != nil {
		return fmt.Errorf("executable is not executable by current user: %w", err)
	}
	return nil
}
