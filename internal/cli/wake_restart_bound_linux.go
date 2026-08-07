//go:build linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func bindWakeRestartCandidatePlatform(candidate wakeImageEvidenceV1) (*wakeRestartBoundImage, error) {
	lstat, err := os.Lstat(candidate.ExecutionPath)
	if err != nil {
		return nil, fmt.Errorf("stat wake restart candidate: %w", err)
	}
	if lstat.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("wake restart candidate must not be a symlink")
	}
	fd, err := unix.Open(candidate.ExecutionPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open wake restart candidate: %w", err)
	}
	file := os.NewFile(uintptr(fd), candidate.ExecutionPath)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind wake restart candidate file descriptor")
	}
	bound := &wakeRestartBoundImage{file: file}
	failed := true
	defer func() {
		if failed {
			_ = bound.close()
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened wake restart candidate: %w", err)
	}
	if !os.SameFile(lstat, opened) {
		return nil, fmt.Errorf("wake restart candidate changed while binding")
	}
	bound.executionPath = filepath.Clean(fmt.Sprintf("/proc/self/fd/%d", fd))
	bound.evidence, err = captureWakeImageEvidenceFromOpenFile(
		file,
		bound.executionPath,
		candidate.EmbeddedVersion,
		wakeImageMethodFDExec,
	)
	if err != nil {
		return nil, err
	}
	if !sameWakeImageEvidenceExceptMethodPath(bound.evidence, candidate) {
		return nil, fmt.Errorf("wake restart candidate changed while binding")
	}
	failed = false
	return bound, nil
}

func bindWakeRestartCandidateForRecordPlatform(record wakeRestartRecord) (*wakeRestartBoundImage, error) {
	return bindWakeRestartCandidatePlatform(record.Candidate)
}

func boundWakeRestartPreflightCommandPlatform(image *wakeRestartBoundImage) (string, []*os.File, error) {
	if image == nil || image.file == nil {
		return "", nil, fmt.Errorf("bound wake restart image is missing")
	}
	return "/proc/self/fd/3", []*os.File{image.file}, nil
}

func revalidateBoundWakeRestartImagePlatform(image *wakeRestartBoundImage) error {
	if image == nil || image.file == nil {
		return fmt.Errorf("bound wake restart image is missing")
	}
	evidence, err := captureWakeImageEvidenceFromOpenFile(
		image.file,
		image.executionPath,
		image.evidence.EmbeddedVersion,
		wakeImageMethodFDExec,
	)
	if err != nil {
		return err
	}
	if evidence != image.evidence {
		return fmt.Errorf("bound wake restart image changed during preflight")
	}
	return nil
}

func verifyWakeResumeBoundImagePlatform(bound wakeImageEvidenceV1) (wakeImageEvidenceV1, error) {
	file, err := os.Open("/proc/self/exe")
	if err != nil {
		return wakeImageEvidenceV1{}, fmt.Errorf("open running wake image: %w", err)
	}
	defer func() { _ = file.Close() }()
	actual, err := captureWakeImageEvidenceFromOpenFile(
		file,
		bound.ExecutionPath,
		bound.EmbeddedVersion,
		wakeImageMethodFDExec,
	)
	if err != nil {
		return wakeImageEvidenceV1{}, err
	}
	if actual != bound {
		return wakeImageEvidenceV1{}, fmt.Errorf("running wake image does not match bound restart image")
	}
	return actual, nil
}

func cleanupBoundWakeRestartImagePlatform(wakeRestartBoundImage) error { return nil }

func cleanupWakeResumeBoundImagePlatform(wakeImageEvidenceV1) error { return nil }

func validPreviousWakeRestartBoundImagePlatform(wakeImageEvidenceV1) bool { return false }
