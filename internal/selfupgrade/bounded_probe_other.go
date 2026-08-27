//go:build !darwin && !linux

package selfupgrade

import "os/exec"

func configureBoundedProbe(*exec.Cmd) {}

func cleanupBoundedProbeProcessGroup(int) error { return nil }
