package update

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		current string
		latest  string
		want    int
		ok      bool
	}{
		{"v1.2.3", "v1.2.3", 0, true},
		{"v1.2.3", "v1.2.4", -1, true},
		{"1.2.3", "v1.2.4", -1, true},
		{"v2.0.0", "v1.9.9", 1, true},
		{"v10.20.30", "v10.20.31", -1, true},
		{"v10.21.0", "v10.20.99", 1, true},
		{"v10.0.0", "v1.0.0", 1, true},
		{"v1.2.3-alpha", "v1.2.4+build", -1, true},
		{"", "v1.2.3", 0, false},
		{"v1.2.3", "", 0, false},
		{"v1.x.3", "v1.2.3", 0, false},
		{"v1.2", "v1.2.3", 0, false},
		{"v1.2.3.4", "v1.2.3", 0, false},
		{"v1.2.-3", "v1.2.3", 0, false},
		{"dev", "v1.2.3", 0, false},
	}

	for _, tc := range cases {
		got, ok := CompareVersions(tc.current, tc.latest)
		if ok != tc.ok {
			t.Fatalf("CompareVersions(%q, %q) ok=%v want %v", tc.current, tc.latest, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Fatalf("CompareVersions(%q, %q)=%d want %d", tc.current, tc.latest, got, tc.want)
		}
	}
}

