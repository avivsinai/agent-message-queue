package cli

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/update"
)

var (
	executablePathForUpgrade        = update.ExecutablePath
	fetchLatestTagForUpgrade        = update.FetchLatestTag
	detectHomebrewPrefixForUpgrade  = detectHomebrewPrefix
	lookPathForHomebrewUpgrade      = exec.LookPath
	runBrewPrefixForHomebrewUpgrade = runBrewPrefix
	homebrewBrewExistsForUpgrade    = homebrewBrewExists
	classifyInstallForUpgrade       = update.ClassifyInstall
	runDelegateForUpgrade           = runDelegateCommand
	companionBinariesForUpgrade     = update.CompanionBinaries
	homeDirForUpgrade               = os.UserHomeDir
	downloadReleaseAssetForUpgrade  = update.DownloadReleaseAsset
	fetchChecksumsForUpgrade        = update.FetchChecksums
	extractBinaryForUpgrade         = extractBinaryFromArchive
	replaceBinaryForUpgrade         = update.ReplaceBinary
	updateVerifySHA256ForUpgrade    = update.VerifySHA256
	saveUpgradeCacheForUpgrade      = saveUpgradeCache
)

// saveUpgradeCache writes the latest-known-version cache for the amq binary
// (not companions). It is best-effort: a cache write failure does not fail
// the upgrade.
func saveUpgradeCache(latest string) {
	cachePath, err := update.DefaultCachePath()
	if err != nil {
		return
	}
	_ = update.SaveCache(cachePath, &update.Cache{
		CheckedAt:     time.Now().UTC(),
		LatestVersion: latest,
	})
}

const homebrewDetectionTimeout = 2 * time.Second

var wellKnownHomebrewPrefixes = []string{
	"/opt/homebrew",
	"/usr/local",
	"/home/linuxbrew/.linuxbrew",
}

