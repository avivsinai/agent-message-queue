//go:build unix

package hookinstall

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBackupIfExistsDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "settings.json")
	original := []byte(`{"keep":true}`)
	mustWrite(t, src, original)
	victim := filepath.Join(dir, "victim")
	victimBytes := []byte("do-not-clobber\n")
	mustWrite(t, victim, victimBytes)

	for attempt := 0; attempt < 8; attempt++ {
		ts := time.Now().UTC().Format("20060102T150405Z")
		planted := fmt.Sprintf("%s.bak-%s", src, ts)
		_ = os.Remove(planted)
		if err := os.Symlink(victim, planted); err != nil {
			t.Fatalf("Symlink backup name: %v", err)
		}
		if time.Now().UTC().Format("20060102T150405Z") != ts {
			continue
		}
		backup, err := backupIfExists(src)
		gotVictim, readErr := os.ReadFile(victim)
		if readErr != nil || !bytes.Equal(gotVictim, victimBytes) {
			t.Fatalf("backup followed symlink: err=%v bytes=%q backup=%q installErr=%v", readErr, gotVictim, backup, err)
		}
		info, statErr := os.Lstat(planted)
		if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("planted backup name is no longer a symlink: err=%v mode=%v", statErr, info.Mode())
		}
		if err == nil {
			if backup == planted {
				t.Fatal("backup reused symlink path")
			}
			got, readErr := os.ReadFile(backup)
			if readErr != nil || !bytes.Equal(got, original) {
				t.Fatalf("exclusive backup: err=%v bytes=%q", readErr, got)
			}
		}
		return
	}
	t.Fatal("could not plant a same-second backup symlink")
}

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
