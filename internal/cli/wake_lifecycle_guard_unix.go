//go:build darwin || linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"golang.org/x/sys/unix"
)

const wakeLifecycleGuardFileName = ".wake.lifecycle.lock"

type wakeAgentDir struct {
	path   string
	file   *os.File
	mu     sync.RWMutex
	closed bool
}

func wakeLifecycleGuardPath(root, me string) string {
	return filepath.Join(fsq.AgentBase(root, me), wakeLifecycleGuardFileName)
}

func openWakeAgentDir(root, me string) (*wakeAgentDir, error) {
	if err := fsq.ValidateHandle(me); err != nil {
		return nil, err
	}
	path := fsq.AgentBase(root, me)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("create wake agent directory %s: %w", path, err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat wake agent directory %s: %w", path, err)
	}
	if err := validateWakeAgentDir(path, before); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open wake agent directory %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat opened wake agent directory %s: %w", path, err)
	}
	if err := validateWakeAgentDir(path, opened); err != nil {
		_ = file.Close()
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("re-stat wake agent directory %s: %w", path, err)
	}
	if err := validateWakeAgentDir(path, after); err != nil {
		_ = file.Close()
		return nil, err
	}
	// Directory ctime changes for ordinary child creates/removes. Device+inode
	// identity is the stable capability boundary here; the metadata files inside
	// it retain the stricter ctime-aware generation checks.
	if !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = file.Close()
		return nil, fmt.Errorf("wake agent directory %s changed while opening", path)
	}
	return &wakeAgentDir{path: path, file: file}, nil
}

func validateWakeAgentDir(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("wake agent directory %s must be a directory, not a symlink", path)
	}
	return validateWakeTargetPathOwnership("wake agent directory", path, info)
}

func (d *wakeAgentDir) withFD(fn func(int) error) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return fmt.Errorf("wake agent directory %s is closed", d.path)
	}
	return fn(int(d.file.Fd()))
}

func (d *wakeAgentDir) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	return d.file.Close()
}

// Lock order: lifecycle guard -> wake lock/target/ready reads and mutations.
// Release the lifecycle guard before any child wait, pidfd exit wait, or
// control wait. Child/cooperative wake paths reacquire it for exact-generation
// cleanup and final readiness publication. Never wait on a child while holding
// the lifecycle guard.
func withWakeLifecycleGuard(root, me string, fn func() error) error {
	agentDir, err := openWakeAgentDir(root, me)
	if err != nil {
		return err
	}
	defer func() { _ = agentDir.Close() }()
	return withWakeLifecycleGuardInDir(agentDir, func(int) error { return fn() })
}

func withWakeLifecycleGuardInDir(agentDir *wakeAgentDir, fn func(int) error) error {
	return agentDir.withFD(func(dirfd int) error {
		path := filepath.Join(agentDir.path, wakeLifecycleGuardFileName)
		file, err := openWakeLifecycleGuardAt(dirfd, path)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()

		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
			return fmt.Errorf("acquire wake lifecycle guard %s: %w", path, err)
		}
		defer func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }()

		info, err := file.Stat()
		if err != nil {
			return fmt.Errorf("stat wake lifecycle guard %s: %w", path, err)
		}
		if err := validateWakeLifecycleGuard(path, info); err != nil {
			return err
		}
		pathFile, err := openWakeLifecycleGuardAt(dirfd, path)
		if err != nil {
			return fmt.Errorf("re-open wake lifecycle guard after acquisition: %w", err)
		}
		pathInfo, statErr := pathFile.Stat()
		_ = pathFile.Close()
		if statErr != nil {
			return fmt.Errorf("stat wake lifecycle guard path %s: %w", path, statErr)
		}
		if !sameWakeFileIdentity(info, pathInfo) {
			return fmt.Errorf("wake lifecycle guard %s changed while acquiring", path)
		}
		return fn(dirfd)
	})
}

func openWakeLifecycleGuardAt(dirfd int, path string) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_NONBLOCK | unix.O_NOFOLLOW | unix.O_CLOEXEC
	var fd int
	var err error
	for attempt := 0; attempt < 100; attempt++ {
		fd, err = unix.Openat(dirfd, wakeLifecycleGuardFileName, flags, 0)
		if err == nil {
			break
		}
		if err != unix.ENOENT {
			return nil, fmt.Errorf("open wake lifecycle guard %s: %w", path, err)
		}
		fd, err = unix.Openat(dirfd, wakeLifecycleGuardFileName, flags|unix.O_CREAT|unix.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if err != unix.EEXIST {
			return nil, fmt.Errorf("create wake lifecycle guard %s: %w", path, err)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("open wake lifecycle guard %s after concurrent creation: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat wake lifecycle guard %s: %w", path, statErr)
	}
	if validateErr := validateWakeLifecycleGuard(path, info); validateErr != nil {
		_ = file.Close()
		return nil, validateErr
	}
	return file, nil
}

func validateWakeLifecycleGuard(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("wake lifecycle guard %s must be a regular file", path)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		return fmt.Errorf("wake lifecycle guard %s mode is %o, want 0600", path, got)
	}
	return validateWakeTargetPathOwnership("wake lifecycle guard", path, info)
}
