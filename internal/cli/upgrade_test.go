//go:build unix

package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/update"
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

func TestRunUpgradeDelegatesHomebrewInstall(t *testing.T) {
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
	// The classifier is authoritative: a Homebrew install delegates to brew,
	// so the /private symlink-resolution mismatch in the temp fixture does
	// not change the outcome.
	oldClassify := classifyInstallForUpgrade
	classifyInstallForUpgrade = func(string, string) update.InstallKind {
		return update.InstallHomebrew
	}
	t.Cleanup(func() { classifyInstallForUpgrade = oldClassify })
	oldFetchLatestTag := fetchLatestTagForUpgrade
	fetchCalls := 0
	fetchLatestTagForUpgrade = func(context.Context, *http.Client) (string, error) {
		fetchCalls++
		return "", errors.New("unexpected release lookup")
	}
	t.Cleanup(func() { fetchLatestTagForUpgrade = oldFetchLatestTag })

	err = runUpgrade(nil, "v0.0.0")
	const want = "amq is installed via Homebrew; run brew update && brew upgrade amq (or amq upgrade -y)"
	if err == nil || err.Error() != want {
		t.Fatalf("runUpgrade error = %v, want %q", err, want)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read Cellar binary after delegate: %v", err)
	}
	if string(got) != original {
		t.Fatalf("Cellar binary mutated: got %q, want %q", got, original)
	}
	if fetchCalls != 0 {
		t.Fatalf("release lookups = %d, want 0 before Homebrew delegate", fetchCalls)
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

	t.Run("dangling Cellar symlink is not refused here (classifier owns it)", func(t *testing.T) {
		if err := os.Remove(binPath); err != nil {
			t.Fatalf("remove regular prefix binary: %v", err)
		}
		danglingTarget := filepath.Join(prefix, "Cellar", "amq", "missing", "bin", "amq")
		if err := os.Symlink(danglingTarget, binPath); err != nil {
			t.Fatalf("create dangling Cellar symlink: %v", err)
		}

		// selectUpgradeDestination only guards the severed-binary corruption;
		// a Cellar symlink (healthy or dangling) is delegated by the
		// classifier upstream, so this function returns the dest cleanly.
		got, err := selectUpgradeDestination(binPath, binPath, func(string) error { return nil })
		if err != nil {
			t.Fatalf("selectUpgradeDestination error = %v, want no refusal (classifier owns Homebrew delegation)", err)
		}
		if got != binPath {
			t.Fatalf("destination = %q, want %q", got, binPath)
		}
	})

	t.Run("healthy Cellar symlink is not refused here (classifier owns it)", func(t *testing.T) {
		if err := os.Remove(binPath); err != nil {
			t.Fatalf("remove dangling symlink: %v", err)
		}
		if err := os.Symlink(cellarPath, binPath); err != nil {
			t.Fatalf("create healthy Cellar symlink: %v", err)
		}

		got, err := selectUpgradeDestination(binPath, cellarPath, func(string) error { return nil })
		if err != nil {
			t.Fatalf("selectUpgradeDestination error = %v, want no refusal (classifier owns Homebrew delegation)", err)
		}
		if got != cellarPath {
			t.Fatalf("destination = %q, want %q", got, cellarPath)
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

// stubUpgradeNetwork replaces the network-touching indirection vars with
// in-memory fakes that succeed for any asset and return a fixed checksum. It
// returns a cleanup func and a counter of how many binaries were replaced.
func stubUpgradeNetwork(t *testing.T) (replaced *[]string) {
	t.Helper()
	replacedList := []string{}
	replaced = &replacedList
	oldDownload := downloadReleaseAssetForUpgrade
	downloadReleaseAssetForUpgrade = func(_ context.Context, _ *http.Client, _, _, destPath string) error {
		return os.WriteFile(destPath, []byte("archive"), 0o600)
	}
	t.Cleanup(func() { downloadReleaseAssetForUpgrade = oldDownload })
	oldChecksums := fetchChecksumsForUpgrade
	fetchChecksumsForUpgrade = func(context.Context, *http.Client, string) (map[string]string, error) {
		// Return a checksum for every asset name the upgrade loop can request
		// (amq + each companion) so the lookup always hits regardless of OS.
		m := map[string]string{}
		for _, name := range append([]string{update.BinaryName}, update.CompanionBinaries...) {
			if asset, err := update.AssetNameFor(name, "v0.0.0-test", runtime.GOOS, runtime.GOARCH); err == nil {
				m[asset] = "checksum"
			}
		}
		return m, nil
	}
	t.Cleanup(func() { fetchChecksumsForUpgrade = oldChecksums })
	oldVerify := updateVerifySHA256ForUpgrade
	updateVerifySHA256ForUpgrade = func(string, string) error { return nil }
	t.Cleanup(func() { updateVerifySHA256ForUpgrade = oldVerify })
	oldExtract := extractBinaryForUpgrade
	extractBinaryForUpgrade = func(name, _, destDir string, _ bool) (string, error) {
		p := filepath.Join(destDir, name)
		return p, os.WriteFile(p, []byte("new-"+name), 0o755)
	}
	t.Cleanup(func() { extractBinaryForUpgrade = oldExtract })
	oldReplace := replaceBinaryForUpgrade
	replaceBinaryForUpgrade = func(_, destPath string) (bool, error) {
		replacedList = append(replacedList, destPath)
		return false, nil
	}
	t.Cleanup(func() { replaceBinaryForUpgrade = oldReplace })
	oldSaveCache := saveUpgradeCacheForUpgrade
	saveUpgradeCacheForUpgrade = func(string) {}
	t.Cleanup(func() { saveUpgradeCacheForUpgrade = oldSaveCache })
	return replaced
}

func TestRunUpgradeDirectInstallReplacesBinary(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(bin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExecutablePath := executablePathForUpgrade
	executablePathForUpgrade = func() (string, string, error) { return bin, bin, nil }
	t.Cleanup(func() { executablePathForUpgrade = oldExecutablePath })
	oldDetect := detectHomebrewPrefixForUpgrade
	detectHomebrewPrefixForUpgrade = func() string { return "" }
	t.Cleanup(func() { detectHomebrewPrefixForUpgrade = oldDetect })
	oldFetch := fetchLatestTagForUpgrade
	fetchLatestTagForUpgrade = func(context.Context, *http.Client) (string, error) { return "v0.0.0-test", nil }
	t.Cleanup(func() { fetchLatestTagForUpgrade = oldFetch })
	replaced := stubUpgradeNetwork(t)

	stdout, _, err := captureEnvOutput(t, func() error {
		return runUpgrade(nil, "dev")
	})
	if err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if len(*replaced) != 1 || (*replaced)[0] != bin {
		t.Fatalf("replaced = %#v, want [%s]", *replaced, bin)
	}
	if !strings.Contains(stdout, "Upgrading to v0.0.0-test") || !strings.Contains(stdout, "amq upgrade complete.") {
		t.Fatalf("upgrade output missing expected lines:\n%s", stdout)
	}
}

func TestRunUpgradeHomebrewDelegatesWithYes(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(bin, []byte("homebrew"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExecutablePath := executablePathForUpgrade
	executablePathForUpgrade = func() (string, string, error) { return bin, bin, nil }
	t.Cleanup(func() { executablePathForUpgrade = oldExecutablePath })
	oldClassify := classifyInstallForUpgrade
	classifyInstallForUpgrade = func(string, string) update.InstallKind { return update.InstallHomebrew }
	t.Cleanup(func() { classifyInstallForUpgrade = oldClassify })
	var delegateRan []string
	oldDelegate := runDelegateForUpgrade
	runDelegateForUpgrade = func(argv []string) error { delegateRan = append(delegateRan, strings.Join(argv, " ")); return nil }
	t.Cleanup(func() { runDelegateForUpgrade = oldDelegate })
	oldFetch := fetchLatestTagForUpgrade
	fetchCalls := 0
	fetchLatestTagForUpgrade = func(context.Context, *http.Client) (string, error) {
		fetchCalls++
		return "", errors.New("unexpected release lookup")
	}
	t.Cleanup(func() { fetchLatestTagForUpgrade = oldFetch })

	stdout, _, err := captureEnvOutput(t, func() error {
		return runUpgrade([]string{"-y"}, "v0.0.0")
	})
	if err != nil {
		t.Fatalf("runUpgrade -y: %v", err)
	}
	if len(delegateRan) != 2 || delegateRan[0] != "brew update" || delegateRan[1] != "brew upgrade amq" {
		t.Fatalf("delegate ran %#v, want [brew update, brew upgrade amq]", delegateRan)
	}
	if fetchCalls != 0 {
		t.Fatalf("release lookups = %d, want 0 for a delegated upgrade", fetchCalls)
	}
	if !strings.Contains(stdout, "Running:") {
		t.Fatalf("output missing 'Running:' line:\n%s", stdout)
	}
	// The Homebrew binary is never overwritten by amq.
	got, _ := os.ReadFile(bin)
	if string(got) != "homebrew" {
		t.Fatalf("homebrew binary mutated: %q", got)
	}
}

// TestRunUpgradeClassifiesUserSymlinkToHomebrew covers Y2: a user-made
// ~/bin/amq -> /opt/homebrew/bin/amq link must classify as Homebrew from the
// pre-resolution path (the link itself sits in <prefix>/bin), so the upgrade
// delegates to brew rather than self-replacing through the link.
func TestRunUpgradeClassifiesUserSymlinkToHomebrew(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "homebrew")
	cellarTarget := filepath.Join(prefix, "Cellar", "amq", "0.73.0", "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(cellarTarget), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cellarTarget, []byte("homebrew"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The pre-resolution path is a user bin link into the Cellar; the resolved
	// path lands in the Cellar too. Either way it must delegate.
	userBin := filepath.Join(prefix, "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(userBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cellarTarget, userBin); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(userBin)
	if err != nil {
		t.Fatal(err)
	}
	oldExecutablePath := executablePathForUpgrade
	executablePathForUpgrade = func() (string, string, error) { return userBin, resolved, nil }
	t.Cleanup(func() { executablePathForUpgrade = oldExecutablePath })
	oldDetect := detectHomebrewPrefixForUpgrade
	detectHomebrewPrefixForUpgrade = func() string { return prefix }
	t.Cleanup(func() { detectHomebrewPrefixForUpgrade = oldDetect })
	var delegateRan []string
	oldDelegate := runDelegateForUpgrade
	runDelegateForUpgrade = func(argv []string) error { delegateRan = append(delegateRan, strings.Join(argv, " ")); return nil }
	t.Cleanup(func() { runDelegateForUpgrade = oldDelegate })
	oldFetch := fetchLatestTagForUpgrade
	fetchLatestTagForUpgrade = func(context.Context, *http.Client) (string, error) {
		return "", errors.New("unexpected release lookup")
	}
	t.Cleanup(func() { fetchLatestTagForUpgrade = oldFetch })

	if _, _, err := captureEnvOutput(t, func() error { return runUpgrade([]string{"-y"}, "dev") }); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if len(delegateRan) != 2 || delegateRan[0] != "brew update" || delegateRan[1] != "brew upgrade amq" {
		t.Fatalf("delegate ran %#v, want [brew update, brew upgrade amq]", delegateRan)
	}
}

func TestRunUpgradeAllReplacesPresentCompanionsSkipsMissing(t *testing.T) {
	binDir := t.TempDir()
	amqBin := filepath.Join(binDir, "amq")
	if err := os.WriteFile(amqBin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Two companions present next to amq; amq-acp absent.
	keepalive := filepath.Join(binDir, "amq-keepalive")
	bridge := filepath.Join(binDir, "amq-bridge")
	for _, p := range []string{keepalive, bridge} {
		if err := os.WriteFile(p, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldExecutablePath := executablePathForUpgrade
	executablePathForUpgrade = func() (string, string, error) { return amqBin, amqBin, nil }
	t.Cleanup(func() { executablePathForUpgrade = oldExecutablePath })
	oldDetect := detectHomebrewPrefixForUpgrade
	detectHomebrewPrefixForUpgrade = func() string { return "" }
	t.Cleanup(func() { detectHomebrewPrefixForUpgrade = oldDetect })
	// Isolate companion search from the operator's real ~/.local/bin.
	fakeHome := t.TempDir()
	oldHome := homeDirForUpgrade
	homeDirForUpgrade = func() (string, error) { return fakeHome, nil }
	t.Cleanup(func() { homeDirForUpgrade = oldHome })
	oldFetch := fetchLatestTagForUpgrade
	fetchLatestTagForUpgrade = func(context.Context, *http.Client) (string, error) { return "v0.0.0-test", nil }
	t.Cleanup(func() { fetchLatestTagForUpgrade = oldFetch })
	replaced := stubUpgradeNetwork(t)

	stdout, _, err := captureEnvOutput(t, func() error {
		return runUpgrade([]string{"--all"}, "dev")
	})
	if err != nil {
		t.Fatalf("runUpgrade --all: %v", err)
	}
	// amq + keepalive + bridge replaced; amq-acp skipped.
	want := map[string]bool{amqBin: false, keepalive: false, bridge: false}
	for _, p := range *replaced {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for p, seen := range want {
		if !seen {
			t.Fatalf("companion %s not replaced; replaced=%#v", p, *replaced)
		}
	}
	if !strings.Contains(stdout, "amq-acp not found; skipping.") {
		t.Fatalf("output missing amq-acp skip line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "amq-keepalive: a running supervisor picks up the new image") {
		t.Fatalf("output missing keepalive self-upgrade remedy:\n%s", stdout)
	}
}

// TestRunUpgradeAllReplacesSymlinkedCompanionAtTarget covers Y4: a symlinked
// companion (~/.local/bin/amq-keepalive -> /stable/amq-keepalive) is upgraded
// at its resolved TARGET, not by replacing the link, so the operator's link
// structure survives and the backing file is the new image.
func TestRunUpgradeAllReplacesSymlinkedCompanionAtTarget(t *testing.T) {
	binDir := t.TempDir()
	amqBin := filepath.Join(binDir, "amq")
	if err := os.WriteFile(amqBin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A stable target the link points at.
	stableDir := t.TempDir()
	keepaliveTarget := filepath.Join(stableDir, "amq-keepalive")
	if err := os.WriteFile(keepaliveTarget, []byte("old-keepalive"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The companion found next to amq is a symlink to the stable target.
	keepaliveLink := filepath.Join(binDir, "amq-keepalive")
	if err := os.Symlink(keepaliveTarget, keepaliveLink); err != nil {
		t.Fatal(err)
	}
	oldExecutablePath := executablePathForUpgrade
	executablePathForUpgrade = func() (string, string, error) { return amqBin, amqBin, nil }
	t.Cleanup(func() { executablePathForUpgrade = oldExecutablePath })
	oldDetect := detectHomebrewPrefixForUpgrade
	detectHomebrewPrefixForUpgrade = func() string { return "" }
	t.Cleanup(func() { detectHomebrewPrefixForUpgrade = oldDetect })
	fakeHome := t.TempDir()
	oldHome := homeDirForUpgrade
	homeDirForUpgrade = func() (string, error) { return fakeHome, nil }
	t.Cleanup(func() { homeDirForUpgrade = oldHome })
	oldFetch := fetchLatestTagForUpgrade
	fetchLatestTagForUpgrade = func(context.Context, *http.Client) (string, error) { return "v0.0.0-test", nil }
	t.Cleanup(func() { fetchLatestTagForUpgrade = oldFetch })
	replaced := stubUpgradeNetwork(t)

	if _, _, err := captureEnvOutput(t, func() error { return runUpgrade([]string{"--all"}, "dev") }); err != nil {
		t.Fatalf("runUpgrade --all: %v", err)
	}
	// The resolved TARGET was replaced, not the link path. Canonicalize the
	// target the way EvalSymlinks does so /var and /private/var agree on macOS.
	canonicalTarget, err := filepath.EvalSymlinks(keepaliveTarget)
	if err != nil {
		t.Fatal(err)
	}
	foundTarget := false
	for _, p := range *replaced {
		if p == canonicalTarget {
			foundTarget = true
		}
		if p == keepaliveLink {
			t.Fatalf("companion link path %q was replaced instead of its target", p)
		}
	}
	if !foundTarget {
		t.Fatalf("symlinked companion target %q not replaced; replaced=%#v", canonicalTarget, *replaced)
	}
	// The link still resolves to the target (structure preserved).
	if resolved, err := filepath.EvalSymlinks(keepaliveLink); err != nil || resolved != canonicalTarget {
		t.Fatalf("link %q no longer resolves to %q (got %q, err=%v)", keepaliveLink, canonicalTarget, resolved, err)
	}
}

// TestUpgradeDoesNotTouchRealCacheWhenIsolated is the WP-646 sentinel for the
// update cache: it pre-seeds the REAL cache path (the one DefaultCachePath
// resolves to when AMQ_CACHE_DIR is unset) with a sentinel, then runs an
// upgrade under the TestMain-isolated AMQ_CACHE_DIR, and asserts the real
// sentinel is byte-identical afterward. This catches a regression where a
// test path writes through to ~/Library/Caches/amq/update.json.
func TestUpgradeDoesNotTouchRealCacheWhenIsolated(t *testing.T) {
	// Resolve the REAL cache path by ignoring the TestMain override.
	realCachePath := realDefaultCachePath(t)
	if err := os.MkdirAll(filepath.Dir(realCachePath), 0o700); err != nil {
		t.Fatalf("mkdir real cache dir: %v", err)
	}
	// Save any pre-existing real cache content so the test restores the
	// developer's actual cache state rather than deleting it.
	preExisting, _ := os.ReadFile(realCachePath)
	sentinel := []byte(`{"checked_at":"2026-01-01T00:00:00Z","latest_version":"v0.0.0-sentinel"}`)
	if err := os.WriteFile(realCachePath, sentinel, 0o600); err != nil {
		t.Fatalf("seed sentinel cache: %v", err)
	}
	t.Cleanup(func() {
		if len(preExisting) > 0 {
			_ = os.WriteFile(realCachePath, preExisting, 0o600)
		} else {
			_ = os.Remove(realCachePath)
		}
	})

	// Confirm the isolated cache dir is distinct from the real one (TestMain
	// sets AMQ_CACHE_DIR); an upgrade must write only there.
	isolatedPath, err := update.DefaultCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if isolatedPath == realCachePath {
		t.Fatalf("TestMain did not isolate the cache: isolated path == real path %s", realCachePath)
	}

	bin := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(bin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExecutablePath := executablePathForUpgrade
	executablePathForUpgrade = func() (string, string, error) { return bin, bin, nil }
	t.Cleanup(func() { executablePathForUpgrade = oldExecutablePath })
	oldDetect := detectHomebrewPrefixForUpgrade
	detectHomebrewPrefixForUpgrade = func() string { return "" }
	t.Cleanup(func() { detectHomebrewPrefixForUpgrade = oldDetect })
	oldFetch := fetchLatestTagForUpgrade
	fetchLatestTagForUpgrade = func(context.Context, *http.Client) (string, error) {
		return "v0.0.0-test", nil
	}
	t.Cleanup(func() { fetchLatestTagForUpgrade = oldFetch })
	stubUpgradeNetwork(t)

	if _, _, err := captureEnvOutput(t, func() error { return runUpgrade(nil, "dev") }); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}

	after, err := os.ReadFile(realCachePath)
	if err != nil {
		t.Fatalf("read sentinel after upgrade: %v", err)
	}
	if !bytes.Equal(sentinel, after) {
		t.Fatalf("real cache sentinel was mutated by upgrade: before=%s after=%s", sentinel, after)
	}
}

// realDefaultCachePath computes DefaultCachePath as it would resolve in
// production (AMQ_CACHE_DIR unset), restoring the override immediately so the
// rest of the test runs under TestMain's isolated cache.
func realDefaultCachePath(t *testing.T) string {
	t.Helper()
	saved := os.Getenv(update.EnvCacheDir)
	_ = os.Unsetenv(update.EnvCacheDir)
	path, err := update.DefaultCachePath()
	if saved != "" {
		_ = os.Setenv(update.EnvCacheDir, saved)
	} else {
		_ = os.Unsetenv(update.EnvCacheDir)
	}
	if err != nil {
		t.Fatalf("resolve real cache path: %v", err)
	}
	return path
}
