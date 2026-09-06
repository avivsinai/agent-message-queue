//go:build darwin || linux

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func guardTestProcessReplacement() {
	coopExecProcess = guardedTestProcessReplacement("coopExecProcess")
	launchExecProcess = guardedTestProcessReplacement("launchExecProcess")
	wakeRestartExec = guardedTestProcessReplacement("wakeRestartExec")
}

func guardedTestProcessReplacement(seam string) func(string, []string, []string) error {
	return func(path string, _ []string, _ []string) error {
		return fmt.Errorf("test attempted unguarded %s process replacement with %q", seam, path)
	}
}

func stubCoopExecSentinel(t *testing.T) error {
	t.Helper()
	sentinel := errors.New("exec sentinel")
	old := coopExecProcess
	coopExecProcess = func(string, []string, []string) error { return sentinel }
	t.Cleanup(func() { coopExecProcess = old })
	return sentinel
}

func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// The notaqueue repro from #480: a pre-existing directory that is not a queue
// must never gain a mailbox, no matter that it exists.
func TestCoopExecForeignRootRefusedWithZeroWrites(t *testing.T) {
	dir := secureTempDirForTest(t)
	if err := os.WriteFile(filepath.Join(dir, "somefile.txt"), []byte("not a queue"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = stubCoopExecSentinel(t)

	err := runCoopExec([]string{"--root", dir, "--me", "codex", "--no-wake", "sh"})
	if err == nil || !strings.Contains(err.Error(), "not an initialized AMQ queue root") {
		t.Fatalf("error = %v, want foreign-root refusal", err)
	}
	if code := GetExitCode(err); code != ExitNotFound {
		t.Fatalf("exit code = %d, want %d (not-found contract)", code, ExitNotFound)
	}
	if got := dirEntryNames(t, dir); len(got) != 1 || got[0] != "somefile.txt" {
		t.Fatalf("foreign dir mutated: %v", got)
	}
}

// An empty pre-made directory provisions the same full tree a missing root
// gets — a valid queue, never the partial agents-only shape the bug minted.
func TestCoopExecEmptyPremadeRootProvisionsFullTree(t *testing.T) {
	dir := secureTempDirForTest(t)
	sentinel := stubCoopExecSentinel(t)

	err := runCoopExec([]string{"--root", dir, "--me", "codex", "--no-wake", "sh"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want exec sentinel", err)
	}
	for _, marker := range []string{"agents", "threads", "meta"} {
		info, statErr := os.Lstat(filepath.Join(dir, marker))
		if statErr != nil || !info.IsDir() {
			t.Fatalf("marker %s missing or not a dir: %v", marker, statErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dir, "agents", "codex", "inbox", "new")); statErr != nil {
		t.Fatalf("mailbox missing: %v", statErr)
	}
}

// agents/ existing as a symlink is a hostile shape: refuse, and never write
// through the link.
func TestCoopExecAgentsSymlinkFailsClosed(t *testing.T) {
	dir := secureTempDirForTest(t)
	target := secureTempDirForTest(t)
	if err := os.Symlink(target, filepath.Join(dir, "agents")); err != nil {
		t.Fatal(err)
	}
	_ = stubCoopExecSentinel(t)

	err := runCoopExec([]string{"--root", dir, "--me", "codex", "--no-wake", "sh"})
	if err == nil || !strings.Contains(err.Error(), "refusing to provision") {
		t.Fatalf("error = %v, want hostile-shape refusal", err)
	}
	if got := dirEntryNames(t, target); len(got) != 0 {
		t.Fatalf("symlink target gained entries: %v", got)
	}
}
