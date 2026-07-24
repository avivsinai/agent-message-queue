//go:build windows

package cli

import (
	"fmt"
	"os"
)

// Windows has no portable Unix ownership or mode bits to validate.
func validateAmqrcFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	// Windows has no portable Unix ownership or mode bits. Symlinked project
	// configs are nevertheless rejected so lookup cannot escape its root.
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(".amqrc at %s is a symlink", path)
	}
	return err
}
