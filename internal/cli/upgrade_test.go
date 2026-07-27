//go:build unix

package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunUpgradeRefusesHomebrewSymlinkBeforeDownload(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "homebrew", "Cellar", "amq", "0.49.6", "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir Cellar layout: %v", err)
	}
	const original = "homebrew-owned"
	if err := os.WriteFile(target, []byte(original), 0o755); err != nil {
		t.Fatalf("write Cellar binary: %v", err)
	}
	link := filepath.Join(base, "homebrew", "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("mkdir Homebrew bin: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink Homebrew binary: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("resolve Homebrew symlink: %v", err)
	}

	oldExecutablePath := executablePathForUpgrade
	executablePathForUpgrade = func() (string, string, error) {
		return link, resolved, nil
	}
	t.Cleanup(func() { executablePathForUpgrade = oldExecutablePath })
	oldFetchLatestTag := fetchLatestTagForUpgrade
	fetchCalls := 0
	fetchLatestTagForUpgrade = func(context.Context, *http.Client) (string, error) {
		fetchCalls++
		return "", errors.New("unexpected release lookup")
	}
	t.Cleanup(func() { fetchLatestTagForUpgrade = oldFetchLatestTag })

	err = runUpgrade(nil, "v0.0.0")
	const want = "amq is installed via Homebrew; run brew upgrade amq instead"
	if err == nil || err.Error() != want {
		t.Fatalf("runUpgrade error = %v, want %q", err, want)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read Cellar binary after refusal: %v", err)
	}
	if string(got) != original {
		t.Fatalf("Cellar binary mutated: got %q, want %q", got, original)
	}
	if fetchCalls != 0 {
		t.Fatalf("release lookups = %d, want 0 before Homebrew refusal", fetchCalls)
	}
}

func TestRunUpgradeRefusesUnwritableDestinationBeforeDownload(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses Unix permission checks")
	}
	path := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(path, []byte("manual"), 0o755); err != nil {
		t.Fatalf("write manual binary: %v", err)
	}
	if err := os.Chmod(path, 0o555); err != nil {
		t.Fatalf("chmod manual binary: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o755) })

	oldExecutablePath := executablePathForUpgrade
	executablePathForUpgrade = func() (string, string, error) {
		return path, path, nil
	}
	t.Cleanup(func() { executablePathForUpgrade = oldExecutablePath })
	oldFetchLatestTag := fetchLatestTagForUpgrade
	fetchCalls := 0
	fetchLatestTagForUpgrade = func(context.Context, *http.Client) (string, error) {
		fetchCalls++
		return "", errors.New("unexpected release lookup")
	}
	t.Cleanup(func() { fetchLatestTagForUpgrade = oldFetchLatestTag })

	err := runUpgrade(nil, "v0.0.0")
	if !errors.Is(err, os.ErrPermission) ||
		!strings.Contains(err.Error(), "cannot write the amq install location") {
		t.Fatalf("runUpgrade error = %v, want clean permission refusal", err)
	}
	if fetchCalls != 0 {
		t.Fatalf("release lookups = %d, want 0 before writability refusal", fetchCalls)
	}
}

func TestSelectUpgradeDestinationPreservesManualSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "versions", "0.49.6", "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir manual layout: %v", err)
	}
	if err := os.WriteFile(target, []byte("manual"), 0o755); err != nil {
		t.Fatalf("write manual binary: %v", err)
	}
	link := filepath.Join(base, "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("mkdir manual bin: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink manual binary: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("resolve manual symlink: %v", err)
	}

	var checked string
	got, err := selectUpgradeDestination(link, resolved, func(path string) error {
		checked = path
		return nil
	})
	if err != nil {
		t.Fatalf("selectUpgradeDestination: %v", err)
	}
	if got != resolved {
		t.Fatalf("destination = %q, want resolved target %q", got, resolved)
	}
	if checked != resolved {
		t.Fatalf("writability checked at %q, want resolved target %q", checked, resolved)
	}
}

func TestSelectUpgradeDestinationRequiresExactCellarComponent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "NotCellar", "amq")
	got, err := selectUpgradeDestination(path, path, func(string) error { return nil })
	if err != nil {
		t.Fatalf("selectUpgradeDestination false positive: %v", err)
	}
	if got != path {
		t.Fatalf("destination = %q, want %q", got, path)
	}
}

func TestSelectUpgradeDestinationRefusesUnwritableResolvedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bin", "amq")
	permissionErr := &os.PathError{Op: "access", Path: path, Err: os.ErrPermission}

	_, err := selectUpgradeDestination(path, path, func(got string) error {
		if got != path {
			t.Fatalf("writability path = %q, want %q", got, path)
		}
		return permissionErr
	})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("selectUpgradeDestination error = %v, want permission error", err)
	}
	if !strings.Contains(err.Error(), "cannot write the amq install location") {
		t.Fatalf("selectUpgradeDestination error = %q, want clean writability refusal", err)
	}
}
