//go:build !unix

package main

import "os/exec"

// killProcessGroupOnCancel keeps the default cancellation, which kills the
// command's process; WaitDelay still bounds pipes held by descendants.
func killProcessGroupOnCancel(*exec.Cmd) {}

// pidAlive cannot probe a process here, so every recorded serve counts as
// alive; a client checks the pid itself.
func pidAlive(int) bool { return true }
