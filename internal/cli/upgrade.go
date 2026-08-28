package cli

import (
	"context"
	debugbuildinfo "debug/buildinfo"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	runtimedebug "runtime/debug"
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
	homebrewCellarExistsForUpgrade        = homebrewCellarExists
	classifyInstallForUpgrade             = update.ClassifyInstall
	runDelegateForUpgrade                 = runDelegateCommand
	companionBinariesForUpgrade           = update.CompanionBinaries
	homeDirForUpgrade                     = os.UserHomeDir
	downloadReleaseAssetForUpgrade        = update.DownloadReleaseAsset
	fetchChecksumsForUpgrade              = update.FetchChecksums
	extractBinaryForUpgrade               = extractBinaryFromArchive
	readBuildInfoForUpgrade               = debugbuildinfo.ReadFile
	replaceBinaryForUpgrade               = update.ReplaceBinary
	updateVerifySHA256ForUpgrade          = update.VerifySHA256
	saveUpgradeCacheForUpgrade            = saveUpgradeCache
)

type homebrewInstallation struct {
	prefix     string
	executable string
}

type derivedHomebrewPrefix struct {
	prefix     string
	fromCellar bool
}

type companionTarget struct {
	name   string
	link   string
	target string
	info   os.FileInfo
}

type preparedUpgrade struct {
	binaryPath    string
	destPath      string
	plannedTarget *companionTarget
}

type preparedCompanionUpgrade struct {
	name string
	item preparedUpgrade
}

