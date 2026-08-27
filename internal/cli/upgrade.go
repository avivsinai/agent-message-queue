package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/selfupgrade"
	"github.com/avivsinai/agent-message-queue/internal/update"
)

var (
	executablePathForUpgrade              = update.ExecutablePath
	fetchLatestTagForUpgrade              = update.FetchLatestTag
	detectHomebrewPrefixForUpgrade        = detectHomebrewPrefix
	detectHomebrewInstallationsForUpgrade = detectHomebrewInstallations
	lookPathForHomebrewUpgrade            = exec.LookPath
	lookPathForDelegateForUpgrade         = exec.LookPath
	runBrewPrefixForHomebrewUpgrade       = runBrewPrefix
	homebrewBrewExistsForUpgrade          = homebrewBrewExists
	classifyInstallForUpgrade             = update.ClassifyInstall
	runDelegateForUpgrade                 = runDelegateCommand
	companionBinariesForUpgrade           = update.CompanionBinaries
	homeDirForUpgrade                     = os.UserHomeDir
	downloadReleaseAssetForUpgrade        = update.DownloadReleaseAsset
	fetchChecksumsForUpgrade              = update.FetchChecksums
	extractBinaryForUpgrade               = extractBinaryFromArchive
	replaceBinaryForUpgrade               = update.ReplaceBinary
	updateVerifySHA256ForUpgrade          = update.VerifySHA256
	saveUpgradeCacheForUpgrade            = saveUpgradeCache
)

type homebrewInstallation struct {
	prefix     string
	executable string
}

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
	"/opt/linuxbrew/.linuxbrew",
}

func runUpgrade(args []string, currentVersion string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	allFlag := fs.Bool("all", false, "Also upgrade present companion binaries (amq-keepalive, amq-bridge, amq-acp)")
	yesFlag := fs.Bool("y", false, "Run the package-manager delegate without an AMQ confirmation; the manager may still prompt")
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

	homebrewInstallations := homebrewInstallationsForUpgrade()
	homebrewPrefixes := homebrewPrefixesFromInstallations(homebrewInstallations)
	kind, managerPath := classifyPackageInstall(path, resolved, homebrewPrefixes, homebrewInstallations)
	if kind.IsPackageManaged() {
		if managerPath == "" {
			return fmt.Errorf("%s executable not found; refusing package-manager delegation", packageManagerName(kind))
		}
		return runManagedUpgradeWithManager(kind, managerPath, *yesFlag, *allFlag)
	}

	destPath, err := selectUpgradeDestinationWithRoots(
		path,
		resolved,
		upgradeDestinationWritable,
		homebrewPrefixes,
		update.ScoopAppsDir(),
	)
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
			if err := upgradeCompanionsWithProtection(
				ctx,
				client,
				latestTag,
				latest,
				destPath,
				homebrewPrefixes,
				update.ScoopAppsDir(),
			); err != nil {
				return err
			}
		}
		return reportStaleWakesAfterUpgrade()
	}

	if err := writeStdoutLine("Upgrading to", latest, "..."); err != nil {
		return err
	}

	if err := validateUpgradeCachePath(); err != nil {
		return err
	}
	result, err := runDirectUpgradeWithProtection(
		ctx,
		client,
		update.BinaryName,
		latestTag,
		latest,
		destPath,
		homebrewPrefixes,
		update.ScoopAppsDir(),
	)
	if err != nil {
		return err
	}
	if result.scheduled {
		if err := writeStdoutLine("replacement scheduled; version cache updates on next run"); err != nil {
			return err
		}
	} else {
		saveUpgradeCacheForUpgrade(latest)
	}
	if *allFlag {
		if err := upgradeCompanionsWithProtection(
			ctx,
			client,
			latestTag,
			latest,
			destPath,
			homebrewPrefixes,
			update.ScoopAppsDir(),
		); err != nil {
			return err
		}
	}
	return reportStaleWakesAfterUpgrade()
}

