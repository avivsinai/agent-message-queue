//go:build unix

package hookinstall

import (
	"os"
	"syscall"
)

func exclusiveCreateFlags() int {
	return os.O_WRONLY | os.O_CREATE | os.O_EXCL | syscall.O_NOFOLLOW
}
