//go:build darwin

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNewWakeLockRecordsDarwinControllingTTYUnderRealPTY(t *testing.T) {
	const helperEnv = "AMQ_TEST_DARWIN_CURRENT_TTY_PTY"
	if os.Getenv(helperEnv) == "1" {
		lock, err := newWakeLock(t.TempDir(), "codex", wakeLockAcquireOptions{wakeMode: wakeInjectModeRaw})
		if err != nil {
			t.Fatalf("create wake lock metadata: %v", err)
		}
		if !strings.HasPrefix(lock.TTY, "/dev/ttys") {
			t.Fatalf("wake lock tty = %q, want /dev/ttys*", lock.TTY)
		}
		info, err := os.Stat(lock.TTY)
		if err != nil {
			t.Fatalf("stat recorded tty %q: %v", lock.TTY, err)
		}
		if info.Mode()&os.ModeCharDevice == 0 {
			t.Fatalf("recorded tty %q mode = %v, want character device", lock.TTY, info.Mode())
		}
		return
	}

	if _, err := os.Stat("/usr/bin/script"); err != nil {
		t.Skipf("Darwin PTY regression requires /usr/bin/script: %v", err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(
		ctx,
		"/usr/bin/script",
		"-q",
		"/dev/null",
		testBinary,
		"-test.run=^TestNewWakeLockRecordsDarwinControllingTTYUnderRealPTY$",
	)
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err = cmd.Run()
	evidencePath := filepath.Join(t.TempDir(), "darwin-current-tty-pty.log")
	if writeErr := os.WriteFile(evidencePath, output.Bytes(), 0o600); writeErr != nil {
		t.Fatalf("write PTY evidence: %v", writeErr)
	}
	t.Logf("PTY evidence: %s", evidencePath)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("Darwin current-TTY regression timed out; evidence=%s\n%s", evidencePath, output.String())
	}
	if err != nil {
		t.Fatalf("Darwin current-TTY regression: %v; evidence=%s\n%s", err, evidencePath, output.String())
	}
}
