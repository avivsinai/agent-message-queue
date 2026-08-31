//go:build linux

package cli

import "golang.org/x/sys/unix"

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
