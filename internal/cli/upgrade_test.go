//go:build unix

package cli

import (
	"context"
	debugbuildinfo "debug/buildinfo"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/update"
)

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
	fetchChecksumsForUpgrade = func(_ context.Context, _ *http.Client, tag string) (map[string]string, error) {
		// Return a checksum for every asset name the upgrade loop can request
		// (amq + each companion) so the lookup always hits regardless of OS.
		m := map[string]string{}
		for _, name := range append([]string{update.BinaryName}, update.CompanionBinaries...) {
			if asset, err := update.AssetNameFor(name, tag, runtime.GOOS, runtime.GOARCH); err == nil {
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

func stubHomebrewPrefixProbe(t *testing.T, prefixes ...string) {
	t.Helper()
	oldProbe := runBrewPrefixForHomebrewUpgrade
	want := make(map[string]string, len(prefixes))
	for _, prefix := range prefixes {
		prefix := canonicalHomebrewPrefix(prefix)
		manager := filepath.Join(prefix, "bin", "brew")
		want[manager] = prefix
	}
	runBrewPrefixForHomebrewUpgrade = func(_ context.Context, path string) ([]byte, error) {
		if prefix, ok := want[update.CanonicalPath(path)]; ok {
			return []byte(prefix + "\n"), nil
		}
		return nil, errors.New("unexpected Homebrew manager probe")
	}
	t.Cleanup(func() { runBrewPrefixForHomebrewUpgrade = oldProbe })
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
	oldBrewExists := homebrewBrewExistsForUpgrade
	homebrewBrewExistsForUpgrade = func(path string) bool {
		return update.CanonicalPath(path) == filepath.Join(canonicalHomebrewPrefix(prefix), "bin", "brew")
	}
	t.Cleanup(func() { homebrewBrewExistsForUpgrade = oldBrewExists })
	stubHomebrewPrefixProbe(t, prefix)
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
	if !strings.Contains(stdout, "amq-keepalive: a running supervisor picks up a strictly newer image") {
		t.Fatalf("output missing keepalive self-upgrade remedy:\n%s", stdout)
	}
}
