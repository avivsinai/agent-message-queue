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

	"golang.org/x/sys/unix"
)

func TestFindDarwinTTYPath(t *testing.T) {
	tests := []struct {
		name  string
		rdev  int32
		stats map[string]unix.Stat_t
		want  string
	}{
		{
			name: "matches ttys device",
			rdev: 42,
			stats: map[string]unix.Stat_t{
				"/dev/ttys001": {Mode: unix.S_IFCHR, Rdev: 7},
				"/dev/ttys002": {Mode: unix.S_IFCHR, Rdev: 42},
				"/dev/console": {Mode: unix.S_IFCHR, Rdev: 9},
			},
			want: "/dev/ttys002",
		},
		{
			name: "matches console fallback",
			rdev: 9,
			stats: map[string]unix.Stat_t{
				"/dev/ttys001": {Mode: unix.S_IFCHR, Rdev: 7},
				"/dev/console": {Mode: unix.S_IFCHR, Rdev: 9},
			},
			want: "/dev/console",
		},
		{
			name: "rejects non character device",
			rdev: 42,
			stats: map[string]unix.Stat_t{
				"/dev/ttys001": {Mode: unix.S_IFREG, Rdev: 42},
			},
		},
		{name: "no device match", rdev: 42},
	}
	paths := []string{"/dev/ttys001", "/dev/ttys002", "/dev/console"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findDarwinTTYPath(tt.rdev, paths, func(path string, stat *unix.Stat_t) error {
				value, ok := tt.stats[path]
				if !ok {
					return os.ErrNotExist
				}
				*stat = value
				return nil
			})
			if got != tt.want {
				t.Fatalf("path = %q, want %q", got, tt.want)
			}
		})
	}
}

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

func TestGetCurrentTTYDarwinReturnsEmptyWithoutControllingTerminal(t *testing.T) {
	const helperEnv = "AMQ_TEST_DARWIN_NO_CURRENT_TTY"
	if os.Getenv(helperEnv) == "1" {
		if tty := getCurrentTTY(); tty != "" {
			t.Fatalf("detached process current tty = %q, want empty", tty)
		}
		return
	}

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(testBinary, "-test.run=^TestGetCurrentTTYDarwinReturnsEmptyWithoutControllingTerminal$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("detached current-TTY regression: %v\n%s", err, output.String())
	}
}
