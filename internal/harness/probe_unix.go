//go:build darwin || linux

package harness

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ProbeVersion runs binaryPath --version and returns its trimmed output,
// bounded to 4 KiB. ok is false when the probe fails or outlives ctx.
func ProbeVersion(ctx context.Context, binaryPath string) (string, bool) {
	cmd := exec.CommandContext(ctx, binaryPath, "--version")
	output := BoundedOutput{Limit: 4 << 10}
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if !RunProbe(ctx, cmd) {
		return "", false
	}
	return strings.TrimSpace(output.String()), true
}

// RunProbe kills and reaps the probe at the caller's deadline.
// The process group also covers children that inherit its output pipe.
func RunProbe(ctx context.Context, cmd *exec.Cmd) bool {
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

// BoundedOutput is a command output buffer that refuses to grow past
// Limit bytes; a write past it keeps what fits and fails.
type BoundedOutput struct {
	bytes.Buffer
	Limit int
}

func (output *BoundedOutput) Write(data []byte) (int, error) {
	if output.Len()+len(data) > output.Limit {
		remaining := output.Limit - output.Len()
		if remaining > 0 {
			_, _ = output.Buffer.Write(data[:remaining])
		}
		return remaining, errors.New("command output exceeds the safety limit")
	}
	return output.Buffer.Write(data)
}