func runManagedUpgradeWithManager(kind update.InstallKind, managerPath string, yes, all bool) error {
	argvs, remedy := delegateUpgradeCommandWithManager(kind, managerPath)
	if all {
		if err := writeStdoutLine("--all: companions are direct-installed; upgrade them with 'amq upgrade --all' from a direct amq install."); err != nil {
			return err
		}
	}
	if len(argvs) == 0 {
		return errors.New("unsupported package manager install")
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

func packageManagerName(kind update.InstallKind) string {
	switch kind {
	case update.InstallHomebrew:
		return "Homebrew brew"
	case update.InstallScoop:
		return "Scoop scoop"
	default:
		return "package manager"
	}
}

func delegateUpgradeCommandWithManager(kind update.InstallKind, managerPath string) ([][]string, string) {
	switch kind {
	case update.InstallHomebrew:
		if managerPath == "" {
			managerPath = "brew"
		}
		return [][]string{{managerPath, "update"}, {managerPath, "upgrade", "amq"}},
			"amq is installed via Homebrew; run brew update && brew upgrade amq (or amq upgrade -y)"
	case update.InstallScoop:
		if managerPath == "" {
			managerPath = "scoop"
		}
		return [][]string{{managerPath, "update", "amq"}},
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

type directUpgradeResult struct {
	scheduled bool
}

func validateUpgradeCachePath() error {
	if _, err := update.DefaultCachePath(); err != nil {
		return fmt.Errorf("cannot resolve update cache path: %w", err)
	}
	return nil
}

// runDirectUpgradeWithProtection performs the final package-manager guard
// immediately before replacement. Classification happens earlier, but this
// independent check protects against a missed Homebrew prefix or a path race.
func runDirectUpgradeWithProtection(
	ctx context.Context,
	client *http.Client,
	binaryName, latestTag, latest, destPath string,
	homebrewPrefixes []string,
	scoopApps string,
) (directUpgradeResult, error) {
	assetName, err := update.AssetNameFor(binaryName, latestTag, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return directUpgradeResult{}, err
	}

	tmpDir, err := os.MkdirTemp("", "amq-upgrade-")
	if err != nil {
		return directUpgradeResult{}, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	archivePath := filepath.Join(tmpDir, assetName)
	if err := downloadReleaseAssetForUpgrade(ctx, client, latestTag, assetName, archivePath); err != nil {
		return directUpgradeResult{}, err
	}

	checksums, err := fetchChecksumsForUpgrade(ctx, client, latestTag)
	if err != nil {
		return directUpgradeResult{}, err
	}
	checksum, ok := checksums[assetName]
	if !ok {
		return directUpgradeResult{}, fmt.Errorf("checksum entry not found for %s", assetName)
	}
	if err := updateVerifySHA256ForUpgrade(archivePath, checksum); err != nil {
		return directUpgradeResult{}, err
	}

	var binaryPath string
	if runtime.GOOS == "windows" {
		binaryPath, err = extractBinaryForUpgrade(binaryName, archivePath, tmpDir, true)
	} else {
		binaryPath, err = extractBinaryForUpgrade(binaryName, archivePath, tmpDir, false)
	}
	if err != nil {
		return directUpgradeResult{}, err
	}

	if err := refusePackageManagedDestination(destPath, homebrewPrefixes, scoopApps); err != nil {
		return directUpgradeResult{}, err
	}
	scheduled, err := replaceBinaryForUpgrade(binaryPath, destPath)
	if err != nil {
		return directUpgradeResult{}, err
	}
	if scheduled {
		if err := writeStdoutLine(binaryName + " upgrade scheduled; it will complete after this process exits."); err != nil {
			return directUpgradeResult{}, err
		}
		return directUpgradeResult{scheduled: true}, nil
	}
	if err := writeStdoutLine(binaryName + " upgrade complete."); err != nil {
		return directUpgradeResult{}, err
	}
	return directUpgradeResult{}, nil
}

func upgradeCompanionsWithProtection(
	ctx context.Context,
	client *http.Client,
	latestTag, latest, amqDest string,
	homebrewPrefixes []string,
	scoopApps string,
) error {
	searchDirs := companionSearchDirs(amqDest)
	for _, name := range companionBinariesForUpgrade {
		matches, err := findCompanionMatches(name, searchDirs, amqDest, homebrewPrefixes, scoopApps)
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			if err := writeStdoutLine(name + " not found; skipping."); err != nil {
				return err
			}
			continue
		}
		if len(matches) > 1 {
			return fmt.Errorf("%s found at multiple distinct targets: %s", name, strings.Join(matches, ", "))
		}
		result, err := runDirectUpgradeWithProtection(
			ctx,
			client,
			name,
			latestTag,
			latest,
			matches[0],
			homebrewPrefixes,
			scoopApps,
		)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if name == "amq-keepalive" {
			if result.scheduled {
				if err := writeStdoutLine("amq-keepalive: replacement scheduled; version cache updates on next run"); err != nil {
					return err
				}
			}
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

func findCompanionMatches(name string, dirs []string, amqDest string, homebrewPrefixes []string, scoopApps string) ([]string, error) {
	var amqInfo os.FileInfo
	var err error
	if amqDest != "" {
		amqInfo, err = os.Stat(amqDest)
		if err != nil {
			return nil, fmt.Errorf("cannot inspect amq destination %s: %w", amqDest, err)
		}
	}
	matches := make([]string, 0, len(dirs))
	seen := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		candidate := filepath.Join(dir, name)
		if runtime.GOOS == "windows" {
			candidate += ".exe"
		}
		linkInfo, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("cannot inspect companion %s: %w", candidate, err)
		}

		var target string
		switch {
		case linkInfo.Mode().IsRegular():
			target, err = filepath.EvalSymlinks(candidate)
			if err != nil {
				return nil, fmt.Errorf("cannot resolve companion %s: %w", candidate, err)
			}
		case linkInfo.Mode()&os.ModeSymlink != 0:
			target, err = filepath.EvalSymlinks(candidate)
			if err != nil {
				return nil, fmt.Errorf("cannot resolve companion symlink %s: %w", candidate, err)
			}
		default:
			return nil, fmt.Errorf("refusing companion %s: object is not a regular file or symlink", candidate)
		}

		target = filepath.Clean(target)
		targetInfo, statErr := os.Stat(target)
		if statErr != nil {
			return nil, fmt.Errorf("cannot inspect companion target %s: %w", target, statErr)
		}
		if !targetInfo.Mode().IsRegular() {
			return nil, fmt.Errorf("refusing companion %s: target %s is not a regular file", candidate, target)
		}
		if amqInfo != nil {
			if os.SameFile(targetInfo, amqInfo) {
				return nil, fmt.Errorf("refusing companion %s: target %s is the amq executable", candidate, target)
			}
		}
		if isUnderProtectedRoot(target, homebrewPrefixes, scoopApps) {
			return nil, fmt.Errorf("refusing companion %s: target %s is package-managed", candidate, target)
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		matches = append(matches, target)
	}
	return matches, nil
}

func classifyPackageInstall(path, resolved string, homebrewPrefixes []string, installations []homebrewInstallation) (update.InstallKind, string) {
	for _, candidate := range []string{resolved, path} {
		if candidate == "" {
			continue
		}
		for _, prefix := range homebrewPrefixes {
			if classifyInstallForUpgrade(candidate, prefix) == update.InstallHomebrew {
				return update.InstallHomebrew, matchingHomebrewManager(candidate, path, installations)
			}
		}
		if classifyInstallForUpgrade(candidate, "") == update.InstallScoop {
			return update.InstallScoop, scoopManagerExecutable()
		}
	}
	return update.InstallDirect, ""
}

func matchingHomebrewManager(candidate, rawPath string, installations []homebrewInstallation) string {
	for _, installation := range installations {
		prefix := canonicalHomebrewPrefix(installation.prefix)
		cellar := filepath.Join(prefix, "Cellar")
		for _, path := range []string{candidate, rawPath} {
			if path == "" {
				continue
			}
			canonical := canonicalExistingPath(path)
			if pathWithinDirectory(canonical, cellar) ||
				(pathWithinDirectory(filepath.Clean(path), filepath.Join(prefix, "bin")) && isSymlinkToHomebrewCellar(path, cellar)) {
				if installation.executable != "" {
					return installation.executable
				}
				return filepath.Join(prefix, "bin", "brew")
			}
		}
	}
	return ""
}

func scoopManagerExecutable() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	path, err := lookPathForDelegateForUpgrade("scoop")
	if err != nil {
		return ""
	}
	return path
}

func homebrewPrefixesFromInstallations(installations []homebrewInstallation) []string {
	prefixes := make([]string, 0, len(installations))
	seen := make(map[string]struct{}, len(installations))
	for _, installation := range installations {
		prefix := canonicalHomebrewPrefix(installation.prefix)
		if prefix == "" {
			continue
		}
		if _, ok := seen[prefix]; ok {
			continue
		}
		seen[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}

func homebrewInstallationsForUpgrade() []homebrewInstallation {
	installations := detectHomebrewInstallationsForUpgrade()
	legacyPrefix := cleanHomebrewPrefix(detectHomebrewPrefixForUpgrade())
	if legacyPrefix == "" {
		return installations
	}
	canonical := canonicalHomebrewPrefix(legacyPrefix)
	for _, installation := range installations {
		if canonicalHomebrewPrefix(installation.prefix) == canonical {
			return installations
		}
	}
	return append(installations, homebrewInstallation{
		prefix:     canonical,
		executable: filepath.Join(legacyPrefix, "bin", "brew"),
	})
}

func detectHomebrewInstallations() []homebrewInstallation {
	installations := make([]homebrewInstallation, 0, len(wellKnownHomebrewPrefixes)+3)
	seen := make(map[string]struct{})
	add := func(prefix, executable string) {
		prefix = cleanHomebrewPrefix(prefix)
		if prefix == "" {
			return
		}
		key := canonicalHomebrewPrefix(prefix)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		if executable == "" {
			executable = filepath.Join(prefix, "bin", "brew")
		}
		installations = append(installations, homebrewInstallation{
			prefix:     key,
			executable: executable,
		})
	}

	if brewPath, err := lookPathForHomebrewUpgrade("brew"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), homebrewDetectionTimeout)
		output, runErr := runBrewPrefixForHomebrewUpgrade(ctx, brewPath)
		cancel()
		if runErr == nil {
			add(strings.TrimSpace(string(output)), brewPath)
		}
	}
	if prefix := cleanHomebrewPrefix(os.Getenv("HOMEBREW_PREFIX")); prefix != "" {
		add(prefix, filepath.Join(prefix, "bin", "brew"))
	}
	for _, prefix := range wellKnownHomebrewPrefixes {
		add(prefix, filepath.Join(prefix, "bin", "brew"))
	}
	if home, err := homeDirForUpgrade(); err == nil {
		add(filepath.Join(home, ".linuxbrew"), filepath.Join(home, ".linuxbrew", "bin", "brew"))
	}
	return installations
}

func canonicalHomebrewPrefix(prefix string) string {
	return canonicalExistingPath(cleanHomebrewPrefix(prefix))
}

func canonicalExistingPath(path string) string {
	path = filepath.Clean(path)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	current := path
	suffix := make([]string, 0, 4)
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			resolved = filepath.Clean(resolved)
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func isSymlinkToHomebrewCellar(path, cellar string) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && pathWithinDirectory(filepath.Clean(resolved), cellar)
}

// selectUpgradeDestination resolves the write target for a direct install and
// performs the last package-manager safety check before any download starts.
func selectUpgradeDestination(path, resolved string, writable func(string) error) (string, error) {
	installations := homebrewInstallationsForUpgrade()
	return selectUpgradeDestinationWithRoots(
		path,
		resolved,
		writable,
		homebrewPrefixesFromInstallations(installations),
		update.ScoopAppsDir(),
	)
}

func selectUpgradeDestinationWithRoots(path, resolved string, writable func(string) error, homebrewPrefixes []string, scoopApps string) (string, error) {
	destPath := resolved
	if destPath == "" {
		destPath = path
	}
	if destPath == "" {
		return "", fmt.Errorf("unable to resolve executable path")
	}
	destPath = filepath.Clean(destPath)

	for _, candidate := range []string{path, destPath} {
		if candidate == "" {
			continue
		}
		if err := refusePackageManagedDestination(candidate, homebrewPrefixes, scoopApps); err != nil {
			return "", err
		}
	}
	if err := writable(destPath); err != nil {
		return "", fmt.Errorf("cannot write the amq install location %s: %w", destPath, err)
	}
	return destPath, nil
}

func refusePackageManagedDestination(path string, homebrewPrefixes []string, scoopApps string) error {
	path = filepath.Clean(path)
	canonical := canonicalExistingPath(path)
	for _, prefix := range homebrewPrefixes {
		prefix = canonicalHomebrewPrefix(prefix)
		if prefix == "" {
			continue
		}
		cellar := filepath.Join(prefix, "Cellar")
		// This is intentionally independent from install classification. A
		// missed prefix or a path race must not turn a Cellar replacement into
		// a direct install.
		if pathWithinDirectory(canonical, cellar) {
			return fmt.Errorf("amq is installed via Homebrew; run brew update && brew upgrade amq")
		}
		if pathWithinDirectory(canonical, filepath.Join(prefix, "bin")) {
			info, err := os.Lstat(path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return fmt.Errorf("cannot inspect possible Homebrew destination %s: %w", path, err)
			}
			switch {
			case info.Mode().IsRegular():
				return fmt.Errorf("amq is in a Homebrew prefix but is not a managed symlink; run brew reinstall amq or brew update && brew upgrade amq")
			case info.Mode()&os.ModeSymlink != 0:
				resolved, resolveErr := filepath.EvalSymlinks(path)
				if resolveErr != nil {
					return fmt.Errorf("amq is in a Homebrew prefix but its symlink cannot be resolved; run brew reinstall amq or brew update && brew upgrade amq: %w", resolveErr)
				}
				if pathWithinDirectory(filepath.Clean(resolved), cellar) {
					return fmt.Errorf("amq is installed via Homebrew; run brew update && brew upgrade amq")
				}
			default:
				return fmt.Errorf("refusing possible Homebrew destination %s; run brew reinstall amq or brew update && brew upgrade amq", path)
			}
		}
	}
	if scoopApps != "" && pathWithinDirectory(canonical, canonicalExistingPath(scoopApps)) {
		return fmt.Errorf("amq is installed via Scoop; run scoop update amq")
	}
	return nil
}

func isUnderProtectedRoot(path string, homebrewPrefixes []string, scoopApps string) bool {
	canonical := canonicalExistingPath(path)
	for _, prefix := range homebrewPrefixes {
		prefix = canonicalHomebrewPrefix(prefix)
		if prefix != "" && pathWithinDirectory(canonical, filepath.Join(prefix, "Cellar")) {
			return true
		}
	}
	return scoopApps != "" && pathWithinDirectory(canonical, canonicalExistingPath(scoopApps))
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
	return selfupgrade.RunBoundedProbe(ctx, brewPath, []string{"--prefix"}, selfupgrade.BoundedProbeOptions{})
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
	path = canonicalExistingPath(path)
	prefix = canonicalHomebrewPrefix(prefix)
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
