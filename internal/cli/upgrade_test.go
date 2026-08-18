//go:build unix

package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestRunUpgradeAlreadyCurrentReportsStaleWakesWithInvalidAmbientAgent(t *testing.T) {
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
	t.Setenv(envMe, "-legacy")
	roots := upgradeDiagnosticRoots(baseRoot)
	if !slices.Contains(roots, staleRoot) || !slices.Contains(roots, currentRoot) {
		t.Fatalf("upgrade diagnostic roots = %#v, want both session roots", roots)
	}
	inspection := inspectWakeLock(staleRoot, "codex")
	if hint, ok, err := checkStaleWakeBinaryHint(inspection); err != nil {
		t.Fatalf("stale fixture inspection error: %v", err)
	} else if !ok {
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
		upgradeLiveWakePreviousBinaryNote,
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

func TestUpgradeDiagnosticRootsIncludesValidUnderscoreSession(t *testing.T) {
	baseRoot := secureTempDirForTest(t)
	sessionRoot := filepath.Join(baseRoot, "_ops")
	if err := fsq.EnsureAgentDirs(sessionRoot, "codex"); err != nil {
		t.Fatal(err)
	}
	if err := validateSessionName(filepath.Base(sessionRoot)); err != nil {
		t.Fatalf("test session is not valid: %v", err)
	}
	if roots := upgradeDiagnosticRoots(baseRoot); !slices.Contains(roots, sessionRoot) {
		t.Fatalf("upgrade diagnostic roots = %#v, want %s", roots, sessionRoot)
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
	oldDetect := detectHomebrewPrefixForUpgrade
	detectHomebrewPrefixForUpgrade = func() string { return filepath.Join(base, "homebrew") }
	t.Cleanup(func() { detectHomebrewPrefixForUpgrade = oldDetect })
	oldFetchLatestTag := fetchLatestTagForUpgrade
	fetchCalls := 0
	fetchLatestTagForUpgrade = func(context.Context, *http.Client) (string, error) {
		fetchCalls++
		return "", errors.New("unexpected release lookup")
	}
	t.Cleanup(func() { fetchLatestTagForUpgrade = oldFetchLatestTag })

	err = runUpgrade(nil, "v0.0.0")
	const want = "amq is installed via Homebrew; run brew update && brew upgrade amq"
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

func TestSelectUpgradeDestinationRefusesHomebrewOwnedPaths(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "homebrew")
	binPath := filepath.Join(prefix, "bin", "amq")
	cellarPath := filepath.Join(prefix, "Cellar", "amq", "0.52.2", "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatalf("mkdir Homebrew bin: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cellarPath), 0o755); err != nil {
		t.Fatalf("mkdir Homebrew Cellar: %v", err)
	}
	if err := os.WriteFile(cellarPath, []byte("homebrew"), 0o755); err != nil {
		t.Fatalf("write Homebrew Cellar binary: %v", err)
	}

	oldDetect := detectHomebrewPrefixForUpgrade
	detectHomebrewPrefixForUpgrade = func() string { return prefix }
	t.Cleanup(func() { detectHomebrewPrefixForUpgrade = oldDetect })

	t.Run("severed regular prefix binary recommends reinstall", func(t *testing.T) {
		if err := os.WriteFile(binPath, []byte("self-upgraded"), 0o755); err != nil {
			t.Fatalf("write severed prefix binary: %v", err)
		}

		_, err := selectUpgradeDestination(binPath, binPath, func(string) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "brew reinstall amq") {
			t.Fatalf("selectUpgradeDestination error = %v, want reinstall remedy", err)
		}
	})

	t.Run("dangling Cellar symlink refuses before resolution", func(t *testing.T) {
		if err := os.Remove(binPath); err != nil {
			t.Fatalf("remove regular prefix binary: %v", err)
		}
		danglingTarget := filepath.Join(prefix, "Cellar", "amq", "missing", "bin", "amq")
		if err := os.Symlink(danglingTarget, binPath); err != nil {
			t.Fatalf("create dangling Cellar symlink: %v", err)
		}

		_, err := selectUpgradeDestination(binPath, binPath, func(string) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "brew update && brew upgrade amq") {
			t.Fatalf("selectUpgradeDestination error = %v, want update and upgrade remedy", err)
		}
	})

	t.Run("healthy Cellar symlink refuses", func(t *testing.T) {
		if err := os.Remove(binPath); err != nil {
			t.Fatalf("remove dangling symlink: %v", err)
		}
		if err := os.Symlink(cellarPath, binPath); err != nil {
			t.Fatalf("create healthy Cellar symlink: %v", err)
		}

		_, err := selectUpgradeDestination(binPath, cellarPath, func(string) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "brew update && brew upgrade amq") {
			t.Fatalf("selectUpgradeDestination error = %v, want update and upgrade remedy", err)
		}
	})
}

func TestSelectUpgradeDestinationRequiresDetectedHomebrewOwnership(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "homebrew")
	brewBinPath := filepath.Join(prefix, "bin", "amq")
	brewCellarPath := filepath.Join(prefix, "Cellar", "amq", "0.52.2", "bin", "amq")
	manualPath := filepath.Join(t.TempDir(), "bin", "amq")

	t.Run("brew paths proceed without Homebrew detection", func(t *testing.T) {
		oldDetect := detectHomebrewPrefixForUpgrade
		detectHomebrewPrefixForUpgrade = func() string { return "" }
		t.Cleanup(func() { detectHomebrewPrefixForUpgrade = oldDetect })

		for _, path := range []string{brewBinPath, brewCellarPath} {
			got, err := selectUpgradeDestination(path, path, func(string) error { return nil })
			if err != nil {
				t.Fatalf("selectUpgradeDestination(%q): %v", path, err)
			}
			if got != path {
				t.Fatalf("destination = %q, want %q", got, path)
			}
		}
	})

	t.Run("manual path proceeds with Homebrew present", func(t *testing.T) {
		oldDetect := detectHomebrewPrefixForUpgrade
		detectHomebrewPrefixForUpgrade = func() string { return prefix }
		t.Cleanup(func() { detectHomebrewPrefixForUpgrade = oldDetect })

		got, err := selectUpgradeDestination(manualPath, manualPath, func(string) error { return nil })
		if err != nil {
			t.Fatalf("selectUpgradeDestination: %v", err)
		}
		if got != manualPath {
			t.Fatalf("destination = %q, want %q", got, manualPath)
		}
	})

	t.Run("prefix-like sibling path proceeds with Homebrew present", func(t *testing.T) {
		oldDetect := detectHomebrewPrefixForUpgrade
		detectHomebrewPrefixForUpgrade = func() string { return prefix }
		t.Cleanup(func() { detectHomebrewPrefixForUpgrade = oldDetect })

		siblingPath := prefix + "-other/bin/amq"
		got, err := selectUpgradeDestination(siblingPath, siblingPath, func(string) error { return nil })
		if err != nil {
			t.Fatalf("selectUpgradeDestination: %v", err)
		}
		if got != siblingPath {
			t.Fatalf("destination = %q, want %q", got, siblingPath)
		}
	})
}

func TestDetectHomebrewPrefixForUpgradeRespectsEnvironment(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "custom-homebrew")
	t.Setenv("HOMEBREW_PREFIX", prefix)

	oldLookPath := lookPathForHomebrewUpgrade
	lookPathForHomebrewUpgrade = func(string) (string, error) {
		t.Fatal("brew PATH lookup must not run when HOMEBREW_PREFIX is set")
		return "", errors.New("unreachable")
	}
	t.Cleanup(func() { lookPathForHomebrewUpgrade = oldLookPath })

	if got := detectHomebrewPrefix(); got != prefix {
		t.Fatalf("detectHomebrewPrefix() = %q, want %q", got, prefix)
	}
}

