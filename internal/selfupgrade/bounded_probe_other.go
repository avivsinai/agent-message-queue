//go:build !darwin && !linux

package selfupgrade

import "os/exec"

func configureBoundedProbe(*exec.Cmd) {}
