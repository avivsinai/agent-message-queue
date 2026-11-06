//go:build unix

package hookinstall

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInstallBothReportsPartialCommitWhenSecondWriteFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	dir := t.TempDir()
	claudeConfig := filepath.Join(dir, "claude", "settings.json")
	mustWrite(t, claudeConfig, []byte(`{"hooks":{"SessionStart":[]}}`))
	codexDir := filepath.Join(dir, "codex")
	codexConfig := filepath.Join(codexDir, "hooks.json")
	mustWrite(t, codexConfig, []byte(`{"hooks":{"SessionStart":[]}}`))
	if err := os.Chmod(codexDir, 0o500); err != nil {
		t.Fatalf("chmod codex dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(codexDir, 0o700) })

	result, err := Install(Options{
		Agent:        AgentBoth,
		ScriptPath:   filepath.Join(dir, "hook.sh"),
		BinaryPath:   writeExecutable(t, filepath.Join(dir, "amq-keepalive")),
		ClaudeConfig: claudeConfig,
		CodexConfig:  codexConfig,
		Timeout:      time.Second,
	})
	if err == nil {
		t.Fatal("Install() error = nil, want second-config write failure")
	}
	claude := readJSON(t, claudeConfig)
	claudeChanged := countCommand(claude, result.Commands[AgentClaude]) == 1
	if claudeChanged {
		if !strings.Contains(err.Error(), "partial hookinstall commit") {
			t.Fatalf("committed Claude without partial-commit error: %v", err)
		}
		return
	}
	if strings.Contains(err.Error(), "partial hookinstall commit") {
		t.Fatalf("partial-commit reported without Claude write: %v", err)
	}
}

func processRunning(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func killPID(pid int) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
