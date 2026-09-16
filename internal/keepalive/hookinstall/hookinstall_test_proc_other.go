//go:build !darwin && !linux

package hookinstall

import "os/exec"

// setProcessGroup is a no-op on non-Unix platforms; exec.CommandContext
// handles process tree cleanup via the OS job object on Windows.
func setProcessGroup(_ *exec.Cmd) {}

// killProcessGroup kills just the process on non-Unix platforms.
func killProcessGroup(pid int) error {
	// On Windows, the process is already killed by exec.CommandContext.
	// This is a fallback that does nothing extra.
	return nil
}