func runUpgrade(args []string, currentVersion string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	allFlag := fs.Bool("all", false, "Also upgrade present companion binaries (amq-keepalive, amq-bridge, amq-acp)")
	yesFlag := fs.Bool("y", false, "Run the package-manager delegate non-interactively (Homebrew/Scoop installs)")
	usage := usageWithFlags(
		fs,
		"amq upgrade [--all] [-y]",
		"Downloads and installs the latest amq release from GitHub.",
		"When amq is installed via Homebrew or Scoop, amq upgrade delegates to the package manager instead of overwriting it; pass -y to run the delegate.",
		"--all upgrades companion binaries present next to a direct-install amq or in ~/.local/bin; a running amq-keepalive is never killed.",
		"Retire live wakes started by the previous binary before its install directory is removed.",
		"If wake check reports binary_dir_gone, run amq doctor --ops --fix-wake-locks.",
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	path, resolved, err := executablePathForUpgrade()
	if err != nil {
		return err
	}

	homebrewPrefix := detectHomebrewPrefixForUpgrade()
	// Classify from BOTH the resolved path (authoritative: a symlink into the
	// Cellar lands there) and the pre-resolution path (a user-made
	// ~/bin/amq -> /opt/homebrew/bin/amq link is Homebrew-owned by its link
	// path even before resolution). Either matching a package manager wins.
	kind := classifyInstallForUpgrade(resolved, homebrewPrefix)
	if kind == update.InstallDirect {
		kind = classifyInstallForUpgrade(path, homebrewPrefix)
	}

	if kind.IsPackageManaged() {
		return runManagedUpgrade(kind, *yesFlag, *allFlag)
	}

	destPath, err := selectUpgradeDestination(path, resolved, upgradeDestinationWritable)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := &http.Client{Timeout: 30 * time.Second}

	latestTag, err := fetchLatestTagForUpgrade(ctx, client)
	if err != nil {
		return err
	}
	latest := update.NormalizeVersion(latestTag)
	if latest == "" {
		return fmt.Errorf("invalid latest version: %q", latestTag)
	}

	if cmp, ok := update.CompareVersions(currentVersion, latest); ok && cmp >= 0 {
		if err := writeStdoutLine(fmt.Sprintf("amq is already up to date (%s)", latest)); err != nil {
			return err
		}
		if *allFlag {
			if err := upgradeCompanions(ctx, client, latestTag, latest, destPath); err != nil {
				return err
			}
		}
		return reportStaleWakesAfterUpgrade()
	}

	if err := writeStdoutLine("Upgrading to", latest, "..."); err != nil {
		return err
	}

	if err := runDirectUpgrade(ctx, client, update.BinaryName, latestTag, latest, destPath); err != nil {
		return err
	}
	saveUpgradeCacheForUpgrade(latest)
	if *allFlag {
		if err := upgradeCompanions(ctx, client, latestTag, latest, destPath); err != nil {
			return err
		}
	}
	return reportStaleWakesAfterUpgrade()
}

// runManagedUpgrade handles Homebrew/Scoop installs by printing the delegate
// command and, with -y, running it. Companions are never package-managed, so
// --all under a managed install is a no-op with an explanatory line.
func runManagedUpgrade(kind update.InstallKind, yes, all bool) error {
	argvs, remedy := delegateUpgradeCommand(kind)
	if all {
		if err := writeStdoutLine("--all: companions are direct-installed; upgrade them with 'amq upgrade --all' from a direct amq install."); err != nil {
			return err
		}
	}
	if yes {
		for _, argv := range argvs {
			if err := writeStdoutLine("Running:", strings.Join(argv, " ")); err != nil {
				return err
			}
			if err := runDelegateForUpgrade(argv); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("%s", remedy)
}

// delegateUpgradeCommand returns the argv slices to run (no shell, so a future
// dynamic value cannot inject) and the non-interactive remedy string for a
// package-managed install kind. Homebrew runs `brew update` then
// `brew upgrade amq`; Scoop runs `scoop update amq`.
func delegateUpgradeCommand(kind update.InstallKind) ([][]string, string) {
	switch kind {
	case update.InstallHomebrew:
		return [][]string{{"brew", "update"}, {"brew", "upgrade", "amq"}},
			"amq is installed via Homebrew; run brew update && brew upgrade amq (or amq upgrade -y)"
	case update.InstallScoop:
		return [][]string{{"scoop", "update", "amq"}},
			"amq is installed via Scoop; run scoop update amq (or amq upgrade -y)"
	default:
		return nil, ""
	}
}

// runDelegateCommand runs a delegate argv (brew/scoop) directly — no shell —
// with the caller's stdin/stdout/stderr and returns its exit status as an
// error.
func runDelegateCommand(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return WithExitCode(exitErr.ExitCode(), fmt.Errorf("delegate command failed: %s", strings.Join(argv, " ")))
		}
		return fmt.Errorf("delegate command failed: %w", err)
	}
	return nil
}

// extractBinaryFromArchive extracts the named binary from a release archive
// into destDir. The zip flag selects the Windows zip reader over tar.gz.
func extractBinaryFromArchive(binaryName, archivePath, destDir string, zip bool) (string, error) {
	if zip {
		return update.ExtractBinaryFromZipNamed(binaryName, archivePath, destDir)
	}
	return update.ExtractBinaryFromTarGzNamed(binaryName, archivePath, destDir)
}

// runDirectUpgrade downloads, verifies, extracts, and atomically replaces a
// single binary at destPath. It is shared by the amq self-upgrade and the
// per-companion --all loop.
func runDirectUpgrade(ctx context.Context, client *http.Client, binaryName, latestTag, latest, destPath string) error {
	assetName, err := update.AssetNameFor(binaryName, latestTag, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp("", "amq-upgrade-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	archivePath := filepath.Join(tmpDir, assetName)
	if err := downloadReleaseAssetForUpgrade(ctx, client, latestTag, assetName, archivePath); err != nil {
		return err
	}

	checksums, err := fetchChecksumsForUpgrade(ctx, client, latestTag)
	if err != nil {
		return err
	}
	checksum, ok := checksums[assetName]
	if !ok {
		return fmt.Errorf("checksum entry not found for %s", assetName)
	}
	if err := updateVerifySHA256ForUpgrade(archivePath, checksum); err != nil {
		return err
	}

	var binaryPath string
	if runtime.GOOS == "windows" {
		binaryPath, err = extractBinaryForUpgrade(binaryName, archivePath, tmpDir, true)
	} else {
		binaryPath, err = extractBinaryForUpgrade(binaryName, archivePath, tmpDir, false)
	}
	if err != nil {
		return err
	}

	scheduled, err := replaceBinaryForUpgrade(binaryPath, destPath)
	if err != nil {
		return err
	}
	if scheduled {
		return writeStdoutLine(binaryName + " upgrade scheduled; it will complete after this process exits.")
	}
	return writeStdoutLine(binaryName + " upgrade complete.")
}

// upgradeCompanions upgrades each companion binary (amq-keepalive,
// amq-bridge, amq-acp) present next to the direct-install amq (destDir) or in
// ~/.local/bin. Missing companions are skipped with a line. A running
// amq-keepalive is never killed: the atomic rename swaps the path while the
// running process keeps the old inode, and the remedy to restart the
// supervisor is printed afterward.
func upgradeCompanions(ctx context.Context, client *http.Client, latestTag, latest, amqDest string) error {
	searchDirs := companionSearchDirs(amqDest)
	for _, name := range companionBinariesForUpgrade {
		dest, found := findCompanion(name, searchDirs)
		if !found {
			if err := writeStdoutLine(name + " not found; skipping."); err != nil {
				return err
			}
			continue
		}
		if err := runDirectUpgrade(ctx, client, name, latestTag, latest, dest); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if name == "amq-keepalive" {
			if err := writeStdoutLine("amq-keepalive: a running supervisor picks up the new image on its next supervise pass (self-upgrade); if it was started with --no-self-upgrade, restart it with 'amq-keepalive install-launchd'."); err != nil {
				return err
			}
		}
	}
	return nil
}

// companionSearchDirs returns the directories to look for companions in, in
// priority order: the directory holding the direct-install amq, then
// ~/.local/bin (the documented companion home).
func companionSearchDirs(amqDest string) []string {
	dirs := make([]string, 0, 2)
	if amqDest != "" {
		if dir := filepath.Dir(amqDest); dir != "" {
			dirs = append(dirs, dir)
		}
	}
	if home, err := homeDirForUpgrade(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	return dirs
}

// findCompanion returns the first existing companion path across the search
// dirs, resolving a symlink to its real target so ReplaceBinary writes the
// backing file (preserving the operator's link structure) rather than
// replacing the link itself. A regular file is returned as-is. Returns "",
// false if absent everywhere.
func findCompanion(name string, dirs []string) (string, bool) {
	for _, dir := range dirs {
		candidate := filepath.Join(dir, name)
		if runtime.GOOS == "windows" {
			candidate += ".exe"
		}
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		// Resolve only an actual symlink to its target; a regular file is
		// returned as-is so its path stays comparable for callers.
		if li, err := os.Lstat(candidate); err == nil && li.Mode()&os.ModeSymlink != 0 {
			if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
				return resolved, true
			}
		}
		return candidate, true
	}
	return "", false
}

// selectUpgradeDestination resolves the write target for a direct install.
// It is only reached after the install classifier has already delegated
// Homebrew/Scoop installs, so it does not re-check for a normal Homebrew
// path. It does keep one guard the classifier cannot express: a severed
// Homebrew binary — a regular file sitting at <prefix>/bin/amq where brew
// expects a symlink — which means someone overwrote the brew-managed path
// and must reinstall rather than self-replace into the corruption.
func selectUpgradeDestination(path, resolved string, writable func(string) error) (string, error) {
	destPath := resolved
	if destPath == "" {
		destPath = path
	}
	if destPath == "" {
		return "", fmt.Errorf("unable to resolve executable path")
	}
	destPath = filepath.Clean(destPath)

	if prefix := detectHomebrewPrefixForUpgrade(); prefix != "" && isSeveredHomebrewBinary(prefix, path) {
		return "", fmt.Errorf("amq is installed via Homebrew; run brew reinstall amq")
	}
	if err := writable(destPath); err != nil {
		return "", fmt.Errorf("cannot write the amq install location %s: %w", destPath, err)
	}
	return destPath, nil
}

func detectHomebrewPrefix() string {
	if prefix := cleanHomebrewPrefix(os.Getenv("HOMEBREW_PREFIX")); prefix != "" {
		return prefix
	}

	if brewPath, err := lookPathForHomebrewUpgrade("brew"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), homebrewDetectionTimeout)
		output, runErr := runBrewPrefixForHomebrewUpgrade(ctx, brewPath)
		cancel()
		if runErr == nil {
			if prefix := cleanHomebrewPrefix(strings.TrimSpace(string(output))); prefix != "" {
				return prefix
			}
		}
	}

	for _, prefix := range wellKnownHomebrewPrefixes {
		if homebrewBrewExistsForUpgrade(filepath.Join(prefix, "bin", "brew")) {
			return filepath.Clean(prefix)
		}
	}
	// User-local Linuxbrew installs under ~/.linuxbrew (not a fixed absolute
	// path, so it is checked separately from the well-known prefix list).
	if home, err := homeDirForUpgrade(); err == nil {
		userLinuxbrew := filepath.Join(home, ".linuxbrew")
		if homebrewBrewExistsForUpgrade(filepath.Join(userLinuxbrew, "bin", "brew")) {
			return userLinuxbrew
		}
	}
	return ""
}

func runBrewPrefix(ctx context.Context, brewPath string) ([]byte, error) {
	return exec.CommandContext(ctx, brewPath, "--prefix").Output()
}

func homebrewBrewExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func cleanHomebrewPrefix(prefix string) string {
	if prefix == "" || !filepath.IsAbs(prefix) {
		return ""
	}
	return filepath.Clean(prefix)
}

func homebrewOwnsUpgradePath(prefix, path string) bool {
	if path == "" {
		return false
	}
	path = filepath.Clean(path)
	return pathWithinDirectory(path, filepath.Join(prefix, "bin")) ||
		pathWithinDirectory(path, filepath.Join(prefix, "Cellar"))
}

func pathWithinDirectory(path, directory string) bool {
	rel, err := filepath.Rel(filepath.Clean(directory), path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isSeveredHomebrewBinary(prefix, path string) bool {
	if path == "" || !pathWithinDirectory(filepath.Clean(path), filepath.Join(prefix, "bin")) {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}
