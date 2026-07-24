//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func readWakeLockFileAt(dirfd int, path string) ([]byte, os.FileInfo, error) {
	open := func() (*os.File, error) {
		fd, err := unix.Openat(dirfd, ".wake.lock", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), path), nil
	}
	file, err := open()
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat wake lock: %w", err)
	}
	if err := validateWakeLockFile(path, info); err != nil {
		return nil, nil, err
	}
	data, err := readWakeMetadata(file, "wake lock", path)
	if err != nil {
		return nil, nil, err
	}
	pathFile, err := open()
	if err != nil {
		return nil, nil, err
	}
	pathInfo, statErr := pathFile.Stat()
	_ = pathFile.Close()
	if statErr != nil {
		return nil, nil, fmt.Errorf("re-stat wake lock: %w", statErr)
	}
	if err := validateWakeLockFile(path, pathInfo); err != nil {
		return nil, nil, err
	}
	if !sameWakeFileIdentity(info, pathInfo) {
		return nil, nil, fmt.Errorf("wake lock %s changed while opening", path)
	}
	return data, info, nil
}

func inspectWakeLockAt(dirfd int, agentDir *wakeAgentDir, root, me string) wakeLockInspection {
	path := filepath.Join(agentDir.path, ".wake.lock")
	return inspectWakeLockWithReader(root, me, path, func() ([]byte, os.FileInfo, error) {
		return readWakeLockFileAt(dirfd, path)
	})
}

func readWakeLockMetadataAt(dirfd int, agentDir *wakeAgentDir, root, me string) wakeLockInspection {
	path := filepath.Join(agentDir.path, ".wake.lock")
	return readWakeLockMetadataWithReader(root, me, path, func() ([]byte, os.FileInfo, error) {
		return readWakeLockFileAt(dirfd, path)
	})
}

func createWakeLockAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	lock wakeLock,
) error {
	if strings.TrimSpace(lock.Generation) == "" {
		return fmt.Errorf("wake lock generation is missing")
	}
	if canonicalWakeRoot(lock.Root) != canonicalWakeRoot(root) {
		return fmt.Errorf("wake lock root mismatch")
	}
	if lock.Agent != me {
		return fmt.Errorf("wake lock agent mismatch")
	}
	data, err := json.Marshal(lock)
	if err != nil {
		return fmt.Errorf("marshal wake lock: %w", err)
	}
	fd, err := unix.Openat(
		dirfd,
		".wake.lock",
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("failed to create wake lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(agentDir.path, ".wake.lock"))
	createdInfo, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return fmt.Errorf("stat created wake lock: %w", statErr)
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			currentFD, openErr := unix.Openat(
				dirfd,
				".wake.lock",
				unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
				0,
			)
			if openErr == nil {
				currentFile := os.NewFile(uintptr(currentFD), filepath.Join(agentDir.path, ".wake.lock"))
				currentInfo, currentErr := currentFile.Stat()
				_ = currentFile.Close()
				if currentErr == nil && sameWakeFileIdentity(createdInfo, currentInfo) {
					_ = unix.Unlinkat(dirfd, ".wake.lock", 0)
					_ = syncWakeOwnerDirFD(dirfd)
				}
			}
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod created wake lock: %w", err)
	}
	n, err := file.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write wake lock: %w", err)
	}
	if n != len(data) {
		return fmt.Errorf("failed to write wake lock: %w", io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync wake lock: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close wake lock: %w", err)
	}
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return fmt.Errorf("sync wake lock directory after commit: %w", err)
	}
	created := readWakeLockMetadataAt(dirfd, agentDir, root, me)
	if !created.Exists ||
		created.Lock.Generation != lock.Generation ||
		!bytes.Equal(created.raw, data) {
		return fmt.Errorf("failed to verify created wake lock generation")
	}
	committed = true
	return nil
}

func createWakeRepairLockAt(
	dirfd int,
	agentDir *wakeAgentDir,
	root string,
	me string,
	rootIdentity string,
	lock wakeLock,
) error {
	if err := revalidateWakeRepairRootIdentity(root, rootIdentity); err != nil {
		return err
	}
	return createWakeLockAt(dirfd, agentDir, root, me, lock)
}

func removeWakeLockIfUnchangedGuardedAt(
	dirfd int,
	agentDir *wakeAgentDir,
	inspection wakeLockInspection,
) error {
	path := filepath.Join(agentDir.path, ".wake.lock")
	return removeWakeLockIfUnchangedGuardedWithIO(
		inspection,
		func() ([]byte, os.FileInfo, error) { return readWakeLockFileAt(dirfd, path) },
		func() error { return unix.Unlinkat(dirfd, ".wake.lock", 0) },
	)
}

func readWakeGenerationFileAt(
	dirfd int,
	agentDir *wakeAgentDir,
	name string,
	label string,
) (wakeReady, bool, error) {
	path := filepath.Join(agentDir.path, name)
	open := func() (*os.File, error) {
		fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), path), nil
	}
	file, err := open()
	if err != nil {
		if err == unix.ENOENT {
			return wakeReady{}, false, nil
		}
		return wakeReady{}, true, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return wakeReady{}, true, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return wakeReady{}, true, fmt.Errorf("%s must be a regular 0600 file", label)
	}
	if err := validateWakeTargetPathOwnership(label, path, info); err != nil {
		return wakeReady{}, true, err
	}
	data, err := readWakeMetadata(file, label, path)
	if err != nil {
		return wakeReady{}, true, err
	}
	pathFile, err := open()
	if err != nil {
		return wakeReady{}, true, err
	}
	pathInfo, statErr := pathFile.Stat()
	_ = pathFile.Close()
	if statErr != nil {
		return wakeReady{}, true, statErr
	}
	if !sameWakeFileIdentity(info, pathInfo) {
		return wakeReady{}, true, fmt.Errorf("%s changed while opening", label)
	}
	var marker wakeReady
	if err := json.Unmarshal(data, &marker); err != nil {
		return wakeReady{}, true, err
	}
	if marker.Schema != wakeReadySchema || marker.Generation == "" {
		return wakeReady{}, true, fmt.Errorf("%s schema is unsupported", label)
	}
	return marker, true, nil
}

func writeWakeGenerationFileAt(
	dirfd int,
	name string,
	label string,
	marker wakeReady,
) error {
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", label, err)
	}
	temp, err := writeWakeOwnerTempAt(dirfd, "wake-generation", append(data, '\n'), 0o600)
	if err != nil {
		return err
	}
	tempPresent := true
	defer func() {
		if tempPresent {
			_ = unix.Unlinkat(dirfd, temp, 0)
		}
	}()
	if err := unix.Renameat(dirfd, temp, dirfd, name); err != nil {
		return fmt.Errorf("install %s: %w", label, err)
	}
	tempPresent = false
	if err := syncWakeOwnerDirFD(dirfd); err != nil {
		return fmt.Errorf("sync %s directory: %w", label, err)
	}
	return nil
}
