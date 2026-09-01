//go:build darwin

package cli

import "golang.org/x/sys/unix"

func withExistingWakeLifecycleGuardNoWaitInDir(agentDir *wakeAgentDir, fn func(int) error) error {
	return withExistingWakeLifecycleGuardModeInDir(agentDir, unix.LOCK_EX|unix.LOCK_NB, fn)
}
