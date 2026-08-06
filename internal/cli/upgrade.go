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
)

const homebrewDetectionTimeout = 2 * time.Second

var wellKnownHomebrewPrefixes = []string{
	"/opt/homebrew",
	"/usr/local",
	"/home/linuxbrew/.linuxbrew",
}

func runUpgrade(args []string, currentVersion string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	usage := usageWithFlags(fs, "amq upgrade", "Downloads and installs the latest amq release from GitHub")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	path, resolved, err := executablePathForUpgrade()
	if err != nil {
		return err
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
		return reportStaleWakesAfterUpgrade()
	}

	if err := writeStdoutLine("Upgrading to", latest, "..."); err != nil {
		return err
	}

	assetName, err := update.AssetName(latestTag, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp("", "amq-upgrade-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	archivePath := filepath.Join(tmpDir, assetName)
	if err := update.DownloadReleaseAsset(ctx, client, latestTag, assetName, archivePath); err != nil {
		return err
	}

	checksums, err := update.FetchChecksums(ctx, client, latestTag)
	if err != nil {
		return err
	}
	checksum, ok := checksums[assetName]
	if !ok {
		return fmt.Errorf("checksum entry not found for %s", assetName)
	}
	if err := update.VerifySHA256(archivePath, checksum); err != nil {
		return err
	}

	var binaryPath string
	if runtime.GOOS == "windows" {
		binaryPath, err = update.ExtractBinaryFromZip(archivePath, tmpDir)
	} else {
		binaryPath, err = update.ExtractBinaryFromTarGz(archivePath, tmpDir)
	}
	if err != nil {
		return err
	}

	scheduled, err := update.ReplaceBinary(binaryPath, destPath)
	if err != nil {
		return err
	}

	if cachePath, err := update.DefaultCachePath(); err == nil {
		_ = update.SaveCache(cachePath, &update.Cache{
			CheckedAt:     time.Now().UTC(),
			LatestVersion: latest,
		})
	}

	if scheduled {
		if err := writeStdoutLine("Upgrade scheduled; it will complete after this process exits."); err != nil {
			return err
		}
		return reportStaleWakesAfterUpgrade()
	}
	if err := writeStdoutLine("Upgrade complete."); err != nil {
		return err
	}
	return reportStaleWakesAfterUpgrade()
}

func selectUpgradeDestination(path, resolved string, writable func(string) error) (string, error) {
	destPath := resolved
	if destPath == "" {
		destPath = path
	}
	if destPath == "" {
		return "", fmt.Errorf("unable to resolve executable path")
	}
	destPath = filepath.Clean(destPath)

	if prefix := detectHomebrewPrefixForUpgrade(); prefix != "" &&
		(homebrewOwnsUpgradePath(prefix, path) || homebrewOwnsUpgradePath(prefix, destPath)) {
		if isSeveredHomebrewBinary(prefix, path) {
			return "", fmt.Errorf("amq is installed via Homebrew; run brew reinstall amq")
		}
		return "", fmt.Errorf("amq is installed via Homebrew; run brew update && brew upgrade amq")
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
