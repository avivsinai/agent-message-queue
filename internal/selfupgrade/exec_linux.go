//go:build linux

package selfupgrade

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func execSupportedPlatform() bool { return true }

func execImagePlatform(candidate ImageEvidence, argv, env []string) error {
	lstat, err := os.Lstat(candidate.ExecutionPath)
	if err != nil {
		return fmt.Errorf("stat self-upgrade candidate: %w", err)
	}
	if lstat.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("self-upgrade candidate must not be a symlink")
	}
	fd, err := unix.Open(candidate.ExecutionPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open self-upgrade candidate: %w", err)
	}
	file := os.NewFile(uintptr(fd), candidate.ExecutionPath)
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("open self-upgrade candidate file descriptor")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat opened self-upgrade candidate: %w", err)
	}
	if !os.SameFile(lstat, opened) {
		return errors.New("self-upgrade candidate changed while binding")
	}
	executionPath := fmt.Sprintf("/proc/self/fd/%d", fd)
	bound, err := CaptureImageEvidenceFromOpenFile(
		file,
		executionPath,
		candidate.EmbeddedVersion,
		ImageMethodFDExec,
	)
	if err != nil {
		return err
	}
	if !SameImageEvidenceExceptMethodPath(bound, candidate) {
		return errors.New("self-upgrade candidate changed while binding")
	}
	return syscall.Exec(executionPath, argv, env)
}
