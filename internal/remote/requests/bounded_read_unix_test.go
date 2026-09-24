//go:build unix

package requests

import "syscall"

func plantNamedPipe(path string) error {
	return syscall.Mkfifo(path, 0o600)
}
