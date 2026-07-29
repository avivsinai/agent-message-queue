//go:build unix

package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunUpgradeAlreadyCurrentReportsStaleWakesAcrossSessions(t *testing.T) {
	baseRoot := secureTempDirForTest(t)
	staleRoot := filepath.Join(baseRoot, "session1")
	currentRoot := filepath.Join(baseRoot, "session2")
	for _, root := range []string{staleRoot, currentRoot} {
		if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
			t.Fatalf("ensure agent dirs for %s: %v", root, err)
		}
	}
	writeWakeLockForTest(t, staleRoot, "codex", wakeLock{
		PID:          4242,
		Root:         canonicalWakeRoot(staleRoot),
		Agent:        "codex",
		Started:      "2026-07-27T10:00:00Z",
		ProcessStart: "stale-start",
		BootID:       "11111111-1111-1111-1111-111111111111",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"amq", "wake"},
		Generation:   "stale-generation",
	})
	writeWakeLockForTest(t, currentRoot, "codex", wakeLock{
		PID:          4343,
		Root:         canonicalWakeRoot(currentRoot),
		Agent:        "codex",
		Started:      "2026-07-29T10:00:00Z",
		ProcessStart: "current-start",
		BootID:       "11111111-1111-1111-1111-111111111111",
		Executable:   "/opt/homebrew/bin/amq",
		Args:         []string{"amq", "wake"},
		Generation:   "current-generation",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		start := "stale-start"
		if pid == 4343 {
			start = "current-start"
		}
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: start,
			BootID:     "11111111-1111-1111-1111-111111111111",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"amq", "wake"},
		}
	})
	stubWakeBinaryStaleness(t, func(inspection wakeLockInspection) (wakeBinaryStaleness, error) {
		return wakeBinaryStaleness{
			Stale:    inspection.Root == canonicalWakeRoot(staleRoot),
			Method:   wakeBinaryComparisonExactIdentity,
			Evidence: stableWakeBinaryEvidenceForTest(),
		}, nil
	})

	binary := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(binary, []byte("current"), 0o755); err != nil {
		t.Fatalf("write current binary: %v", err)
	}
	oldExecutablePath := executablePathForUpgrade
	executablePathForUpgrade = func() (string, string, error) {
		return binary, binary, nil
	}
	t.Cleanup(func() { executablePathForUpgrade = oldExecutablePath })
	oldFetchLatestTag := fetchLatestTagForUpgrade
	fetchLatestTagForUpgrade = func(context.Context, *http.Client) (string, error) {
		return "v0.49.9", nil
	}
	t.Cleanup(func() { fetchLatestTagForUpgrade = oldFetchLatestTag })
	t.Setenv(envRoot, baseRoot)
	t.Setenv(envBaseRoot, baseRoot)
	t.Setenv(envSession, "")
	roots := upgradeDiagnosticRoots(baseRoot)
	if !slices.Contains(roots, staleRoot) || !slices.Contains(roots, currentRoot) {
		t.Fatalf("upgrade diagnostic roots = %#v, want both session roots", roots)
	}
	inspection := inspectWakeLock(staleRoot, "codex")
	if hint, ok := checkStaleWakeBinaryHint(inspection); !ok {
		t.Fatalf("stale fixture produced no hint: %#v", inspection)
	} else if hint.WakeBinary == nil || hint.WakeBinary.PID != 4242 {
		t.Fatalf("stale fixture hint = %#v", hint)
	}

	stdout, _, err := captureEnvOutput(t, func() error {
		return runUpgrade(nil, "v0.49.9")
	})
	if err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	for _, want := range []string{
		"amq is already up to date (v0.49.9)",
		"Stale running wakes:",
		staleRoot,
		`agent "codex"`,
		"pid 4242",
		"restart",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("upgrade output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, currentRoot) || strings.Contains(stdout, "pid 4343") {
		t.Fatalf("upgrade reported current wake as stale:\n%s", stdout)
	}
}

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
