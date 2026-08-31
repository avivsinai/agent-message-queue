//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// wakeMutationScope owns a lifecycle guard lease when present and the retained
// directory descriptor for one mutation callback. Its lifetime ends before the
// guard is unlocked, so an escaped scope cannot commit an effect after its
// authority is released.
type wakeMutationScope struct {
	agentDir *wakeAgentDir
	dirfd    int
	guard    *wakeLifecycleGuardLease
	active   *atomic.Bool
}

var wakeRetireUnlinkWakeLockAt = unix.Unlinkat

func newWakeMutationScope(
	agentDir *wakeAgentDir,
	dirfd int,
	guard *wakeLifecycleGuardLease,
) *wakeMutationScope {
	active := &atomic.Bool{}
	active.Store(true)
	return &wakeMutationScope{
		agentDir: agentDir,
		dirfd:    dirfd,
		guard:    guard,
		active:   active,
	}
}

func (scope *wakeMutationScope) release() error {
	if scope == nil {
		return unix.EINVAL
	}
	if scope.active != nil {
		scope.active.Store(false)
	}
	guard := scope.guard
	scope.guard = nil
	if guard == nil {
		return nil
	}
	return guard.release()
}

func (scope *wakeMutationScope) requireActive() error {
	if scope == nil || scope.active == nil || !scope.active.Load() {
		return fmt.Errorf("wake mutation scope is inactive")
	}
	if scope.guard != nil && (scope.guard.file == nil || scope.guard.released) {
		return fmt.Errorf("wake mutation scope lifecycle guard is unavailable")
	}
	if scope.agentDir == nil || scope.agentDir.file == nil {
		return unix.EINVAL
	}
	if scope.dirfd != int(scope.agentDir.file.Fd()) {
		return fmt.Errorf("wake mutation scope directory capability does not match retained descriptor")
	}
	return nil
}

func (scope *wakeMutationScope) location() (int, *wakeAgentDir, error) {
	if err := scope.requireActive(); err != nil {
		return 0, nil, err
	}
	return scope.dirfd, scope.agentDir, nil
}

func withWakeMutationScopeInDir(
	agentDir *wakeAgentDir,
	fn func(*wakeMutationScope) error,
) error {
	return withWakeLifecycleGuardLeaseModeAndTimeoutInDir(
		agentDir,
		false,
		unix.LOCK_EX,
		wakeLifecycleGuardRetryTimeout,
		func(dirfd int, guard *wakeLifecycleGuardLease) (retErr error) {
			scope := newWakeMutationScope(agentDir, dirfd, guard)
			defer func() { retErr = errors.Join(retErr, scope.release()) }()
			return fn(scope)
		},
	)
}

func withWakeMutationScopeNoWaitInDir(
	agentDir *wakeAgentDir,
	fn func(*wakeMutationScope) error,
) error {
	return withWakeLifecycleGuardLeaseModeAndTimeoutInDir(
		agentDir,
		false,
		unix.LOCK_EX|unix.LOCK_NB,
		wakeLifecycleGuardRetryTimeout,
		func(dirfd int, guard *wakeLifecycleGuardLease) (retErr error) {
			scope := newWakeMutationScope(agentDir, dirfd, guard)
			defer func() { retErr = errors.Join(retErr, scope.release()) }()
			return fn(scope)
		},
	)
}

func withExistingWakeMutationScopeInDir(
	agentDir *wakeAgentDir,
	fn func(*wakeMutationScope) error,
) error {
	return withWakeLifecycleGuardLeaseModeAndTimeoutInDir(
		agentDir,
		true,
		unix.LOCK_EX,
		wakeLifecycleGuardRetryTimeout,
		func(dirfd int, guard *wakeLifecycleGuardLease) (retErr error) {
			scope := newWakeMutationScope(agentDir, dirfd, guard)
			defer func() { retErr = errors.Join(retErr, scope.release()) }()
			return fn(scope)
		},
	)
}

