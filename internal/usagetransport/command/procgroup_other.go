//go:build !unix

package command

import "os/exec"

// ownGroup keeps the default cancellation, which kills the command's
// process; WaitDelay still bounds pipes held by descendants.
func ownGroup(*exec.Cmd) {}