func TestNormalizeVersion(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"1.2.3", "v1.2.3"},
		{"v1.2.3", "v1.2.3"},
		{"dev", "dev"},
		{" 1.2.3 ", "v1.2.3"},
		{"1foo", "v1foo"},
		{"0foo", "v0foo"},
		{"9foo", "v9foo"},
		{"-1.2.3", "-1.2.3"},
		{"", ""},
	}

	for _, tc := range cases {
		got := NormalizeVersion(tc.input)
		if got != tc.want {
			t.Fatalf("NormalizeVersion(%q)=%q want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	data := []byte(`abcd1234  amq_0.1.0_darwin_arm64.tar.gz
efgh5678 amq_0.1.0_linux_amd64.tar.gz

`) // trailing newline

	checksums, err := ParseChecksums(data)
	if err != nil {
		t.Fatalf("ParseChecksums: %v", err)
	}
	if checksums["amq_0.1.0_darwin_arm64.tar.gz"] != "abcd1234" {
		t.Fatalf("checksum mismatch for darwin asset")
	}
	if checksums["amq_0.1.0_linux_amd64.tar.gz"] != "efgh5678" {
		t.Fatalf("checksum mismatch for linux asset")
	}
}

func TestAssetName(t *testing.T) {
	name, err := AssetName("v0.1.0", "darwin", "arm64")
	if err != nil {
		t.Fatalf("AssetName error: %v", err)
	}
	if name != "amq_0.1.0_darwin_arm64.tar.gz" {
		t.Fatalf("AssetName=%q", name)
	}
	winName, err := AssetName("v0.1.0", "windows", "amd64")
	if err != nil {
		t.Fatalf("AssetName windows error: %v", err)
	}
	if winName != "amq_0.1.0_windows_amd64.zip" {
		t.Fatalf("AssetName windows=%q", winName)
	}
}

func TestAssetNameForCompanions(t *testing.T) {
	cases := []struct {
		binary string
		goos   string
		want   string
	}{
		{"amq-keepalive", "darwin", "amq-keepalive_0.1.0_darwin_arm64.tar.gz"},
		{"amq-bridge", "linux", "amq-bridge_0.1.0_linux_amd64.tar.gz"},
		{"amq-acp", "darwin", "amq-acp_0.1.0_darwin_arm64.tar.gz"},
	}
	for _, tc := range cases {
		name, err := AssetNameFor(tc.binary, "v0.1.0", tc.goos, "arm64")
		if tc.goos == "linux" {
			name, err = AssetNameFor(tc.binary, "v0.1.0", tc.goos, "amd64")
		}
		if err != nil {
			t.Fatalf("AssetNameFor(%q): %v", tc.binary, err)
		}
		if name != tc.want {
			t.Fatalf("AssetNameFor(%q)=%q want %q", tc.binary, name, tc.want)
		}
	}
	// The core amq asset matches AssetName exactly.
	core, err := AssetNameFor("amq", "v0.1.0", "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if core != "amq_0.1.0_darwin_arm64.tar.gz" {
		t.Fatalf("AssetNameFor(amq)=%q", core)
	}
	if _, err := AssetNameFor("", "v0.1.0", "darwin", "arm64"); err == nil {
		t.Fatal("AssetNameFor(empty binary) want error")
	}
	if _, err := AssetNameFor("amq", "", "darwin", "arm64"); err == nil {
		t.Fatal("AssetNameFor(empty tag) want error")
	}
	for _, binary := range []string{"../amq", `amq\escape`, "amq/escape", "amq.exe"} {
		if _, err := AssetNameFor(binary, "v0.1.0", "darwin", "arm64"); err == nil {
			t.Fatalf("AssetNameFor(%q) want invalid binary error", binary)
		}
	}
	for _, tag := range []string{"../../victim", "v0.1", "v01.2.3", "v0.1.0/escape", "v0.1.0?bad"} {
		if _, err := AssetNameFor("amq", tag, "darwin", "arm64"); err == nil {
			t.Fatalf("AssetNameFor tag %q want invalid release error", tag)
		}
	}
}

func TestClassifyInstall(t *testing.T) {
	prefix := t.TempDir()
	binPath := filepath.Join(prefix, "bin", "amq")
	cellarPath := filepath.Join(prefix, "Cellar", "amq", "0.73.0", "bin", "amq")
	manualPath := filepath.Join(t.TempDir(), "bin", "amq")

	cases := []struct {
		name   string
		path   string
		prefix string
		want   InstallKind
	}{
		{"cellar binary", cellarPath, prefix, InstallHomebrew},
		{"regular prefix bin remains ambiguous", binPath, prefix, InstallDirect},
		{"manual install", manualPath, prefix, InstallDirect},
		{"manual install no prefix", manualPath, "", InstallDirect},
		{"empty path", "", prefix, InstallDirect},
		{"cellar path but no prefix detected", cellarPath, "", InstallDirect},
		// A sibling dir that merely shares a prefix STRING must not match.
		{"prefix-like sibling", prefix + "-other/bin/amq", prefix, InstallDirect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyInstall(tc.path, tc.prefix)
			if got != tc.want {
				t.Fatalf("ClassifyInstall(%q, %q)=%v want %v", tc.path, tc.prefix, got, tc.want)
			}
		})
	}
}

// TestClassifyInstallResolvesSymlinkIntoCellar is the bead's negative case:
// a symlink INTO the Cellar is classified by its resolved target, so it is
// Homebrew-owned (the operator must not self-replace through the link).
func TestClassifyInstallResolvesSymlinkIntoCellar(t *testing.T) {
	rawPrefix := t.TempDir()
	// Canonicalize the prefix the way `brew --prefix` would: EvalSymlinks so
	// /var/folders and /private/var/folders agree on macOS.
	prefix, err := filepath.EvalSymlinks(rawPrefix)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(prefix, "Cellar", "amq", "0.73.0", "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("homebrew"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "amq-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if got := ClassifyInstall(resolved, prefix); got != InstallHomebrew {
		t.Fatalf("ClassifyInstall(resolved symlink into Cellar)=%v want %v", got, InstallHomebrew)
	}
}

func TestClassifyInstallAgainstPrefixesFindsAnyCellar(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	path := filepath.Join(second, "Cellar", "amq", "0.73.0", "bin", "amq")
	if got := ClassifyInstallAgainstPrefixes(path, []string{first, second}); got != InstallHomebrew {
		t.Fatalf("ClassifyInstallAgainstPrefixes(%q)=%v want %v", path, got, InstallHomebrew)
	}
}

func TestClassifyInstallDoesNotTreatDirectSymlinkInHomebrewBinAsOwned(t *testing.T) {
	prefix := t.TempDir()
	target := filepath.Join(t.TempDir(), "versions", "amq")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("direct"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(prefix, "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got := ClassifyInstall(link, prefix); got != InstallDirect {
		t.Fatalf("ClassifyInstall(%q, %q)=%v want direct", link, prefix, got)
	}
}

func TestClassifyInstallScoop(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Scoop classification is Windows-only")
	}
	home := t.TempDir()
	t.Setenv("SCOOP", home)
	appsDir := filepath.Join(home, "apps", "amq", "current")
	path := filepath.Join(appsDir, "amq.exe")
	if got := ClassifyInstall(path, ""); got != InstallScoop {
		t.Fatalf("ClassifyInstall(scoop apps)=%v want %v", got, InstallScoop)
	}
	// Outside the apps dir is direct.
	other := filepath.Join(home, "bin", "amq.exe")
	if got := ClassifyInstall(other, ""); got != InstallDirect {
		t.Fatalf("ClassifyInstall(non-scoop)=%v want %v", got, InstallDirect)
	}
}

func TestClassifyInstallLinuxbrew(t *testing.T) {
	for _, prefix := range []string{"/home/linuxbrew/.linuxbrew", "/home/u/.linuxbrew"} {
		cellarPath := filepath.Join(prefix, "Cellar", "amq", "0.73.0", "bin", "amq")
		if got := ClassifyInstall(cellarPath, prefix); got != InstallHomebrew {
			t.Fatalf("ClassifyInstall(%q, %q)=%v want %v", cellarPath, prefix, got, InstallHomebrew)
		}
		binPath := filepath.Join(prefix, "bin", "amq")
		if got := ClassifyInstall(binPath, prefix); got != InstallDirect {
			t.Fatalf("ClassifyInstall(%q, %q)=%v want %v for a regular-file path", binPath, prefix, got, InstallDirect)
		}
	}
}

func TestDefaultCachePathUsesAMQCacheDirPrecedence(t *testing.T) {
	override := t.TempDir()
	t.Setenv(EnvCacheDir, override)
	oldUserCacheDir := userCacheDirForUpdate
	userCacheDirForUpdate = func() (string, error) {
		t.Fatal("DefaultCachePath consulted the platform cache despite AMQ_CACHE_DIR")
		return "", nil
	}
	t.Cleanup(func() { userCacheDirForUpdate = oldUserCacheDir })

	got, err := DefaultCachePath()
	if err != nil {
		t.Fatalf("DefaultCachePath: %v", err)
	}
	want := filepath.Join(override, "amq", "update.json")
	if got != want {
		t.Fatalf("DefaultCachePath()=%q want %q", got, want)
	}
}

func TestDefaultCachePathRefusesInvalidAMQCacheDir(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		t.Setenv(EnvCacheDir, "")
		if _, err := DefaultCachePath(); err == nil || !strings.Contains(err.Error(), "unset AMQ_CACHE_DIR or set it to an absolute cache directory") {
			t.Fatalf("DefaultCachePath error = %v, want actionable empty-value refusal", err)
		}
	})
	t.Run("relative", func(t *testing.T) {
		t.Setenv(EnvCacheDir, "relative-cache")
		if _, err := DefaultCachePath(); err == nil || !strings.Contains(err.Error(), "unset AMQ_CACHE_DIR or set it to an absolute cache directory") {
			t.Fatalf("DefaultCachePath error = %v, want actionable relative-value refusal", err)
		}
	})
	t.Run("whitespace", func(t *testing.T) {
		t.Setenv(EnvCacheDir, " ")
		if _, err := DefaultCachePath(); err == nil || !strings.Contains(err.Error(), "unset AMQ_CACHE_DIR or set it to an absolute cache directory") {
			t.Fatalf("DefaultCachePath error = %v, want actionable whitespace-value refusal", err)
		}
	})
}

func TestDefaultCachePathPreservesSpacesInAbsoluteOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "cache with trailing space ")
	t.Setenv(EnvCacheDir, override)
	got, err := DefaultCachePath()
	if err != nil {
		t.Fatalf("DefaultCachePath: %v", err)
	}
	want := filepath.Join(override, "amq", "update.json")
	if got != want {
		t.Fatalf("DefaultCachePath()=%q want %q", got, want)
	}
}

func TestSaveCacheWritesToIsolatedDefaultPath(t *testing.T) {
	override := t.TempDir()
	t.Setenv(EnvCacheDir, override)
	path, err := DefaultCachePath()
	if err != nil {
		t.Fatalf("DefaultCachePath: %v", err)
	}
	want := &Cache{CheckedAt: time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC), LatestVersion: "v0.1.0"}
	if err := SaveCache(path, want); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	got, err := LoadCache(path)
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if got == nil || !got.CheckedAt.Equal(want.CheckedAt) || got.LatestVersion != want.LatestVersion {
		t.Fatalf("cache = %#v want %#v", got, want)
	}
}

func TestDownloadReleaseAssetRejectsUnsafeAssetBeforeNetwork(t *testing.T) {
	if err := DownloadReleaseAsset(context.Background(), nil, "v0.1.0", "../victim", filepath.Join(t.TempDir(), "asset")); err == nil {
		t.Fatal("DownloadReleaseAsset accepted a traversal asset name")
	}
	if err := DownloadReleaseAsset(context.Background(), nil, "v0.1.0/escape", "amq_0.1.0_darwin_arm64.tar.gz", filepath.Join(t.TempDir(), "asset")); err == nil {
		t.Fatal("DownloadReleaseAsset accepted a traversal release tag")
	}
}
