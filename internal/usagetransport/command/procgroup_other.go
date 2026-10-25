//go:build !unix

package command

import "os/exec"

// groups is false: without a process group the runner kills as a whole, a
// descendant of the child could outlive the run, so Run starts nothing.
const groups = false

func ownGroup(*exec.Cmd) {}

func killGroup(*exec.Cmd) error { return nil }
