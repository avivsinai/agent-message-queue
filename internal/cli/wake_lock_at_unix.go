//go:build darwin || linux

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

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
