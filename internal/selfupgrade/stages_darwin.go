//go:build darwin

package selfupgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const selfUpgradeStagePIDProbeTimeout = 5 * time.Second

var (
	selfUpgradeStagePIDKill         = syscall.Kill
	selfUpgradeStagePSPath          = "/bin/ps"
	selfUpgradeStageRunBoundedProbe = RunBoundedProbe
)

func selfUpgradeStagePrefix(locator string) string {
	return "." + filepath.Base(locator) + ".amq-self-upgrade-"
}

func cleanupImageStagesPlatform(locator string) error {
	dir := filepath.Dir(locator)
	prefix := selfUpgradeStagePrefix(locator)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		pid, ok := selfUpgradeStagePID(entry.Name(), prefix)
		if !ok || selfUpgradeStagePIDLive(pid) {
			continue
		}
		stageDir := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(stageDir)
		if err != nil {
			return fmt.Errorf("inspect self-upgrade stage %q: %w", stageDir, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
			return fmt.Errorf("self-upgrade stage %q is not a private directory", stageDir)
		}
		children, err := os.ReadDir(stageDir)
		if err != nil {
			return fmt.Errorf("read self-upgrade stage %q: %w", stageDir, err)
		}
		if len(children) != 1 || children[0].Name() != filepath.Base(locator) {
			return fmt.Errorf("self-upgrade stage %q has unexpected contents", stageDir)
		}
		stagePath := filepath.Join(stageDir, children[0].Name())
		stageInfo, err := os.Lstat(stagePath)
		if err != nil {
			return fmt.Errorf("inspect self-upgrade stage image %q: %w", stagePath, err)
		}
		if stageInfo.Mode()&os.ModeSymlink != 0 || !stageInfo.Mode().IsRegular() || stageInfo.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("self-upgrade stage image %q is not executable", stagePath)
		}
		if err := validateImagePathOwnership("self-upgrade stage image", stagePath, stageInfo); err != nil {
			return err
		}
		if err := os.Remove(stagePath); err != nil {
			return fmt.Errorf("remove self-upgrade stage image %q: %w", stagePath, err)
		}
		if err := os.Remove(stageDir); err != nil {
			return fmt.Errorf("remove self-upgrade stage directory %q: %w", stageDir, err)
		}
	}
	return nil
}

func selfUpgradeStagePID(name, prefix string) (int, bool) {
	remainder := strings.TrimPrefix(name, prefix)
	separator := strings.IndexByte(remainder, '-')
	if separator <= 0 {
		return 0, false
	}
	pid, err := strconv.ParseInt(remainder[:separator], 10, 32)
	return int(pid), err == nil && pid > 0
}

func selfUpgradeStagePIDLive(pid int) bool {
	if pid == os.Getpid() {
		return false
	}
	err := selfUpgradeStagePIDKill(pid, 0)
	switch {
	case errors.Is(err, syscall.ESRCH):
		return false
	case errors.Is(err, syscall.EPERM):
		return true
	case err != nil:
		return true
	}

	comm, err := selfUpgradeStagePIDComm(pid)
	if err != nil {
		return true
	}
	return comm == "amq" || comm == "amq-keepalive"
}

func selfUpgradeStagePIDComm(pid int) (string, error) {
	if err := verifyDarwinSystemTool(selfUpgradeStagePSPath); err != nil {
		return "", fmt.Errorf("verify ps for self-upgrade stage PID %d: %w", pid, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), selfUpgradeStagePIDProbeTimeout)
	defer cancel()
	out, err := selfUpgradeStageRunBoundedProbe(
		ctx,
		selfUpgradeStagePSPath,
		[]string{"-o", "comm=", "-p", strconv.Itoa(pid)},
		BoundedProbeOptions{Env: os.Environ()},
	)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("inspect self-upgrade stage PID %d timed out: %w", pid, ctx.Err())
		}
		return "", fmt.Errorf("inspect self-upgrade stage PID %d: %w", pid, err)
	}
	trimmed := strings.TrimSpace(string(out))
	lines := strings.Split(trimmed, "\n")
	if trimmed == "" || len(lines) != 1 {
		return "", fmt.Errorf("inspect self-upgrade stage PID %d: ps returned ambiguous command %q", pid, string(out))
	}
	comm := strings.TrimSpace(lines[0])
	if comm == "" || strings.ContainsRune(comm, '\r') {
		return "", fmt.Errorf("inspect self-upgrade stage PID %d: ps returned invalid command %q", pid, string(out))
	}
	return filepath.Base(comm), nil
}
