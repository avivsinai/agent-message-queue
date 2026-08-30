//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// wakeMutationScope is the only production capability that owns a lifecycle
// guard and the retained directory descriptor while a wake mutation commits.
// Callers keep all waits outside its callback and use action methods for raw
// effects.
type wakeMutationScope struct {
	agentDir *wakeAgentDir
	dirfd    int
}

func newWakeMutationScope(agentDir *wakeAgentDir, dirfd int) *wakeMutationScope {
	return &wakeMutationScope{agentDir: agentDir, dirfd: dirfd}
}

func withWakeMutationScopeInDir(
	agentDir *wakeAgentDir,
	fn func(*wakeMutationScope) error,
) error {
	return withWakeMutationScopeModeInDir(agentDir, unix.LOCK_EX, fn)
}

func withWakeMutationScopeModeInDir(
	agentDir *wakeAgentDir,
	lockMode int,
	fn func(*wakeMutationScope) error,
) error {
	return withWakeLifecycleGuardModeInDir(agentDir, lockMode, func(dirfd int) error {
		return fn(newWakeMutationScope(agentDir, dirfd))
	})
}

func withExistingWakeMutationScopeInDir(
	agentDir *wakeAgentDir,
	fn func(*wakeMutationScope) error,
) error {
	return withExistingWakeMutationScopeModeInDir(agentDir, unix.LOCK_EX, fn)
}

func withExistingWakeMutationScopeModeInDir(
	agentDir *wakeAgentDir,
	lockMode int,
	fn func(*wakeMutationScope) error,
) error {
	return withExistingWakeLifecycleGuardModeInDir(agentDir, lockMode, func(dirfd int) error {
		return fn(newWakeMutationScope(agentDir, dirfd))
	})
}

func withWakeMutationScopeOrRetainedDir(
	agentDir *wakeAgentDir,
	allowMissing bool,
	lockMode int,
	fn func(*wakeMutationScope) error,
) error {
	if !allowMissing {
		return withExistingWakeMutationScopeModeInDir(agentDir, lockMode, fn)
	}
	missing, err := wakeLifecycleGuardMissingAt(agentDir)
	if err != nil {
		return err
	}
	if missing {
		return withWakeMutationScopeModeInDir(agentDir, lockMode, fn)
	}
	return withExistingWakeMutationScopeModeInDir(agentDir, lockMode, fn)
}

func (scope *wakeMutationScope) unlinkWakeLock() error {
	if err := scope.requireCanonical(); err != nil {
		return err
	}
	return unix.Unlinkat(scope.dirfd, wakeLockFileName, 0)
}

func (scope *wakeMutationScope) unlinkWakeLockWith(
	unlink func(int, string, int) error,
) error {
	if unlink == nil {
		return unix.EINVAL
	}
	if err := scope.requireCanonical(); err != nil {
		return err
	}
	return unlink(scope.dirfd, wakeLockFileName, 0)
}

// unlinkWakeLockForCleanup permits only exact cleanup in either the canonical
// retained directory or a directory proven detached from its canonical path.
// It never selects or signals a successor claim.
func (scope *wakeMutationScope) unlinkWakeLockForCleanup() error {
	if _, err := scope.requireCanonicalOrDetached(); err != nil {
		return err
	}
	return unix.Unlinkat(scope.dirfd, wakeLockFileName, 0)
}

func assertNotWakeLockName(name string) error {
	if name == wakeLockFileName {
		return fmt.Errorf("refuse non-lock cleanup of %s", wakeLockFileName)
	}
	return nil
}

func (scope *wakeMutationScope) unlinkLifecycleGuard() error {
	if _, err := scope.requireCanonicalOrDetached(); err != nil {
		return err
	}
	err := unix.Unlinkat(scope.dirfd, wakeLifecycleGuardFileName, 0)
	if err == unix.ENOENT {
		return nil
	}
	return err
}

func (scope *wakeMutationScope) requireCanonical() error {
	if scope == nil || scope.agentDir == nil {
		return unix.EINVAL
	}
	return validateWakeStateAgentDirAt(scope.dirfd, scope.agentDir)
}

func (scope *wakeMutationScope) requireCanonicalOrDetached() (wakeAgentDirRelation, error) {
	if scope == nil || scope.agentDir == nil || scope.agentDir.file == nil {
		return wakeAgentDirInconclusive, unix.EINVAL
	}
	if scope.dirfd != int(scope.agentDir.file.Fd()) {
		return wakeAgentDirInconclusive, fmt.Errorf("wake mutation scope directory capability does not match retained descriptor")
	}
	relation, err := retainedWakeAgentDirRelation(scope.agentDir)
	if err != nil {
		return wakeAgentDirInconclusive, fmt.Errorf("wake agent directory relation is inconclusive before cleanup: %w", err)
	}
	switch relation {
	case wakeAgentDirCanonical:
		if err := scope.requireCanonical(); err != nil {
			return wakeAgentDirInconclusive, err
		}
		return wakeAgentDirCanonical, nil
	case wakeAgentDirDetached:
		return wakeAgentDirDetached, nil
	case wakeAgentDirInconclusive:
		return wakeAgentDirInconclusive, fmt.Errorf("wake agent directory relation is inconclusive before cleanup")
	default:
		return wakeAgentDirInconclusive, fmt.Errorf("unknown wake agent directory relation %d", relation)
	}
}

type wakeAgentDirRelation uint8

const (
	wakeAgentDirInconclusive wakeAgentDirRelation = iota
	wakeAgentDirCanonical
	wakeAgentDirDetached
)

// retainedWakeAgentDirRelation distinguishes a proven detached retained
// directory from a namespace lookup that failed for an unknown reason.
func retainedWakeAgentDirRelation(agentDir *wakeAgentDir) (wakeAgentDirRelation, error) {
	if agentDir == nil || agentDir.file == nil {
		return wakeAgentDirInconclusive, fmt.Errorf("wake agent directory capability is missing")
	}
	var retainedInfo os.FileInfo
	if err := agentDir.withFD(func(int) error {
		info, err := agentDir.file.Stat()
		if err != nil {
			return fmt.Errorf("stat retained wake agent directory %s: %w", agentDir.path, err)
		}
		retainedInfo = info
		return nil
	}); err != nil {
		return wakeAgentDirInconclusive, err
	}

	fd, err := unix.Open(
		agentDir.path,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return wakeAgentDirDetached, nil
		}
		return wakeAgentDirInconclusive, fmt.Errorf(
			"open canonical wake agent directory %s: %w",
			agentDir.path,
			err,
		)
	}
	canonical := os.NewFile(uintptr(fd), agentDir.path)
	defer func() { _ = canonical.Close() }()
	canonicalInfo, err := canonical.Stat()
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return wakeAgentDirDetached, nil
		}
		return wakeAgentDirInconclusive, fmt.Errorf(
			"stat canonical wake agent directory %s: %w",
			agentDir.path,
			err,
		)
	}
	if os.SameFile(retainedInfo, canonicalInfo) {
		return wakeAgentDirCanonical, nil
	}
	return wakeAgentDirDetached, nil
}

func canonicalWakeAgentDirPathPresent(agentDir *wakeAgentDir) (bool, error) {
	if agentDir == nil || agentDir.file == nil {
		return false, fmt.Errorf("wake agent directory capability is missing")
	}
	fd, err := unix.Open(
		agentDir.path,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("open canonical wake agent directory %s: %w", agentDir.path, err)
	}
	if err := unix.Close(fd); err != nil {
		return false, fmt.Errorf("close canonical wake agent directory %s: %w", agentDir.path, err)
	}
	return true, nil
}
