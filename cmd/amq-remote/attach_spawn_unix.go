//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// startEndpoint launches `amq-remote up --root <root>` in its own session so
// it outlives the command that started it. Its output goes to up.log in the
// state directory.
func startEndpoint(root, stateDir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(stateDir, "up.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	cmd := exec.Command(self, "up", "--root", root)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
