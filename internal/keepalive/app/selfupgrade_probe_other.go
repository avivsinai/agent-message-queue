//go:build !darwin && !linux

package app

import "os/exec"

func configureSelfUpgradeVersionProbe(*exec.Cmd) {}