func TestDetectHomebrewPrefixForUpgradeUsesBoundedBrewLookup(t *testing.T) {
	t.Setenv("HOMEBREW_PREFIX", "")
	prefix := filepath.Join(t.TempDir(), "path-homebrew")

	oldLookPath := lookPathForHomebrewUpgrade
	lookPathForHomebrewUpgrade = func(name string) (string, error) {
		if name != "brew" {
			t.Fatalf("LookPath name = %q, want brew", name)
		}
		return "/custom/bin/brew", nil
	}
	t.Cleanup(func() { lookPathForHomebrewUpgrade = oldLookPath })

	oldRun := runBrewPrefixForHomebrewUpgrade
	runBrewPrefixForHomebrewUpgrade = func(ctx context.Context, path string) ([]byte, error) {
		if path != "/custom/bin/brew" {
			t.Fatalf("brew path = %q, want /custom/bin/brew", path)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > homebrewDetectionTimeout {
			t.Fatalf("brew --prefix context is not bounded by %s", homebrewDetectionTimeout)
		}
		return []byte(prefix + "\n"), nil
	}
	t.Cleanup(func() { runBrewPrefixForHomebrewUpgrade = oldRun })

	oldExists := homebrewBrewExistsForUpgrade
	homebrewBrewExistsForUpgrade = func(string) bool {
		t.Fatal("well-known prefix lookup must not run after brew --prefix succeeds")
		return false
	}
	t.Cleanup(func() { homebrewBrewExistsForUpgrade = oldExists })

	if got := detectHomebrewPrefix(); got != prefix {
		t.Fatalf("detectHomebrewPrefix() = %q, want %q", got, prefix)
	}
}

func TestDetectHomebrewPrefixForUpgradeFallsBackToExistingWellKnownBrew(t *testing.T) {
	t.Setenv("HOMEBREW_PREFIX", "")

	oldLookPath := lookPathForHomebrewUpgrade
	lookPathForHomebrewUpgrade = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { lookPathForHomebrewUpgrade = oldLookPath })

	want := "/home/linuxbrew/.linuxbrew"
	oldExists := homebrewBrewExistsForUpgrade
	homebrewBrewExistsForUpgrade = func(path string) bool {
		return path == filepath.Join(want, "bin", "brew")
	}
	t.Cleanup(func() { homebrewBrewExistsForUpgrade = oldExists })

	if got := detectHomebrewPrefix(); got != want {
		t.Fatalf("detectHomebrewPrefix() = %q, want %q", got, want)
	}
}