// saveUpgradeCache writes the latest-known-version cache for the amq binary
// (not companions). It is best-effort: a cache write failure does not fail
// the upgrade.
func saveUpgradeCache(latest string) {
	if update.IsUnpublishedTestVersion(latest) {
		return
	}
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
		"--all upgrades companion binaries in the raw or resolved executable directory of a direct-install amq, or in ~/.local/bin; a running amq-keepalive is never killed.",
		"Retire live wakes started by the previous binary before its install directory is removed.",
		"If wake check reports binary_dir_gone, run amq doctor --ops --fix-wake-locks.",
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := skipUnavailableCompanionsOnWindows(allFlag, runtime.GOOS); err != nil {
		return err
	}

	path, resolved, err := executablePathForUpgrade()
	if err != nil {
		return err
	}
	scoopRoots := update.ScoopInstallRoots()

	homebrewInstallations := homebrewInstallationsForUpgrade()
	homebrewInstallations, err = addDerivedHomebrewInstallations(path, resolved, homebrewInstallations)
	if err != nil {
		return err
	}
	homebrewPrefixes := homebrewPrefixesFromInstallations(homebrewInstallations)
	kind, managerPath := classifyPackageInstall(path, resolved, homebrewPrefixes, homebrewInstallations)
	if kind.IsPackageManaged() {
		if kind == update.InstallScoopUnknown {
			return refuseUnknownScoopInstall(path, resolved)
		}
		if managerPath == "" {
			if kind == update.InstallHomebrew {
				return refuseHomebrewWithoutMatchingManager(path, resolved, homebrewInstallations)
			}
			return fmt.Errorf("%s executable not found; refusing package-manager delegation; install the package manager or repoint amq to a direct install", packageManagerName(kind))
		}
		return runManagedUpgradeWithManager(kind, managerPath, *yesFlag, *allFlag)
	}

	destPath, err := selectUpgradeDestinationWithScoopRoots(
		path,
		resolved,
		upgradeDestinationWritable,
		homebrewPrefixes,
		scoopRoots,
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
		if err := validateUpgradeCachePath(); err != nil {
			return err
		}
		saveUpgradeCacheForUpgrade(latest)
		if *allFlag {
			if err := upgradeCompanionsWithScoopRoots(
				ctx,
				client,
				latestTag,
				latest,
				path,
				destPath,
				homebrewPrefixes,
				scoopRoots,
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
	result, err := runDirectUpgradeWithScoopRoots(
		ctx,
		client,
		update.BinaryName,
		latestTag,
		latest,
		destPath,
		homebrewPrefixes,
		scoopRoots,
	)
	if err != nil {
		return err
	}
	if result.scheduled {
		if err := writeStdoutLine("replacement scheduled; version cache is refreshed on the next successful check (best-effort)"); err != nil {
			return err
		}
	} else {
		saveUpgradeCacheForUpgrade(latest)
	}
	if *allFlag {
		if err := upgradeCompanionsWithScoopRoots(
			ctx,
			client,
			latestTag,
			latest,
			path,
			destPath,
			homebrewPrefixes,
			scoopRoots,
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
	case update.InstallScoop, update.InstallScoopGlobal:
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
		commands := [][]string{{managerPath, "update"}, {managerPath, "upgrade", "amq"}}
		return commands,
			fmt.Sprintf("amq is installed via Homebrew; run %s && %s (or amq upgrade -y)", strings.Join(commands[0], " "), strings.Join(commands[1], " "))
	case update.InstallScoop:
		if managerPath == "" {
			managerPath = "scoop"
		}
		commands := [][]string{{managerPath, "update", "amq"}}
		return commands,
			fmt.Sprintf("amq is installed via Scoop (user scope); run %s (or amq upgrade -y)", strings.Join(commands[0], " "))
	case update.InstallScoopGlobal:
		if managerPath == "" {
			managerPath = "scoop"
		}
		commands := [][]string{{managerPath, "update", "-g", "amq"}}
		return commands,
			fmt.Sprintf("amq is installed via Scoop (global scope); run %s (or amq upgrade -y)", strings.Join(commands[0], " "))
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

func prepareDirectUpgradeWithScoopRoots(
	ctx context.Context,
	client *http.Client,
	binaryName, latestTag, destPath string,
	homebrewPrefixes []string,
	scoopRoots []update.ScoopInstallRoot,
	tmpDir string,
) (preparedUpgrade, error) {
	assetName, err := update.AssetNameFor(binaryName, latestTag, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return preparedUpgrade{}, err
	}

	archivePath := filepath.Join(tmpDir, assetName)
	if err := downloadReleaseAssetForUpgrade(ctx, client, latestTag, assetName, archivePath); err != nil {
		return preparedUpgrade{}, err
	}

	checksums, err := fetchChecksumsForUpgrade(ctx, client, latestTag)
	if err != nil {
		return preparedUpgrade{}, err
	}
	checksum, ok := checksums[assetName]
	if !ok {
		return preparedUpgrade{}, fmt.Errorf("checksum entry not found for %s", assetName)
	}
	if err := updateVerifySHA256ForUpgrade(archivePath, checksum); err != nil {
		return preparedUpgrade{}, err
	}

	var binaryPath string
	if runtime.GOOS == "windows" {
		binaryPath, err = extractBinaryForUpgrade(binaryName, archivePath, tmpDir, true)
	} else {
		binaryPath, err = extractBinaryForUpgrade(binaryName, archivePath, tmpDir, false)
	}
	if err != nil {
		return preparedUpgrade{}, err
	}

	// This is a second package-root classification check. Path-derived
	// Homebrew prefixes are added before classification; this check covers a
	// prefix that changes between destination selection and replacement.
	if err := refusePackageManagedDestinationWithScoopRoots(destPath, homebrewPrefixes, scoopRoots); err != nil {
		return preparedUpgrade{}, err
	}
	return preparedUpgrade{binaryPath: binaryPath, destPath: destPath}, nil
}

func replacePreparedUpgrade(binaryName string, prepared preparedUpgrade) (directUpgradeResult, error) {
	scheduled, err := replaceBinaryForUpgrade(prepared.binaryPath, prepared.destPath)
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

// runDirectUpgradeWithProtection prepares one release and then replaces its
// destination after the independent package-manager guard succeeds.
func runDirectUpgradeWithProtection(
	ctx context.Context,
	client *http.Client,
	binaryName, latestTag, latest, destPath string,
	homebrewPrefixes []string,
	scoopApps string,
) (directUpgradeResult, error) {
	return runDirectUpgradeWithScoopRoots(
		ctx,
		client,
		binaryName,
		latestTag,
		latest,
		destPath,
		homebrewPrefixes,
		legacyScoopRoots(scoopApps),
	)
}

func runDirectUpgradeWithScoopRoots(
	ctx context.Context,
	client *http.Client,
	binaryName, latestTag, latest, destPath string,
	homebrewPrefixes []string,
	scoopRoots []update.ScoopInstallRoot,
) (directUpgradeResult, error) {
	tmpDir, err := os.MkdirTemp("", "amq-upgrade-")
	if err != nil {
		return directUpgradeResult{}, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	prepared, err := prepareDirectUpgradeWithScoopRoots(
		ctx,
		client,
		binaryName,
		latestTag,
		destPath,
		homebrewPrefixes,
		scoopRoots,
		tmpDir,
	)
	if err != nil {
		return directUpgradeResult{}, err
	}
	return replacePreparedUpgrade(binaryName, prepared)
}

func upgradeCompanionsWithProtection(
	ctx context.Context,
	client *http.Client,
	latestTag, latest, amqDest string,
	homebrewPrefixes []string,
	scoopApps string,
) error {
	return upgradeCompanionsWithScoopRoots(
		ctx,
		client,
		latestTag,
		latest,
		amqDest,
		amqDest,
		homebrewPrefixes,
		legacyScoopRoots(scoopApps),
	)
}

func upgradeCompanionsWithScoopRoots(
	ctx context.Context,
	client *http.Client,
	latestTag, latest, rawAMQPath, amqDest string,
	homebrewPrefixes []string,
	scoopRoots []update.ScoopInstallRoot,
) error {
	searchDirs, homeErr := companionSearchDirsForUpgrade(rawAMQPath, amqDest)
	if homeErr != nil {
		if err := writeStdoutLine(fmt.Sprintf("warning: cannot resolve user home directory for companion search; skipping ~/.local/bin: %v", homeErr)); err != nil {
			return err
		}
	}
	plan, err := discoverCompanionPlan(companionBinariesForUpgrade, searchDirs, amqDest, homebrewPrefixes, scoopRoots)
	if err != nil {
		return err
	}

	prepared := make([]preparedCompanionUpgrade, 0, len(plan))
	for _, name := range companionBinariesForUpgrade {
		_, ok := plan[name]
		if !ok {
			if err := writeStdoutLine(name + " not found; skipping."); err != nil {
				return err
			}
			continue
		}
	}
	if len(plan) == 0 {
		return nil
	}

	tmpDir, err := os.MkdirTemp("", "amq-upgrade-companions-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	for _, name := range companionBinariesForUpgrade {
		target, ok := plan[name]
		if !ok {
			continue
		}
		item, err := prepareDirectUpgradeWithScoopRoots(
			ctx,
			client,
			name,
			latestTag,
			target.target,
			homebrewPrefixes,
			scoopRoots,
			tmpDir,
		)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		planned := target
		item.plannedTarget = &planned
		prepared = append(prepared, preparedCompanionUpgrade{name: name, item: item})
	}

	if err := revalidatePreparedCompanionPlan(prepared, amqDest, homebrewPrefixes, scoopRoots); err != nil {
		return err
	}
	for _, item := range prepared {
		result, err := replacePreparedUpgrade(item.name, item.item)
		if err != nil {
			return fmt.Errorf("%s: %w", item.name, err)
		}
		name := item.name
		if name == "amq-keepalive" {
			if result.scheduled {
				if err := writeStdoutLine("amq-keepalive: replacement scheduled; version cache is refreshed on the next successful check (best-effort)"); err != nil {
					return err
				}
			}
			if err := writeStdoutLine("amq-keepalive: a running supervisor picks up a strictly newer image on its next supervise pass (self-upgrade); if it was started with --no-self-upgrade, restart it through its service manager (on macOS use 'amq-keepalive install-launchd'; on Linux use 'systemctl --user restart amq-keepalive.service')."); err != nil {
				return err
			}
		}
	}
	return nil
}

// revalidatePreparedCompanionPlan binds every prepared replacement to the
// target inspected during discovery. The checks are pathname-based and do not
// claim to close races that would require directory descriptors or no-follow
// replacement primitives.
func revalidatePreparedCompanionPlan(prepared []preparedCompanionUpgrade, amqDest string, homebrewPrefixes []string, scoopRoots []update.ScoopInstallRoot) error {
	var amqInfo os.FileInfo
	if amqDest != "" {
		info, err := os.Stat(amqDest)
		if err != nil {
			return fmt.Errorf("cannot inspect amq destination %s before companion replacement: %w", amqDest, err)
		}
		amqInfo = info
	}
	for index := range prepared {
		planned := prepared[index].item.plannedTarget
		if planned == nil {
			return fmt.Errorf("companion %s has no planned target; remove or repoint it", prepared[index].name)
		}
		current, found, err := inspectCompanionCandidate(
			planned.name,
			planned.link,
			amqInfo,
			homebrewPrefixes,
			scoopRoots,
		)
		if err != nil {
			return fmt.Errorf("companion %s changed while preparing; %w", planned.link, err)
		}
		if !found {
			return fmt.Errorf("companion %s disappeared while preparing; remove or restore its target", planned.link)
		}
		if !os.SameFile(current.info, planned.info) {
			return fmt.Errorf("companion %s changed while preparing; remove or repoint it", planned.link)
		}
		if err := verifyCompanionBuild(current); err != nil {
			return fmt.Errorf("companion %s changed while preparing; %w", planned.link, err)
		}
		prepared[index].item.destPath = current.target
		prepared[index].item.plannedTarget = &current
	}
	return nil
}

func companionSearchDirsForUpgrade(rawPath, resolvedPath string) ([]string, error) {
	dirs := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	add := func(dir string) {
		if dir == "" {
			return
		}
		dir = filepath.Clean(dir)
		canonical := update.CanonicalPath(dir)
		if _, ok := seen[canonical]; ok {
			return
		}
		seen[canonical] = struct{}{}
		dirs = append(dirs, dir)
	}
	for _, path := range []string{rawPath, resolvedPath} {
		if path != "" {
			add(filepath.Dir(path))
		}
	}
	home, err := homeDirForUpgrade()
	if err != nil {
		return dirs, err
	}
	add(filepath.Join(home, ".local", "bin"))
	return dirs, nil
}

func discoverCompanionPlan(names []string, dirs []string, amqDest string, homebrewPrefixes []string, scoopRoots []update.ScoopInstallRoot) (map[string]companionTarget, error) {
	var amqInfo os.FileInfo
	var err error
	if amqDest != "" {
		amqInfo, err = os.Stat(amqDest)
		if err != nil {
			return nil, fmt.Errorf("cannot inspect amq destination %s: %w", amqDest, err)
		}
	}

	byName := make(map[string][]companionTarget, len(names))
	byCanonicalTarget := make(map[string]companionTarget)
	allTargets := make([]companionTarget, 0, len(names))
	for _, name := range names {
		for _, dir := range dirs {
			candidate := filepath.Join(dir, name)
			if runtime.GOOS == "windows" {
				candidate += ".exe"
			}
			item, found, err := inspectCompanionCandidate(name, candidate, amqInfo, homebrewPrefixes, scoopRoots)
			if err != nil {
				return nil, err
			}
			if !found {
				continue
			}

			canonicalTarget := update.CanonicalPath(item.target)
			if existing, ok := byCanonicalTarget[canonicalTarget]; ok {
				if existing.name != name {
					return nil, companionAliasError(item, existing)
				}
				continue
			}
			for _, existing := range allTargets {
				if !os.SameFile(existing.info, item.info) {
					continue
				}
				if existing.name != name {
					return nil, companionAliasError(item, existing)
				}
				return nil, companionHardlinkError(item, existing)
			}
			byCanonicalTarget[canonicalTarget] = item
			allTargets = append(allTargets, item)
			byName[name] = append(byName[name], item)
		}
	}
	for _, item := range allTargets {
		if err := verifyCompanionBuild(item); err != nil {
			return nil, err
		}
	}

	plan := make(map[string]companionTarget, len(byName))
	for _, name := range names {
		matches := byName[name]
		if len(matches) == 0 {
			continue
		}
		if len(matches) > 1 {
			targets := make([]string, 0, len(matches))
			for _, match := range matches {
				targets = append(targets, match.target)
			}
			return nil, fmt.Errorf("%s found at multiple distinct targets: %s; remove or consolidate one: %s", name, strings.Join(targets, ", "), strings.Join(targets, ", "))
		}
		plan[name] = matches[0]
	}
	return plan, nil
}

func findCompanionMatches(name string, dirs []string, amqDest string, homebrewPrefixes []string, scoopApps string) ([]string, error) {
	plan, err := discoverCompanionPlan([]string{name}, dirs, amqDest, homebrewPrefixes, legacyScoopRoots(scoopApps))
	if err != nil {
		return nil, err
	}
	item, ok := plan[name]
	if !ok {
		return nil, nil
	}
	return []string{item.target}, nil
}

func inspectCompanionCandidate(name, candidate string, amqInfo os.FileInfo, homebrewPrefixes []string, scoopRoots []update.ScoopInstallRoot) (companionTarget, bool, error) {
	linkInfo, err := os.Lstat(candidate)
	if errors.Is(err, os.ErrNotExist) {
		return companionTarget{}, false, nil
	}
	if err != nil {
		return companionTarget{}, false, fmt.Errorf("cannot inspect companion %s: %w; remove %s or fix its target", candidate, err, candidate)
	}

	var target string
	switch {
	case linkInfo.Mode().IsRegular():
		target, err = filepath.EvalSymlinks(candidate)
		if err != nil {
			return companionTarget{}, false, fmt.Errorf("cannot resolve companion %s: %w; remove %s or fix its target", candidate, err, candidate)
		}
	case linkInfo.Mode()&os.ModeSymlink != 0:
		target, err = filepath.EvalSymlinks(candidate)
		if err != nil {
			return companionTarget{}, false, fmt.Errorf("cannot resolve companion symlink %s: %w; remove %s or fix its target", candidate, err, candidate)
		}
	default:
		return companionTarget{}, false, fmt.Errorf("refusing companion %s: object is not a regular file or symlink; remove %s or fix its target", candidate, candidate)
	}

	target = filepath.Clean(target)
	targetInfo, statErr := os.Stat(target)
	if statErr != nil {
		return companionTarget{}, false, fmt.Errorf("cannot inspect companion target %s: %w; remove %s or fix its target", target, statErr, candidate)
	}
	if !targetInfo.Mode().IsRegular() {
		return companionTarget{}, false, fmt.Errorf("refusing companion %s: target %s is not a regular file; remove %s or fix its target", candidate, target, candidate)
	}
	if amqInfo != nil {
		if os.SameFile(targetInfo, amqInfo) {
			return companionTarget{}, false, fmt.Errorf("refusing companion %s: target %s is the amq executable; repair the %s symlink to point at a real %s binary", candidate, target, name, name)
		}
	}
	if isUnderProtectedRoot(target, homebrewPrefixes, scoopRoots) {
		return companionTarget{}, false, fmt.Errorf("refusing companion %s: target %s is package-managed; upgrade it with the package manager or repoint the companion link to a direct install", candidate, target)
	}
	return companionTarget{name: name, link: candidate, target: target, info: targetInfo}, true, nil
}

func verifyCompanionBuild(item companionTarget) error {
	info, buildErr := readBuildInfoForUpgrade(item.target)
	expected := expectedCompanionBuildPath(item.name)
	if buildErr != nil || info == nil {
		return fmt.Errorf("%s is not an %s build (found non-Go binary); remove or repoint it", item.target, item.name)
	}
	found := strings.TrimSpace(info.Path)
	if found == "" {
		found = "unknown"
	}
	if found != expected {
		return fmt.Errorf("%s is not an %s build (found %s); remove or repoint it", item.target, item.name, found)
	}
	return nil
}

func expectedCompanionBuildPath(name string) string {
	modulePath := update.ModulePath
	if info, ok := runtimedebug.ReadBuildInfo(); ok && info != nil {
		if mainPath := strings.TrimSpace(info.Main.Path); mainPath != "" && mainPath != "command-line-arguments" {
			modulePath = mainPath
		}
	}
	return modulePath + "/cmd/" + name
}

func companionAliasError(item, existing companionTarget) error {
	return fmt.Errorf("refusing companion %s: target %s aliases %s at %s; repoint %s to a dedicated %s binary or remove it", item.name, item.target, existing.name, existing.link, item.link, item.name)
}

func companionHardlinkError(item, existing companionTarget) error {
	return fmt.Errorf("refusing companion %s: distinct paths %s and %s are hardlinks to one file; repoint %s to a dedicated %s binary or remove it", item.name, existing.target, item.target, item.link, item.name)
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
		if kind := classifyInstallForUpgrade(candidate, ""); kind.IsScoop() {
			if kind == update.InstallScoopUnknown {
				return kind, ""
			}
			return kind, scoopManagerExecutable()
		}
	}
	return update.InstallDirect, ""
}

func matchingHomebrewManager(candidate, rawPath string, installations []homebrewInstallation) string {
	for _, installation := range installations {
		prefix := canonicalHomebrewPrefix(installation.prefix)
		if prefix == "" || !homebrewInstallationMatchesPath(candidate, rawPath, prefix) {
			continue
		}
		if manager, ok := verifiedHomebrewManager(installation); ok {
			return manager
		}
	}
	return ""
}

func homebrewInstallationMatchesPath(candidate, rawPath, prefix string) bool {
	prefix = canonicalHomebrewPrefix(prefix)
	if prefix == "" {
		return false
	}
	cellar := filepath.Join(prefix, "Cellar")
	for _, path := range []string{candidate, rawPath} {
		if path == "" {
			continue
		}
		canonical := update.CanonicalPath(path)
		if pathWithinDirectory(canonical, cellar) ||
			(pathWithinDirectory(filepath.Clean(path), filepath.Join(prefix, "bin")) && isSymlinkToHomebrewCellar(path, cellar)) {
			return true
		}
	}
	return false
}

func homebrewManagerPath(installation homebrewInstallation) string {
	if installation.executable != "" {
		return filepath.Clean(installation.executable)
	}
	return filepath.Join(canonicalHomebrewPrefix(installation.prefix), "bin", "brew")
}

func verifiedHomebrewManager(installation homebrewInstallation) (string, bool) {
	prefix := canonicalHomebrewPrefix(installation.prefix)
	manager := homebrewManagerPath(installation)
	if prefix == "" || manager == "" || !homebrewBrewExistsForUpgrade(manager) {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), homebrewDetectionTimeout)
	output, err := runBrewPrefixForHomebrewUpgrade(ctx, manager)
	cancel()
	if err != nil {
		return "", false
	}
	reported := cleanHomebrewPrefix(strings.TrimSpace(string(output)))
	if reported == "" || canonicalHomebrewPrefix(reported) != prefix {
		return "", false
	}
	return manager, true
}

func refuseHomebrewWithoutMatchingManager(path, resolved string, installations []homebrewInstallation) error {
	for _, installation := range installations {
		prefix := canonicalHomebrewPrefix(installation.prefix)
		if prefix == "" || !homebrewInstallationMatchesPath(resolved, path, prefix) {
			continue
		}
		manager := homebrewManagerPath(installation)
		location := resolved
		if location == "" {
			location = path
		}
		if !homebrewBrewExistsForUpgrade(manager) {
			if cellarPrefix := homebrewCellarPrefix(location); cellarPrefix != "" {
				return fmt.Errorf("amq lives in a Homebrew Cellar at %s but no brew executable was found at %s; run %s upgrade amq from that installation, or reinstall amq under ~/.local/bin for direct upgrades", filepath.Clean(location), manager, manager)
			}
			return fmt.Errorf("amq is installed in Homebrew prefix %s but no brew executable was found at %s; repair that installation or reinstall amq under ~/.local/bin for direct upgrades", prefix, manager)
		}
		return fmt.Errorf("amq is installed in a Homebrew Cellar at %s, but %s does not report prefix %s; repair that Homebrew installation or reinstall amq under ~/.local/bin for direct upgrades", filepath.Clean(location), manager, prefix)
	}
	if len(installations) > 0 {
		manager := homebrewManagerPath(installations[0])
		if !homebrewBrewExistsForUpgrade(manager) {
			return fmt.Errorf("amq is installed via Homebrew but no brew executable was found at %s; run %s upgrade amq from that installation, or reinstall amq under ~/.local/bin for direct upgrades", manager, manager)
		}
		return fmt.Errorf("amq is installed via Homebrew, but %s does not report its installation prefix; repair that Homebrew installation or reinstall amq under ~/.local/bin for direct upgrades", manager)
	}
	return fmt.Errorf("amq is installed via Homebrew but no matching brew executable was found; repair that Homebrew installation or reinstall amq under ~/.local/bin for direct upgrades")
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

func refuseUnknownScoopInstall(path, resolved string) error {
	location := filepath.Clean(resolved)
	if location == "." || location == "" {
		location = filepath.Clean(path)
	}
	return fmt.Errorf("amq is in an unrecognized Scoop apps directory at %s; set SCOOP or SCOOP_GLOBAL to that Scoop root, or reinstall amq under a direct path", location)
}

func legacyScoopRoots(path string) []update.ScoopInstallRoot {
	if path == "" {
		return nil
	}
	return []update.ScoopInstallRoot{{Path: path, Scope: update.ScoopScopeUser}}
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

func addDerivedHomebrewInstallations(path, resolved string, installations []homebrewInstallation) ([]homebrewInstallation, error) {
	derived := derivedHomebrewPrefixes(path, resolved)
	if len(derived) == 0 {
		return installations, nil
	}

	result := append([]homebrewInstallation(nil), installations...)
	known := make(map[string]struct{}, len(result)+len(derived))
	for _, installation := range result {
		if prefix := canonicalHomebrewPrefix(installation.prefix); prefix != "" {
			known[prefix] = struct{}{}
		}
	}
	for _, candidate := range derived {
		prefix := canonicalHomebrewPrefix(candidate.prefix)
		if prefix == "" {
			continue
		}
		brewPath := filepath.Join(prefix, "bin", "brew")
		if candidate.fromCellar && !homebrewBrewExistsForUpgrade(brewPath) {
			cellarPath := resolved
			if cellarPath == "" {
				cellarPath = path
			}
			return nil, fmt.Errorf("amq lives in a Homebrew Cellar at %s but no brew executable was found at %s; run %s upgrade amq from that installation, or reinstall amq under ~/.local/bin for direct upgrades", filepath.Clean(cellarPath), brewPath, brewPath)
		}
		if _, ok := known[prefix]; ok {
			continue
		}
		known[prefix] = struct{}{}
		result = append(result, homebrewInstallation{prefix: prefix, executable: brewPath})
	}
	return result, nil
}

func derivedHomebrewPrefixes(path, resolved string) []derivedHomebrewPrefix {
	result := make([]derivedHomebrewPrefix, 0, 2)
	seen := make(map[string]struct{}, 2)
	add := func(prefix string, fromCellar bool) {
		prefix = canonicalHomebrewPrefix(prefix)
		if prefix == "" {
			return
		}
		if _, exists := seen[prefix]; exists {
			return
		}
		seen[prefix] = struct{}{}
		result = append(result, derivedHomebrewPrefix{prefix: prefix, fromCellar: fromCellar})
	}

	for _, candidate := range []string{resolved} {
		if prefix := homebrewCellarPrefix(candidate); prefix != "" {
			add(prefix, true)
		}
	}

	rawPath := filepath.Clean(path)
	if filepath.Base(filepath.Dir(rawPath)) == "bin" {
		prefix := filepath.Dir(filepath.Dir(rawPath))
		if homebrewCellarExistsForUpgrade(filepath.Join(prefix, "Cellar")) {
			add(prefix, false)
		}
	}
	return result
}

func homebrewCellarPrefix(path string) string {
	if path == "" {
		return ""
	}
	clean := filepath.Clean(path)
	marker := string(filepath.Separator) + "Cellar" + string(filepath.Separator)
	index := strings.Index(clean, marker)
	if index <= 0 {
		return ""
	}
	return clean[:index]
}

func detectHomebrewInstallations() []homebrewInstallation {
	installations := make([]homebrewInstallation, 0, len(wellKnownHomebrewPrefixes)+3)
	seen := make(map[string]struct{})
	add := func(prefix, executable string, evidenced bool) {
		if !evidenced {
			return
		}
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
			add(strings.TrimSpace(string(output)), brewPath, true)
		}
	}
	if prefix := cleanHomebrewPrefix(os.Getenv("HOMEBREW_PREFIX")); prefix != "" {
		add(prefix, filepath.Join(prefix, "bin", "brew"), true)
	}
	for _, prefix := range wellKnownHomebrewPrefixes {
		brewPath := filepath.Join(prefix, "bin", "brew")
		add(prefix, brewPath, homebrewBrewExistsForUpgrade(brewPath) || homebrewCellarExistsForUpgrade(filepath.Join(prefix, "Cellar")))
	}
	if home, err := homeDirForUpgrade(); err == nil {
		prefix := filepath.Join(home, ".linuxbrew")
		brewPath := filepath.Join(prefix, "bin", "brew")
		add(prefix, brewPath, homebrewBrewExistsForUpgrade(brewPath) || homebrewCellarExistsForUpgrade(filepath.Join(prefix, "Cellar")))
	}
	return installations
}

func canonicalHomebrewPrefix(prefix string) string {
	return update.CanonicalPath(cleanHomebrewPrefix(prefix))
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
	var err error
	installations, err = addDerivedHomebrewInstallations(path, resolved, installations)
	if err != nil {
		return "", err
	}
	return selectUpgradeDestinationWithScoopRoots(
		path,
		resolved,
		writable,
		homebrewPrefixesFromInstallations(installations),
		update.ScoopInstallRoots(),
	)
}

func skipUnavailableCompanionsOnWindows(all *bool, goos string) error {
	if all == nil || !*all || goos != "windows" {
		return nil
	}
	if err := writeStdoutLine("--all: companion binaries are not published for Windows; skipping"); err != nil {
		return err
	}
	*all = false
	return nil
}

func selectUpgradeDestinationWithRoots(path, resolved string, writable func(string) error, homebrewPrefixes []string, scoopApps string) (string, error) {
	return selectUpgradeDestinationWithScoopRoots(
		path,
		resolved,
		writable,
		homebrewPrefixes,
		legacyScoopRoots(scoopApps),
	)
}

func selectUpgradeDestinationWithScoopRoots(path, resolved string, writable func(string) error, homebrewPrefixes []string, scoopRoots []update.ScoopInstallRoot) (string, error) {
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
		if err := refusePackageManagedDestinationWithScoopRoots(candidate, homebrewPrefixes, scoopRoots); err != nil {
			return "", err
		}
	}
	if err := writable(destPath); err != nil {
		return "", fmt.Errorf("cannot write the amq install location %s: %w", destPath, err)
	}
	return destPath, nil
}

func refusePackageManagedDestination(path string, homebrewPrefixes []string, scoopApps string) error {
	return refusePackageManagedDestinationWithScoopRoots(path, homebrewPrefixes, legacyScoopRoots(scoopApps))
}

func refusePackageManagedDestinationWithScoopRoots(path string, homebrewPrefixes []string, scoopRoots []update.ScoopInstallRoot) error {
	path = filepath.Clean(path)
	canonical := update.CanonicalPath(path)
	for _, prefix := range homebrewPrefixes {
		prefix = canonicalHomebrewPrefix(prefix)
		if prefix == "" {
			continue
		}
		cellar := filepath.Join(prefix, "Cellar")
		// This is intentionally independent from install classification. A path
		// race must not turn a package-managed replacement into a direct install;
		// executable-derived prefixes cover prefixes missed during discovery.
		if pathWithinDirectory(canonical, cellar) {
			return fmt.Errorf("amq is installed via Homebrew; run brew update && brew upgrade amq")
		}
		if pathWithinDirectory(canonical, filepath.Join(prefix, "bin")) {
			info, err := os.Lstat(path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return fmt.Errorf("cannot inspect possible Homebrew destination %s: %w; remove %s or fix its target", path, err, path)
			}
			switch {
			case info.Mode().IsRegular():
				return fmt.Errorf("amq is in an evidenced Homebrew prefix but is not a managed symlink; brew reinstall amq (if Homebrew owns it) or move the binary to ~/.local/bin and rerun amq upgrade (if you installed it manually)")
			case info.Mode()&os.ModeSymlink != 0:
				resolved, resolveErr := filepath.EvalSymlinks(path)
				if resolveErr != nil {
					return fmt.Errorf("amq is in a Homebrew prefix but its symlink cannot be resolved; remove %s or fix its target: %w", path, resolveErr)
				}
				if pathWithinDirectory(filepath.Clean(resolved), cellar) {
					return fmt.Errorf("amq is installed via Homebrew; run brew update && brew upgrade amq")
				}
			default:
				return fmt.Errorf("refusing possible Homebrew destination %s; remove %s or fix its target", path, path)
			}
		}
	}
	for _, scope := range []update.ScoopScope{update.ScoopScopeGlobal, update.ScoopScopeUser} {
		for _, root := range scoopRoots {
			if root.Scope != scope || root.Path == "" || !pathWithinDirectory(canonical, update.CanonicalPath(root.Path)) {
				continue
			}
			if scope == update.ScoopScopeGlobal {
				return fmt.Errorf("amq is installed via Scoop (global scope); run scoop update -g amq")
			}
			return fmt.Errorf("amq is installed via Scoop (user scope); run scoop update amq")
		}
	}
	if update.IsScoopShapedPath(path) || update.IsScoopShapedPath(canonical) {
		return fmt.Errorf("amq is in an unrecognized Scoop apps directory at %s; set SCOOP or SCOOP_GLOBAL to that Scoop root, or reinstall amq under a direct path", path)
	}
	return nil
}

func isUnderProtectedRoot(path string, homebrewPrefixes []string, scoopRoots []update.ScoopInstallRoot) bool {
	canonical := update.CanonicalPath(path)
	for _, prefix := range homebrewPrefixes {
		prefix = canonicalHomebrewPrefix(prefix)
		if prefix != "" && (pathWithinDirectory(canonical, filepath.Join(prefix, "Cellar")) ||
			pathWithinDirectory(canonical, filepath.Join(prefix, "bin"))) {
			return true
		}
	}
	for _, root := range scoopRoots {
		if root.Path != "" && pathWithinDirectory(canonical, update.CanonicalPath(root.Path)) {
			return true
		}
	}
	return update.IsScoopShapedPath(path) || update.IsScoopShapedPath(canonical)
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
		brewPath := filepath.Join(prefix, "bin", "brew")
		if homebrewBrewExistsForUpgrade(brewPath) || homebrewCellarExistsForUpgrade(filepath.Join(prefix, "Cellar")) {
			return filepath.Clean(prefix)
		}
	}
	// User-local Linuxbrew installs under ~/.linuxbrew (not a fixed absolute
	// path, so it is checked separately from the well-known prefix list).
	if home, err := homeDirForUpgrade(); err == nil {
		userLinuxbrew := filepath.Join(home, ".linuxbrew")
		brewPath := filepath.Join(userLinuxbrew, "bin", "brew")
		if homebrewBrewExistsForUpgrade(brewPath) || homebrewCellarExistsForUpgrade(filepath.Join(userLinuxbrew, "Cellar")) {
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
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func homebrewCellarExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
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
	path = update.CanonicalPath(path)
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
