//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCoopWakeLaunchLocatorPreservesStableSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "amq-0.58.0")
	if err := os.WriteFile(target, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(dir, "amq")
	if err := os.Symlink(target, stable); err != nil {
		t.Fatal(err)
	}
	if got := coopWakeLaunchLocator(stable); got != stable {
		t.Fatalf("coop wake launch path=%q, want unresolved stable locator %q", got, stable)
	}
	if got := coopWakeLaunchLocator(target); got != target {
		t.Fatalf("direct coop wake launch path=%q, want %q", got, target)
	}
	if got := coopWakeLaunchLocator("./amq"); got != "" {
		t.Fatalf("relative coop wake launch locator=%q, want ineligible empty locator", got)
	}
}

func TestCoopWakeExecutionPathPinsCurrentSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "amq-0.58.0")
	next := filepath.Join(dir, "amq-0.59.0")
	for _, path := range []string{current, next} {
		if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stable := filepath.Join(dir, "amq")
	if err := os.Symlink(current, stable); err != nil {
		t.Fatal(err)
	}
	selected, err := coopWakeExecutionPath(stable)
	if err != nil {
		t.Fatal(err)
	}
	resolvedCurrent, err := filepath.EvalSymlinks(current)
	if err != nil {
		t.Fatal(err)
	}
	replaceWakeSelfUpgradeLocator(t, stable, next)
	if selected != resolvedCurrent {
		t.Fatalf("coop wake execution path=%q, want pinned current target %q", selected, resolvedCurrent)
	}
	if got := coopWakeLaunchLocator(stable); got != stable {
		t.Fatalf("coop wake locator=%q, want stable path %q", got, stable)
	}
}