func withExistingWakeMutationScopeNoWaitInDir(
	agentDir *wakeAgentDir,
	fn func(*wakeMutationScope) error,
) error {
	return withWakeLifecycleGuardLeaseModeAndTimeoutInDir(
		agentDir,
		true,
		unix.LOCK_EX|unix.LOCK_NB,
		wakeLifecycleGuardRetryTimeout,
		func(dirfd int, guard *wakeLifecycleGuardLease) (retErr error) {
			scope := newWakeMutationScope(agentDir, dirfd, guard)
			defer func() { retErr = errors.Join(retErr, scope.release()) }()
			return fn(scope)
		},
	)
}

func (scope *wakeMutationScope) unlinkWakeLock() error {
	dirfd, _, err := scope.location()
	if err != nil {
		return err
	}
	if err := scope.requireCanonical(); err != nil {
		return err
	}
	return unix.Unlinkat(dirfd, wakeLockFileName, 0)
}

func (scope *wakeMutationScope) unlinkWakeLockForCleanup() error {
	dirfd, _, err := scope.location()
	if err != nil {
		return err
	}
	if _, err := scope.requireCanonicalOrDetached(); err != nil {
		return err
	}
	return unix.Unlinkat(dirfd, wakeLockFileName, 0)
}

func (scope *wakeMutationScope) unlinkWakeLockForRetire() error {
	dirfd, _, err := scope.location()
	if err != nil {
		return err
	}
	if err := scope.requireCanonical(); err != nil {
		return err
	}
	return wakeRetireUnlinkWakeLockAt(dirfd, wakeLockFileName, 0)
}

func assertNotWakeLockName(name string) error {
	if name == wakeLockFileName {
		return fmt.Errorf("refuse non-lock cleanup of %s", wakeLockFileName)
	}
	return nil
}

func (scope *wakeMutationScope) requireCanonical() error {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return err
	}
	return validateWakeStateAgentDirAt(dirfd, agentDir)
}

func (scope *wakeMutationScope) requireCanonicalOrDetached() (wakeAgentDirRelation, error) {
	dirfd, agentDir, err := scope.location()
	if err != nil {
		return wakeAgentDirInconclusive, err
	}
	relation, err := retainedWakeAgentDirRelationAt(agentDir, dirfd)
	if err != nil {
		return wakeAgentDirInconclusive, fmt.Errorf("wake agent directory relation is inconclusive before cleanup: %w", err)
	}
	switch relation {
	case wakeAgentDirCanonical:
		if err := validateWakeStateAgentDirAt(dirfd, agentDir); err != nil {
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
// directory from a namespace lookup that failed for an unknown reason. It
// reacquires agentDir.withFD, so call it only when no withFD callback is active;
// use retainedWakeAgentDirRelationAt inside one.
func retainedWakeAgentDirRelation(agentDir *wakeAgentDir) (wakeAgentDirRelation, error) {
	if agentDir == nil {
		return wakeAgentDirInconclusive, fmt.Errorf("wake agent directory capability is missing")
	}
	var relation wakeAgentDirRelation
	err := agentDir.withFD(func(dirfd int) error {
		var err error
		relation, err = retainedWakeAgentDirRelationAt(agentDir, dirfd)
		return err
	})
	return relation, err
}

// retainedWakeAgentDirRelationAt inspects an already-held directory descriptor
// without acquiring agentDir.mu. Use it from an active withFD callback.
func retainedWakeAgentDirRelationAt(
	agentDir *wakeAgentDir,
	dirfd int,
) (wakeAgentDirRelation, error) {
	if agentDir == nil || agentDir.file == nil {
		return wakeAgentDirInconclusive, fmt.Errorf("wake agent directory capability is missing")
	}
	if dirfd != int(agentDir.file.Fd()) {
		return wakeAgentDirInconclusive, fmt.Errorf("wake agent directory descriptor does not match retained capability")
	}
	retainedInfo, err := agentDir.file.Stat()
	if err != nil {
		return wakeAgentDirInconclusive, fmt.Errorf("stat retained wake agent directory %s: %w", agentDir.path, err)
	}
	canonicalFD, err := unix.Open(
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
	canonical := os.NewFile(uintptr(canonicalFD), agentDir.path)
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
