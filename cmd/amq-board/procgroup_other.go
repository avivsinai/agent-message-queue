//go:build !unix

package main

import "os/exec"

// killProcessGroupOnCancel keeps the default cancellation, which kills the
// command's process; WaitDelay still bounds pipes held by descendants.
func killProcessGroupOnCancel(*exec.Cmd) {}
