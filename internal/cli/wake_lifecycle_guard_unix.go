//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

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
	return openWakeDirectory(path, "wake agent directory")
}

func openWakeDirectory(path, label string) (*wakeAgentDir, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s %s: %w", label, path, err)
	}
	if err := validateWakeDirectory(label, path, before); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s %s: %w", label, path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	opened, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat opened %s %s: %w", label, path, err)
	}
	if err := validateWakeDirectory(label, path, opened); err != nil {
		_ = file.Close()
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("re-stat %s %s: %w", label, path, err)
	}
	if err := validateWakeDirectory(label, path, after); err != nil {
		_ = file.Close()
		return nil, err
	}
	// Directory ctime changes for ordinary child creates/removes. Device+inode
	// identity is the stable capability boundary here; the metadata files inside
	// it retain the stricter ctime-aware generation checks.
	if !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = file.Close()
		return nil, newWakeSnapshotReadChangedError(
			fmt.Errorf("%s %s changed while opening", label, path),
		)
	}
	return &wakeAgentDir{path: path, file: file}, nil
}

func validateWakeAgentDir(path string, info os.FileInfo) error {
	return validateWakeDirectory("wake agent directory", path, info)
}

func validateWakeDirectory(label, path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s %s must be a directory, not a symlink", label, path)
	}
	return validateWakeTargetPathOwnership(label, path, info)
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
	return withWakeLifecycleGuardModeInDir(agentDir, unix.LOCK_EX, fn)
}

func withWakeLifecycleGuardModeInDir(
	agentDir *wakeAgentDir,
	lockMode int,
	fn func(int) error,
) error {
	return withWakeLifecycleGuardModeAndTimeoutInDir(
		agentDir,
		lockMode,
		wakeLifecycleGuardRetryTimeout,
		fn,
	)
}

// Reload authentication runs in a handler that Close must be able to drain
// promptly. It refuses a held guard instead of waiting for another owner.
func withWakeLifecycleGuardNoWaitInDir(
	agentDir *wakeAgentDir,
	fn func(int) error,
) error {
	return withWakeLifecycleGuardModeAndTimeoutInDir(
		agentDir,
		unix.LOCK_EX|unix.LOCK_NB,
		0,
		fn,
	)
}

func withWakeLifecycleGuardModeAndTimeoutInDir(
	agentDir *wakeAgentDir,
	lockMode int,
	retryTimeout time.Duration,
	fn func(int) error,
) error {
	return withWakeLifecycleGuardLeaseModeAndTimeoutInDir(
		agentDir,
		false,
		lockMode,
		retryTimeout,
		func(dirfd int, lease *wakeLifecycleGuardLease) (retErr error) {
			defer func() { retErr = errors.Join(retErr, lease.release()) }()
			return fn(dirfd)
		},
	)
}

type wakeLifecycleGuardLease struct {
	file     *os.File
	path     string
	locked   bool
	released bool
}

func (lease *wakeLifecycleGuardLease) release() error {
	if lease == nil || lease.released {
		return nil
	}
	lease.released = true
	if lease.file == nil {
		return nil
	}
	var unlockErr error
	if lease.locked {
		unlockErr = unix.Flock(int(lease.file.Fd()), unix.LOCK_UN)
	}
	closeErr := lease.file.Close()
	return errors.Join(unlockErr, closeErr)
}

func withWakeLifecycleGuardLeaseModeAndTimeoutInDir(
	agentDir *wakeAgentDir,
	existing bool,
	lockMode int,
	retryTimeout time.Duration,
	fn func(int, *wakeLifecycleGuardLease) error,
) error {
	return agentDir.withFD(func(dirfd int) error {
		path := filepath.Join(agentDir.path, wakeLifecycleGuardFileName)
		lease, err := openAndAcquireWakeLifecycleGuardLeaseAt(
			dirfd,
			path,
			agentDir,
			existing,
			lockMode,
			retryTimeout,
		)
		if err != nil {
			return err
		}
		return fn(dirfd, lease)
	})
}

