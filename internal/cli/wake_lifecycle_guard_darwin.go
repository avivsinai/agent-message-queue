//go:build darwin

package cli

import (
	"errors"

	"golang.org/x/sys/unix"
)

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
