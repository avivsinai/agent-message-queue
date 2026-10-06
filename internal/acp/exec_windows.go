//go:build windows

package acp

import "os/exec"

// canExecute is true on Windows, where there is no execute bit.
func canExecute(string) bool { return true }

// ownProcessGroup leaves cmd unchanged on Windows; WaitDelay still bounds
// the wait for its pipes.
func ownProcessGroup(*exec.Cmd) {}