func openAndAcquireWakeLifecycleGuardLeaseAt(
	dirfd int,
	path string,
	agentDir *wakeAgentDir,
	existing bool,
	lockMode int,
	retryTimeout time.Duration,
) (*wakeLifecycleGuardLease, error) {
	var (
		file *os.File
		err  error
	)
	if existing {
		file, err = openExistingWakeLifecycleGuardAt(dirfd, path)
	} else {
		file, err = openWakeLifecycleGuardAt(dirfd, path)
	}
	if err != nil {
		return nil, err
	}
	lease := &wakeLifecycleGuardLease{file: file, path: path}
	if err := acquireWakeLifecycleGuard(file, path, agentDir, lockMode, retryTimeout); err != nil {
		_ = file.Close()
		return nil, err
	}
	lease.locked = true

	info, err := file.Stat()
	if err != nil {
		_ = lease.release()
		return nil, fmt.Errorf("stat wake lifecycle guard %s: %w", path, err)
	}
	if err := validateWakeLifecycleGuard(path, info); err != nil {
		_ = lease.release()
		return nil, err
	}
	var pathFile *os.File
	if existing {
		pathFile, err = openExistingWakeLifecycleGuardAt(dirfd, path)
	} else {
		pathFile, err = openWakeLifecycleGuardAt(dirfd, path)
	}
	if err != nil {
		_ = lease.release()
		return nil, fmt.Errorf("re-open wake lifecycle guard after acquisition: %w", err)
	}
	pathInfo, statErr := pathFile.Stat()
	_ = pathFile.Close()
	if statErr != nil {
		_ = lease.release()
		return nil, fmt.Errorf("stat wake lifecycle guard path %s: %w", path, statErr)
	}
	if !sameWakeFileIdentity(info, pathInfo) {
		_ = lease.release()
		return nil, fmt.Errorf("wake lifecycle guard %s changed while acquiring", path)
	}
	return lease, nil
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

func openExistingWakeLifecycleGuardAt(dirfd int, path string) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_NONBLOCK | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(dirfd, wakeLifecycleGuardFileName, flags, 0)
	if err != nil {
		return nil, fmt.Errorf("open existing wake lifecycle guard %s: %w", path, err)
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

// This boundary is plumbing only. Callers must not turn detached-directory
// access into acquire, repair, or readiness success; it is for exact cleanup
// of old residue while preserving the canonical successor's authority.
func withExistingWakeLifecycleGuardInDir(agentDir *wakeAgentDir, fn func(int) error) error {
	return withExistingWakeLifecycleGuardModeInDir(agentDir, unix.LOCK_EX, fn)
}

func withExistingWakeLifecycleGuardNoWaitInDir(agentDir *wakeAgentDir, fn func(int) error) error {
	return withExistingWakeLifecycleGuardModeInDir(agentDir, unix.LOCK_EX|unix.LOCK_NB, fn)
}

func withExistingWakeLifecycleGuardModeInDir(
	agentDir *wakeAgentDir,
	lockMode int,
	fn func(int) error,
) error {
	return withWakeLifecycleGuardLeaseModeAndTimeoutInDir(
		agentDir,
		true,
		lockMode,
		wakeLifecycleGuardRetryTimeout,
		func(dirfd int, lease *wakeLifecycleGuardLease) (retErr error) {
			defer func() { retErr = errors.Join(retErr, lease.release()) }()
			return fn(dirfd)
		},
	)
}

const (
	wakeLifecycleGuardRetryInterval = 10 * time.Millisecond
	wakeLifecycleGuardRetryTimeout  = 500 * time.Millisecond
)

func acquireWakeLifecycleGuard(
	file *os.File,
	path string,
	agentDir *wakeAgentDir,
	lockMode int,
	retryTimeout time.Duration,
) error {
	if lockMode&unix.LOCK_NB == 0 {
		if err := unix.Flock(int(file.Fd()), lockMode); err != nil {
			return fmt.Errorf("acquire wake lifecycle guard %s: %w", path, err)
		}
		return nil
	}
	deadline := time.Now().Add(retryTimeout)
	for {
		err := unix.Flock(int(file.Fd()), lockMode)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("acquire wake lifecycle guard %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			root := filepath.Dir(filepath.Dir(agentDir.path))
			agent := filepath.Base(agentDir.path)
			remedy := wakeCheckRemedy(root, agent).String()
			return fmt.Errorf(
				"wake lifecycle guard %s is held by another process; holder is unknown after %s; inspect with %s or escalate manually",
				path,
				retryTimeout,
				remedy,
			)
		}
		time.Sleep(wakeLifecycleGuardRetryInterval)
	}
}

func wakeLifecycleGuardMissingAt(agentDir *wakeAgentDir) (bool, error) {
	missing := false
	err := agentDir.withFD(func(dirfd int) error {
		var info unix.Stat_t
		err := unix.Fstatat(
			dirfd,
			wakeLifecycleGuardFileName,
			&info,
			unix.AT_SYMLINK_NOFOLLOW,
		)
		if err == unix.ENOENT {
			missing = true
			return nil
		}
		return err
	})
	return missing, err
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
