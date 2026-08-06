//go:build darwin || linux

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupWakeQuarantineRequiresExplicitSelectorAndIsDryRunSafe(t *testing.T) {
	root := secureTempDirForTest(t)
	agentDir := filepath.Join(root, "agents", "codex")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	oldPath := filepath.Join(
		agentDir,
		".wake.lock.quarantined."+now.Add(-2*time.Hour).Format(wakeQuarantineTimestampLayout),
	)
	recentPath := filepath.Join(
		agentDir,
		".wake.target.quarantined."+now.Add(-10*time.Minute).Format(wakeQuarantineTimestampLayout),
	)
	for _, path := range []string{oldPath, recentPath} {
		if err := os.WriteFile(path, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := runCleanup([]string{"--root", root}); err == nil ||
		!strings.Contains(err.Error(), "--wake-quarantine-older-than") {
		t.Fatalf("cleanup without selector error = %v", err)
	}

	dryRunOutput, err := captureEnvStdout(t, func() error {
		return runCleanup([]string{
			"--root", root,
			"--wake-quarantine-older-than", "1h",
			"--dry-run",
			"--json",
		})
	})
	if err != nil {
		t.Fatalf("dry-run wake quarantine cleanup: %v", err)
	}
	if !strings.Contains(dryRunOutput, oldPath) || strings.Contains(dryRunOutput, recentPath) {
		t.Fatalf("dry-run output = %s", dryRunOutput)
	}
	for _, path := range []string{oldPath, recentPath} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("dry-run removed %s: %v", path, err)
		}
	}

	if _, err := captureEnvStdout(t, func() error {
		return runCleanup([]string{"--root", root, "--tmp-older-than", "1ns", "--yes"})
	}); err != nil {
		t.Fatalf("ordinary tmp cleanup: %v", err)
	}
	for _, path := range []string{oldPath, recentPath} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("ordinary tmp cleanup removed quarantine %s: %v", path, err)
		}
	}

	actualOutput, err := captureEnvStdout(t, func() error {
		return runCleanup([]string{
			"--root", root,
			"--wake-quarantine-older-than", "1h",
			"--yes",
			"--json",
		})
	})
	if err != nil {
		t.Fatalf("wake quarantine cleanup: %v", err)
	}
	if !strings.Contains(actualOutput, `"removed": 1`) {
		t.Fatalf("cleanup output = %s", actualOutput)
	}
	if _, err := os.Lstat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old quarantine was not removed: %v", err)
	}
	if _, err := os.Lstat(recentPath); err != nil {
		t.Fatalf("recent quarantine was removed: %v", err)
	}
}

func TestCleanupWakeQuarantinePreservesReplacementAfterCandidateScan(t *testing.T) {
	root := secureTempDirForTest(t)
	agentDir := filepath.Join(root, "agents", "codex")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := ".wake.lock.quarantined." + time.Now().UTC().Add(-2*time.Hour).Format(wakeQuarantineTimestampLayout)
	path := filepath.Join(agentDir, name)
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement := []byte("replacement")
	originalHook := beforeWakeQuarantineCleanupRevalidation
	beforeWakeQuarantineCleanupRevalidation = func(candidate wakeQuarantineCleanupCandidate) {
		beforeWakeQuarantineCleanupRevalidation = func(wakeQuarantineCleanupCandidate) {}
		if err := os.Rename(candidate.Path, candidate.Path+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(candidate.Path, replacement, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeWakeQuarantineCleanupRevalidation = originalHook })

	_, err := captureEnvStdout(t, func() error {
		return runCleanup([]string{
			"--root", root,
			"--wake-quarantine-older-than", "1h",
			"--yes",
		})
	})
	if err == nil || !strings.Contains(err.Error(), "changed before cleanup") {
		t.Fatalf("replacement cleanup error = %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("replacement changed: got %q want %q", got, replacement)
	}
}
