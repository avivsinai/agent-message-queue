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
	coopExecProcess = func(path string, _ []string, _ []string) error {
		return fmt.Errorf("test attempted unguarded process replacement with %q", path)
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

// A partial tree (the shape this very bug used to mint) converges to a valid
// queue instead of persisting half-initialized.
func TestCoopExecPartialRootConvergesToValidTree(t *testing.T) {
	dir := secureTempDirForTest(t)
	if err := os.Mkdir(filepath.Join(dir, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := stubCoopExecSentinel(t)

	err := runCoopExec([]string{"--root", dir, "--me", "codex", "--no-wake", "sh"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want exec sentinel", err)
	}
	for _, marker := range []string{"threads", "meta"} {
		info, statErr := os.Lstat(filepath.Join(dir, marker))
		if statErr != nil || !info.IsDir() {
			t.Fatalf("marker %s not completed: %v", marker, statErr)
		}
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

// A hostile threads/ or meta/ symlink beside a real agents/ must never cause
// a write outside the tree; same-root partial completion before the failure
// is reported honestly via the returned error.
func TestCoopExecHostileSiblingSymlinkNeverWritesOutside(t *testing.T) {
	for _, marker := range []string{"threads", "meta"} {
		t.Run(marker, func(t *testing.T) {
			dir := secureTempDirForTest(t)
			outside := secureTempDirForTest(t)
			if err := os.Mkdir(filepath.Join(dir, "agents"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, marker)); err != nil {
				t.Fatal(err)
			}
			_ = stubCoopExecSentinel(t)

			err := runCoopExec([]string{"--root", dir, "--me", "codex", "--no-wake", "sh"})
			if err == nil {
				t.Fatalf("hostile %s symlink accepted", marker)
			}
			if got := dirEntryNames(t, outside); len(got) != 0 {
				t.Fatalf("outside tree gained entries through %s: %v", marker, got)
			}
		})
	}
}

// A typo'd command must fail before any provisioning: no root, no tree.
func TestCoopExecMissingBinaryPerformsZeroWrites(t *testing.T) {
	parent := secureTempDirForTest(t)
	root := filepath.Join(parent, "wouldberoot")

	err := runCoopExec([]string{"--root", root, "--me", "codex", "--no-wake", "definitely-missing-agent-binary-xyz"})
	if err == nil || !strings.Contains(err.Error(), "command not found") {
		t.Fatalf("error = %v, want command-not-found", err)
	}
	if _, statErr := os.Lstat(root); !os.IsNotExist(statErr) {
		t.Fatalf("root created despite missing binary: %v", statErr)
	}
}

// Usage validation keeps precedence over binary resolution: an invalid
// session name with a missing binary reports the validation error, never
// command-not-found.
func TestCoopExecInvalidSessionPrecedesMissingBinary(t *testing.T) {
	err := runCoopExec([]string{"--session", "../bad", "--no-wake", "definitely-missing-agent-binary-xyz"})
	if err == nil || strings.Contains(err.Error(), "command not found") {
		t.Fatalf("error = %v, want session validation error before binary resolution", err)
	}
	if code := GetExitCode(err); code != ExitUsage {
		t.Fatalf("exit code = %d, want %d (usage contract)", code, ExitUsage)
	}
}

// Selector-free exec with a missing binary must not auto-init anything: no
// .amqrc, no queue tree. This is the new-contract twin of the old fixture
// that asserted auto-init ran before binary resolution.
func TestCoopExecSelectorFreeMissingBinaryPerformsZeroWrites(t *testing.T) {
	clearCoopSessionPinForTest(t)
	setOptionalEnv(t, envRoot, "", false)
	setOptionalEnv(t, envGlobalRoot, "", false)
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		resetAmqrcCache()
	})
	resetAmqrcCache()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	err = runCoopExec([]string{"--no-gitignore", "--no-wake", "-y", "definitely-missing-agent-binary-xyz"})
	if err == nil || !strings.Contains(err.Error(), "command not found") {
		t.Fatalf("error = %v, want command-not-found", err)
	}
	for _, artifact := range []string{".amqrc", defaultCoopRoot} {
		if _, statErr := os.Lstat(filepath.Join(projectDir, artifact)); !os.IsNotExist(statErr) {
			t.Fatalf("%s created despite missing binary: %v", artifact, statErr)
		}
	}
}

// The alias-swap boundary: replacing the lexical root between classification
// and provisioning must fail closed with zero writes to the impostor.
func TestCoopExecAliasSwapBetweenClassifyAndProvisionFailsClosed(t *testing.T) {
	parent := secureTempDirForTest(t)
	root := filepath.Join(parent, "queue")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved-away")
	impostor := filepath.Join(parent, "impostor")
	if err := os.Mkdir(impostor, 0o700); err != nil {
		t.Fatal(err)
	}

	oldHook := coopProvisionAfterClassifyHook
	coopProvisionAfterClassifyHook = func() {
		if err := os.Rename(root, moved); err != nil {
			t.Fatalf("move root: %v", err)
		}
		if err := os.Rename(impostor, root); err != nil {
			t.Fatalf("install impostor: %v", err)
		}
	}
	t.Cleanup(func() { coopProvisionAfterClassifyHook = oldHook })
	_ = stubCoopExecSentinel(t)

	err := runCoopExec([]string{"--root", root, "--me", "codex", "--no-wake", "sh"})
	if err == nil {
		t.Fatal("alias swap accepted")
	}
	if got := dirEntryNames(t, root); len(got) != 0 {
		t.Fatalf("impostor gained entries: %v", got)
	}
	if got := dirEntryNames(t, filepath.Join(moved, "agents")); len(got) != 0 {
		t.Fatalf("moved tree mutated after swap detection: %v", got)
	}
}
