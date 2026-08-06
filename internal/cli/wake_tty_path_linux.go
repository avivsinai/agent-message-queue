//go:build linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
)

func currentTTYPath(tty *os.File) string {
	link, err := os.Readlink(fmt.Sprintf("/dev/fd/%d", tty.Fd()))
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(link); err == nil {
		return real
	}
	return link
}