func TestPathWithinDirectoryEachClause(t *testing.T) {
	parent := t.TempDir()
	inside := filepath.Join(parent, "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if !pathWithinDirectory(parent, parent) {
		t.Fatal("directory not treated as within itself")
	}
	if !pathWithinDirectory(inside, parent) {
		t.Fatal("nested file not treated as within parent")
	}
	if pathWithinDirectory(filepath.Dir(parent), parent) {
		t.Fatal("parent of parent treated as within")
	}
	if pathWithinDirectory(filepath.Join(filepath.Dir(parent), "sibling"), parent) {
		t.Fatal("sibling treated as within")
	}
}

func TestHomebrewOwnsUpgradePathBinOrCellar(t *testing.T) {
	prefix := t.TempDir()
	binPath := filepath.Join(prefix, "bin", "amq")
	cellarPath := filepath.Join(prefix, "Cellar", "amq", "0.65.0", "bin", "amq")
	other := filepath.Join(t.TempDir(), "amq")
	if !homebrewOwnsUpgradePath(prefix, binPath) {
		t.Fatal("prefix/bin path not owned")
	}
	if !homebrewOwnsUpgradePath(prefix, cellarPath) {
		t.Fatal("prefix/Cellar path not owned")
	}
	if homebrewOwnsUpgradePath(prefix, other) {
		t.Fatal("unrelated path treated as Homebrew-owned")
	}
	if homebrewOwnsUpgradePath(prefix, "") {
		t.Fatal("empty path treated as Homebrew-owned")
	}
}
