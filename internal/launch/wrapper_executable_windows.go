//go:build windows

package launch

import (
	"fmt"
	"os"
)

func validateWrapperExecutable(_ string, mode os.FileMode) error {
	if mode.Perm()&0o111 == 0 {
		return fmt.Errorf("executable must have an execute bit")
	}
	return nil
}
