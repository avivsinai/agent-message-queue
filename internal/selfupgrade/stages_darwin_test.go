//go:build darwin

package selfupgrade

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestCleanupStagesLeavesLiveProcessStages(t *testing.T) {
	dir := t.TempDir()
	locator := filepath.Join(dir, "amq-keepalive")
	liveProcess := exec.Command("sleep", "30")
	if err := liveProcess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = liveProcess.Process.Kill()
		_ = liveProcess.Wait()
	})
	liveDir := filepath.Join(dir, selfUpgradeStagePrefix(locator)+strconv.Itoa(liveProcess.Process.Pid)+"-live")
	deadPID := deadSelfUpgradeStagePID(t)
	deadDir := filepath.Join(dir, selfUpgradeStagePrefix(locator)+strconv.Itoa(deadPID)+"-dead")
	for _, stageDir := range []string{liveDir, deadDir} {
		if err := os.Mkdir(stageDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stageDir, filepath.Base(locator)), []byte("stage"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := CleanupStages(locator); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(liveDir); err != nil {
		t.Fatalf("live stage = %v, want preserved", err)
	}
	if _, err := os.Stat(deadDir); !os.IsNotExist(err) {
		t.Fatalf("dead stage = %v, want removed", err)
	}
}

func deadSelfUpgradeStagePID(t *testing.T) int {
	t.Helper()
	for pid := os.Getpid() + 1; pid < os.Getpid()+10000; pid++ {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return pid
		}
	}
	t.Fatal("could not find a dead process id")
	return 0
}
