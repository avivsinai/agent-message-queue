//go:build unix

package cli

import (
	"context"
	debugbuildinfo "debug/buildinfo"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
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
	oldInstallations := detectHomebrewInstallationsForUpgrade
	detectHomebrewInstallationsForUpgrade = func() []homebrewInstallation {
		return []homebrewInstallation{{
			prefix:     filepath.Join(base, "homebrew"),
			executable: filepath.Join(base, "homebrew", "bin", "brew"),
		}}
	}
	t.Cleanup(func() { detectHomebrewInstallationsForUpgrade = oldInstallations })
	oldBrewExists := homebrewBrewExistsForUpgrade
	homebrewBrewExistsForUpgrade = func(path string) bool {
		return path == filepath.Join(canonicalHomebrewPrefix(filepath.Join(base, "homebrew")), "bin", "brew")
	}
	t.Cleanup(func() { homebrewBrewExistsForUpgrade = oldBrewExists })
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
	oldInstallations := detectHomebrewInstallationsForUpgrade
	detectHomebrewInstallationsForUpgrade = func() []homebrewInstallation {
		return []homebrewInstallation{{prefix: prefix, executable: filepath.Join(prefix, "bin", "brew")}}
	}
	t.Cleanup(func() { detectHomebrewInstallationsForUpgrade = oldInstallations })

	t.Run("severed regular prefix binary recommends reinstall", func(t *testing.T) {
		if err := os.WriteFile(binPath, []byte("self-upgraded"), 0o755); err != nil {
			t.Fatalf("write severed prefix binary: %v", err)
		}

		_, err := selectUpgradeDestinationWithRoots(binPath, binPath, func(string) error { return nil }, []string{prefix}, "")
		if err == nil || !strings.Contains(err.Error(), "brew reinstall amq (if Homebrew owns it)") || !strings.Contains(err.Error(), "move the binary to ~/.local/bin and rerun amq upgrade (if you installed it manually)") {
			t.Fatalf("selectUpgradeDestination error = %v, want both Homebrew remedies", err)
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

		_, err := selectUpgradeDestinationWithRoots(binPath, binPath, func(string) error { return nil }, []string{prefix}, "")
		if err == nil {
			t.Fatal("selectUpgradeDestination accepted a broken Homebrew symlink")
		}
	})

	t.Run("healthy Cellar symlink refuses", func(t *testing.T) {
		if err := os.Remove(binPath); err != nil {
			t.Fatalf("remove dangling symlink: %v", err)
		}
		if err := os.Symlink(cellarPath, binPath); err != nil {
			t.Fatalf("create healthy Cellar symlink: %v", err)
		}

		_, err := selectUpgradeDestinationWithRoots(binPath, cellarPath, func(string) error { return nil }, []string{prefix}, "")
		if err == nil || !strings.Contains(err.Error(), "brew update && brew upgrade amq") {
			t.Fatalf("selectUpgradeDestination error = %v, want Homebrew remedy", err)
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

		got, err := selectUpgradeDestination(brewBinPath, brewBinPath, func(string) error { return nil })
		if err != nil {
			t.Fatalf("selectUpgradeDestination(%q): %v", brewBinPath, err)
		}
		if got != brewBinPath {
			t.Fatalf("destination = %q, want %q", got, brewBinPath)
		}
		if _, err := selectUpgradeDestination(brewCellarPath, brewCellarPath, func(string) error { return nil }); err == nil ||
			!strings.Contains(err.Error(), "amq lives in a Homebrew Cellar") {
			t.Fatalf("selectUpgradeDestination(%q) error = %v, want executable-derived Homebrew refusal", brewCellarPath, err)
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
	oldCellarExists := homebrewCellarExistsForUpgrade
	homebrewCellarExistsForUpgrade = func(string) bool { return false }
	t.Cleanup(func() { homebrewCellarExistsForUpgrade = oldCellarExists })

	if got := detectHomebrewPrefix(); got != want {
		t.Fatalf("detectHomebrewPrefix() = %q, want %q", got, want)
	}
}

func TestDetectHomebrewInstallationsCollectsAllCandidates(t *testing.T) {
	envPrefix := filepath.Join(t.TempDir(), "env-homebrew")
	probePrefix := filepath.Join(t.TempDir(), "probe-homebrew")
	fakeHome := t.TempDir()
	t.Setenv("HOMEBREW_PREFIX", envPrefix)
	oldLookPath := lookPathForHomebrewUpgrade
	lookPathForHomebrewUpgrade = func(name string) (string, error) {
		if name != "brew" {
			t.Fatalf("LookPath name = %q, want brew", name)
		}
		return "/custom/bin/brew", nil
	}
	t.Cleanup(func() { lookPathForHomebrewUpgrade = oldLookPath })
	oldRun := runBrewPrefixForHomebrewUpgrade
	runBrewPrefixForHomebrewUpgrade = func(context.Context, string) ([]byte, error) {
		return []byte(probePrefix + "\n"), nil
	}
	t.Cleanup(func() { runBrewPrefixForHomebrewUpgrade = oldRun })
	oldHome := homeDirForUpgrade
	homeDirForUpgrade = func() (string, error) { return fakeHome, nil }
	t.Cleanup(func() { homeDirForUpgrade = oldHome })
	oldExists := homebrewBrewExistsForUpgrade
	homebrewBrewExistsForUpgrade = func(path string) bool {
		return path == filepath.Join("/usr/local", "bin", "brew")
	}
	t.Cleanup(func() { homebrewBrewExistsForUpgrade = oldExists })
	oldCellarExists := homebrewCellarExistsForUpgrade
	homebrewCellarExistsForUpgrade = func(path string) bool {
		return path == filepath.Join("/home/linuxbrew/.linuxbrew", "Cellar") ||
			path == filepath.Join(fakeHome, ".linuxbrew", "Cellar")
	}
	t.Cleanup(func() { homebrewCellarExistsForUpgrade = oldCellarExists })

	installations := detectHomebrewInstallations()
	prefixes := homebrewPrefixesFromInstallations(installations)
	for _, want := range []string{envPrefix, probePrefix, "/usr/local", "/home/linuxbrew/.linuxbrew", filepath.Join(fakeHome, ".linuxbrew")} {
		found := false
		canonicalWant := canonicalHomebrewPrefix(want)
		for _, got := range prefixes {
			if got == canonicalWant {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("detected Homebrew prefixes = %#v, missing %q", prefixes, want)
		}
	}
	for _, unwanted := range []string{"/opt/homebrew", "/opt/linuxbrew/.linuxbrew"} {
		if slices.Contains(prefixes, canonicalHomebrewPrefix(unwanted)) {
			t.Fatalf("unevidenced Homebrew prefix %q was detected: %#v", unwanted, prefixes)
		}
	}
	for _, installation := range installations {
		if installation.prefix == canonicalHomebrewPrefix(probePrefix) && installation.executable != "/custom/bin/brew" {
			t.Fatalf("probe installation executable = %q, want exact PATH executable", installation.executable)
		}
	}
}

func TestRunUpgradeDerivesUnknownHomebrewCellarPrefix(t *testing.T) {
	const cellarBinary = "/srv/homebrew/Cellar/amq/0.74.0/bin/amq"
	t.Setenv("HOMEBREW_PREFIX", "")
	oldExecutablePath := executablePathForUpgrade
	executablePathForUpgrade = func() (string, string, error) { return cellarBinary, cellarBinary, nil }
	t.Cleanup(func() { executablePathForUpgrade = oldExecutablePath })
	oldInstallations := detectHomebrewInstallationsForUpgrade
	detectHomebrewInstallationsForUpgrade = func() []homebrewInstallation { return nil }
	t.Cleanup(func() { detectHomebrewInstallationsForUpgrade = oldInstallations })
	oldDetect := detectHomebrewPrefixForUpgrade
	detectHomebrewPrefixForUpgrade = func() string { return "" }
	t.Cleanup(func() { detectHomebrewPrefixForUpgrade = oldDetect })
	oldLookPath := lookPathForHomebrewUpgrade
	lookPathForHomebrewUpgrade = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { lookPathForHomebrewUpgrade = oldLookPath })
	oldBrewExists := homebrewBrewExistsForUpgrade
	homebrewBrewExistsForUpgrade = func(string) bool { return false }
	t.Cleanup(func() { homebrewBrewExistsForUpgrade = oldBrewExists })
	oldFetch := fetchLatestTagForUpgrade
	fetchCalls := 0
	fetchLatestTagForUpgrade = func(context.Context, *http.Client) (string, error) {
		fetchCalls++
		return "v0.0.0-test", nil
	}
	t.Cleanup(func() { fetchLatestTagForUpgrade = oldFetch })

	err := runUpgrade(nil, "dev")
	want := "amq lives in a Homebrew Cellar at " + cellarBinary + " but no brew executable was found at /srv/homebrew/bin/brew; run /srv/homebrew/bin/brew upgrade amq from that installation, or reinstall amq under ~/.local/bin for direct upgrades"
	if err == nil || err.Error() != want {
		t.Fatalf("runUpgrade error = %v, want %q", err, want)
	}
	if fetchCalls != 0 {
		t.Fatalf("release lookups = %d, want 0 before Homebrew prefix refusal", fetchCalls)
	}
}

func TestDerivedRawHomebrewPrefixProtectsEvidencedBin(t *testing.T) {
	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "Cellar"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(prefix, "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("manual"), 0o755); err != nil {
		t.Fatal(err)
	}

	derived := derivedHomebrewPrefixes(path, path)
	if len(derived) != 1 || canonicalHomebrewPrefix(derived[0].prefix) != canonicalHomebrewPrefix(prefix) {
		t.Fatalf("derived prefixes = %#v, want %q", derived, prefix)
	}
	installations, err := addDerivedHomebrewInstallations(path, path, nil)
	if err != nil {
		t.Fatalf("addDerivedHomebrewInstallations: %v", err)
	}
	got := homebrewPrefixesFromInstallations(installations)
	if len(got) != 1 || got[0] != canonicalHomebrewPrefix(prefix) {
		t.Fatalf("derived installation prefixes = %#v, want [%q]", got, canonicalHomebrewPrefix(prefix))
	}
	err = refusePackageManagedDestination(path, got, "")
	if err == nil || !strings.Contains(err.Error(), "brew reinstall amq (if Homebrew owns it)") ||
		!strings.Contains(err.Error(), "move the binary to ~/.local/bin and rerun amq upgrade (if you installed it manually)") {
		t.Fatalf("refusePackageManagedDestination error = %v, want both manual/Homebrew remedies", err)
	}
}

func TestSkipUnavailableCompanionsOnWindows(t *testing.T) {
	all := true
	stdout, _, err := captureEnvOutput(t, func() error {
		return skipUnavailableCompanionsOnWindows(&all, "windows")
	})
	if err != nil {
		t.Fatalf("skipUnavailableCompanionsOnWindows: %v", err)
	}
	if all {
		t.Fatal("--all remained enabled on Windows")
	}
	if stdout != "--all: companion binaries are not published for Windows; skipping\n" {
		t.Fatalf("stdout = %q, want exact Windows skip line", stdout)
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

func stubExpectedCompanionBuildInfo(t *testing.T) {
	t.Helper()
	oldReadBuildInfo := readBuildInfoForUpgrade
	readBuildInfoForUpgrade = func(path string) (*debugbuildinfo.BuildInfo, error) {
		name := strings.TrimSuffix(filepath.Base(path), ".exe")
		return &debugbuildinfo.BuildInfo{Path: update.ModulePath + "/cmd/" + name}, nil
	}
	t.Cleanup(func() { readBuildInfoForUpgrade = oldReadBuildInfo })
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

func TestRunUpgradeWritesCacheToOverride(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv(update.EnvCacheDir, cacheDir)
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
	stubUpgradeNetwork(t)
	oldSave := saveUpgradeCacheForUpgrade
	saveUpgradeCacheForUpgrade = saveUpgradeCache
	t.Cleanup(func() { saveUpgradeCacheForUpgrade = oldSave })

	if _, _, err := captureEnvOutput(t, func() error { return runUpgrade(nil, "dev") }); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	path, err := update.DefaultCachePath()
	if err != nil {
		t.Fatalf("DefaultCachePath: %v", err)
	}
	cache, err := update.LoadCache(path)
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if cache == nil || cache.LatestVersion != "v0.0.0-test" {
		t.Fatalf("cache = %#v, want latest v0.0.0-test under %s", cache, cacheDir)
	}
}

func TestRunUpgradeScheduledSkipsVersionCache(t *testing.T) {
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
	stubUpgradeNetwork(t)
	oldReplace := replaceBinaryForUpgrade
	replaceBinaryForUpgrade = func(_, _ string) (bool, error) { return true, nil }
	t.Cleanup(func() { replaceBinaryForUpgrade = oldReplace })
	cacheWrites := 0
	oldSave := saveUpgradeCacheForUpgrade
	saveUpgradeCacheForUpgrade = func(string) { cacheWrites++ }
	t.Cleanup(func() { saveUpgradeCacheForUpgrade = oldSave })

	stdout, _, err := captureEnvOutput(t, func() error { return runUpgrade(nil, "dev") })
	if err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if cacheWrites != 0 {
		t.Fatalf("scheduled replacement wrote version cache %d times", cacheWrites)
	}
	if !strings.Contains(stdout, "replacement scheduled; version cache updates on next run") {
		t.Fatalf("scheduled output missing cache timing: %s", stdout)
	}
}

func TestFindCompanionRejectsSymlinkToAMQ(t *testing.T) {
	dir := t.TempDir()
	amqDest := filepath.Join(dir, "amq")
	if err := os.WriteFile(amqDest, []byte("amq"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(amqDest, filepath.Join(dir, "amq-keepalive")); err != nil {
		t.Fatal(err)
	}
	_, err := findCompanionMatches("amq-keepalive", []string{dir}, amqDest, nil, "")
	if err == nil || !strings.Contains(err.Error(), "is the amq executable") {
		t.Fatalf("findCompanionMatches error = %v, want same-file refusal", err)
	}
}

func TestFindCompanionRejectsPackageManagedSymlink(t *testing.T) {
	prefix := t.TempDir()
	target := filepath.Join(prefix, "Cellar", "amq", "0.74.0", "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("brew"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "amq-keepalive")); err != nil {
		t.Fatal(err)
	}
	_, err := findCompanionMatches("amq-keepalive", []string{dir}, "", []string{prefix}, "")
	if err == nil || !strings.Contains(err.Error(), "package-managed") {
		t.Fatalf("findCompanionMatches error = %v, want package-managed refusal", err)
	}
}

func TestFindCompanionRejectsBrokenSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "amq-keepalive")); err != nil {
		t.Fatal(err)
	}
	_, err := findCompanionMatches("amq-keepalive", []string{dir}, "", nil, "")
	if err == nil || !strings.Contains(err.Error(), "cannot resolve companion symlink") {
		t.Fatalf("findCompanionMatches error = %v, want broken-link refusal", err)
	}
}

func TestFindCompanionRejectsNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "amq-keepalive")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := findCompanionMatches("amq-keepalive", []string{dir}, "", nil, "")
	if err == nil || !strings.Contains(err.Error(), "not a regular file or symlink") {
		t.Fatalf("findCompanionMatches error = %v, want non-regular refusal", err)
	}
}

func TestUpgradeCompanionsRejectsDistinctDuplicateTargets(t *testing.T) {
	stubExpectedCompanionBuildInfo(t)
	firstDir := t.TempDir()
	secondHome := t.TempDir()
	secondDir := filepath.Join(secondHome, ".local", "bin")
	if err := os.MkdirAll(secondDir, 0o700); err != nil {
		t.Fatal(err)
	}
	amqDest := filepath.Join(firstDir, "amq")
	if err := os.WriteFile(amqDest, []byte("amq"), 0o755); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(firstDir, "amq-keepalive")
	second := filepath.Join(secondDir, "amq-keepalive")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldHome := homeDirForUpgrade
	homeDirForUpgrade = func() (string, error) { return secondHome, nil }
	t.Cleanup(func() { homeDirForUpgrade = oldHome })
	oldCompanions := companionBinariesForUpgrade
	companionBinariesForUpgrade = []string{"amq-keepalive"}
	t.Cleanup(func() { companionBinariesForUpgrade = oldCompanions })

	err := upgradeCompanionsWithProtection(context.Background(), nil, "v0.0.0-test", "v0.0.0-test", amqDest, nil, "")
	if err == nil || !strings.Contains(err.Error(), "multiple distinct targets") ||
		!strings.Contains(err.Error(), first) || !strings.Contains(err.Error(), second) {
		t.Fatalf("upgradeCompanions error = %v, want both duplicate paths", err)
	}
}

func TestUpgradeCompanionsPlansEveryTargetBeforeReplacement(t *testing.T) {
	binDir := t.TempDir()
	amqDest := filepath.Join(binDir, "amq")
	keepalive := filepath.Join(binDir, "amq-keepalive")
	bridge := filepath.Join(binDir, "amq-bridge")
	for _, path := range []string{amqDest, keepalive, bridge} {
		if err := os.WriteFile(path, []byte(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldCompanions := companionBinariesForUpgrade
	companionBinariesForUpgrade = []string{"amq-keepalive", "amq-bridge"}
	t.Cleanup(func() { companionBinariesForUpgrade = oldCompanions })
	oldReadBuildInfo := readBuildInfoForUpgrade
	readBuildInfoForUpgrade = func(path string) (*debugbuildinfo.BuildInfo, error) {
		name := strings.TrimSuffix(filepath.Base(path), ".exe")
		if name == "amq-bridge" {
			return &debugbuildinfo.BuildInfo{Path: update.ModulePath + "/cmd/amq"}, nil
		}
		return &debugbuildinfo.BuildInfo{Path: update.ModulePath + "/cmd/" + name}, nil
	}
	t.Cleanup(func() { readBuildInfoForUpgrade = oldReadBuildInfo })
	replaced := stubUpgradeNetwork(t)

	err := upgradeCompanionsWithProtection(context.Background(), nil, "v0.0.0-test", "v0.0.0-test", amqDest, nil, "")
	if err == nil || !strings.Contains(err.Error(), bridge+" is not an amq-bridge build (found "+update.ModulePath+"/cmd/amq); remove or repoint it") {
		t.Fatalf("upgradeCompanions error = %v, want unrelated Go build refusal", err)
	}
	if len(*replaced) != 0 {
		t.Fatalf("replacements = %#v, want none when a later target fails verification", *replaced)
	}
	_ = keepalive
}

func TestUpgradeCompanionsRejectsCrossCompanionAlias(t *testing.T) {
	binDir := t.TempDir()
	amqDest := filepath.Join(binDir, "amq")
	target := filepath.Join(t.TempDir(), "amq-keepalive")
	for _, path := range []string{amqDest, target} {
		if err := os.WriteFile(path, []byte(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"amq-keepalive", "amq-bridge"} {
		if err := os.Symlink(target, filepath.Join(binDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	oldCompanions := companionBinariesForUpgrade
	companionBinariesForUpgrade = []string{"amq-keepalive", "amq-bridge"}
	t.Cleanup(func() { companionBinariesForUpgrade = oldCompanions })
	stubExpectedCompanionBuildInfo(t)
	replaced := stubUpgradeNetwork(t)

	err := upgradeCompanionsWithProtection(context.Background(), nil, "v0.0.0-test", "v0.0.0-test", amqDest, nil, "")
	if err == nil || !strings.Contains(err.Error(), "repoint "+filepath.Join(binDir, "amq-bridge")+" to a dedicated amq-bridge binary or remove it") {
		t.Fatalf("upgradeCompanions error = %v, want cross-companion alias refusal", err)
	}
	if len(*replaced) != 0 {
		t.Fatalf("replacements = %#v, want none for cross-companion alias", *replaced)
	}
}

func TestFindCompanionRejectsNonGoBinary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "amq-keepalive")
	if err := os.WriteFile(path, []byte("not a Go binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := findCompanionMatches("amq-keepalive", []string{dir}, "", nil, "")
	if err == nil || !strings.Contains(err.Error(), path+" is not an amq-keepalive build (found non-Go binary); remove or repoint it") {
		t.Fatalf("findCompanionMatches error = %v, want non-Go build refusal", err)
	}
}

func TestVerifyCompanionBuildUsesModulePath(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", ".."))
	binary := filepath.Join(t.TempDir(), "amq-acp")
	build := exec.Command("go", "build", "-o", binary, "./cmd/amq-acp")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/amq-acp: %v\n%s", err, output)
	}

	oldReadBuildInfo := readBuildInfoForUpgrade
	readBuildInfoForUpgrade = debugbuildinfo.ReadFile
	t.Cleanup(func() { readBuildInfoForUpgrade = oldReadBuildInfo })

	item := companionTarget{name: "amq-acp", target: binary}
	if err := verifyCompanionBuild(item); err != nil {
		t.Fatalf("verifyCompanionBuild(%s): %v", item.name, err)
	}
	item.name = "amq-bridge"
	if err := verifyCompanionBuild(item); err == nil || !strings.Contains(err.Error(), "found "+update.ModulePath+"/cmd/amq-acp") {
		t.Fatalf("verifyCompanionBuild(%s) = %v, want module-path mismatch", item.name, err)
	}
}

func TestRunUpgradeClassifiesAgainstAllHomebrewPrefixes(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	amqPath := filepath.Join(second, "Cellar", "amq", "0.74.0", "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(amqPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(amqPath, []byte("brew"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExecutablePath := executablePathForUpgrade
	executablePathForUpgrade = func() (string, string, error) { return amqPath, amqPath, nil }
	t.Cleanup(func() { executablePathForUpgrade = oldExecutablePath })
	oldInstallations := detectHomebrewInstallationsForUpgrade
	detectHomebrewInstallationsForUpgrade = func() []homebrewInstallation {
		return []homebrewInstallation{
			{prefix: first, executable: filepath.Join(first, "bin", "brew")},
			{prefix: second, executable: filepath.Join(second, "bin", "brew")},
		}
	}
	t.Cleanup(func() { detectHomebrewInstallationsForUpgrade = oldInstallations })
	oldDetect := detectHomebrewPrefixForUpgrade
	detectHomebrewPrefixForUpgrade = func() string { return "" }
	t.Cleanup(func() { detectHomebrewPrefixForUpgrade = oldDetect })
	oldBrewExists := homebrewBrewExistsForUpgrade
	homebrewBrewExistsForUpgrade = func(path string) bool {
		return path == filepath.Join(canonicalHomebrewPrefix(second), "bin", "brew")
	}
	t.Cleanup(func() { homebrewBrewExistsForUpgrade = oldBrewExists })
	oldDelegate := runDelegateForUpgrade
	var ran []string
	runDelegateForUpgrade = func(argv []string) error { ran = append(ran, strings.Join(argv, " ")); return nil }
	t.Cleanup(func() { runDelegateForUpgrade = oldDelegate })

	if _, _, err := captureEnvOutput(t, func() error { return runUpgrade([]string{"-y"}, "dev") }); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	manager := filepath.Join(second, "bin", "brew")
	if len(ran) != 2 || ran[0] != manager+" update" || ran[1] != manager+" upgrade amq" {
		t.Fatalf("delegation = %#v, want manager %s", ran, manager)
	}
}

func TestRunDirectUpgradeFinalCellarGuard(t *testing.T) {
	prefix := t.TempDir()
	dest := filepath.Join(prefix, "Cellar", "amq", "0.74.0", "bin", "amq")
	stubUpgradeNetwork(t)
	replaced := false
	oldReplace := replaceBinaryForUpgrade
	replaceBinaryForUpgrade = func(_, _ string) (bool, error) { replaced = true; return false, nil }
	t.Cleanup(func() { replaceBinaryForUpgrade = oldReplace })
	_, err := runDirectUpgradeWithProtection(context.Background(), nil, "amq", "v0.0.0-test", "v0.0.0-test", dest, []string{prefix}, "")
	if err == nil || !strings.Contains(err.Error(), "brew update && brew upgrade amq") {
		t.Fatalf("runDirectUpgrade error = %v, want independent Cellar guard", err)
	}
	if replaced {
		t.Fatal("final Cellar guard ran replacement")
	}
}

func TestRunBrewPrefixIsBounded(t *testing.T) {
	dir := t.TempDir()
	brew := filepath.Join(dir, "brew")
	if err := os.WriteFile(brew, []byte("#!/bin/sh\nsleep 60 &\nprintf '%s\\n' /tmp/test-prefix\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := runBrewPrefix(context.Background(), brew)
	if err == nil {
		t.Fatal("runBrewPrefix succeeded despite a child holding stdout")
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("runBrewPrefix took %v, want bounded cleanup", elapsed)
	}
}

func TestRunUpgradeHomebrewDelegatesWithYes(t *testing.T) {
	prefix := t.TempDir()
	bin := filepath.Join(prefix, "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	cellar := filepath.Join(prefix, "Cellar", "amq", "0.0.0", "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(cellar), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cellar, []byte("homebrew"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cellar, bin); err != nil {
		t.Fatal(err)
	}
	oldExecutablePath := executablePathForUpgrade
	executablePathForUpgrade = func() (string, string, error) { return bin, bin, nil }
	t.Cleanup(func() { executablePathForUpgrade = oldExecutablePath })
	oldClassify := classifyInstallForUpgrade
	classifyInstallForUpgrade = func(string, string) update.InstallKind { return update.InstallHomebrew }
	t.Cleanup(func() { classifyInstallForUpgrade = oldClassify })
	oldInstallations := detectHomebrewInstallationsForUpgrade
	detectHomebrewInstallationsForUpgrade = func() []homebrewInstallation {
		return []homebrewInstallation{{prefix: prefix, executable: filepath.Join(prefix, "bin", "brew")}}
	}
	t.Cleanup(func() { detectHomebrewInstallationsForUpgrade = oldInstallations })
	oldDetect := detectHomebrewPrefixForUpgrade
	detectHomebrewPrefixForUpgrade = func() string { return "" }
	t.Cleanup(func() { detectHomebrewPrefixForUpgrade = oldDetect })
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
	manager := filepath.Join(prefix, "bin", "brew")
	if len(delegateRan) != 2 || delegateRan[0] != manager+" update" || delegateRan[1] != manager+" upgrade amq" {
		t.Fatalf("delegate ran %#v, want [%s update, %s upgrade amq]", delegateRan, manager, manager)
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
	oldBrewExists := homebrewBrewExistsForUpgrade
	homebrewBrewExistsForUpgrade = func(path string) bool {
		return path == filepath.Join(canonicalHomebrewPrefix(prefix), "bin", "brew")
	}
	t.Cleanup(func() { homebrewBrewExistsForUpgrade = oldBrewExists })
	oldInstallations := detectHomebrewInstallationsForUpgrade
	detectHomebrewInstallationsForUpgrade = func() []homebrewInstallation {
		return []homebrewInstallation{{prefix: prefix, executable: filepath.Join(prefix, "bin", "brew")}}
	}
	t.Cleanup(func() { detectHomebrewInstallationsForUpgrade = oldInstallations })
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
	wantManager := filepath.Join(prefix, "bin", "brew")
	if len(delegateRan) != 2 || delegateRan[0] != wantManager+" update" || delegateRan[1] != wantManager+" upgrade amq" {
		t.Fatalf("delegate ran %#v, want [%s update, %s upgrade amq]", delegateRan, wantManager, wantManager)
	}
}

func TestRunUpgradeAllReplacesPresentCompanionsSkipsMissing(t *testing.T) {
	stubExpectedCompanionBuildInfo(t)
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
	want := map[string]bool{amqBin: false}
	for _, path := range []string{keepalive, bridge} {
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		want[canonical] = false
	}
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
	stubExpectedCompanionBuildInfo(t)
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
