//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func probeHarnessVersion(ctx context.Context, binaryPath string) (string, bool) {
	cmd := exec.CommandContext(ctx, binaryPath, "--version")
	output := boundedOutput{limit: 4 << 10}
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if !runHarnessProbe(ctx, cmd) {
		return "", false
	}
	return strings.TrimSpace(output.String()), true
}

// runHarnessProbe kills and reaps the probe at the caller's deadline.
// The process group also covers children that inherit its output pipe.
func runHarnessProbe(ctx context.Context, cmd *exec.Cmd) bool {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stopGroup := func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	cmd.Cancel = stopGroup
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return false
	}
	err := cmd.Wait()
	cleanupErr := stopGroup()
	return err == nil && cleanupErr == nil && ctx.Err() == nil
}
