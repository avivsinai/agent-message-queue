package cli

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/update"
)

func runUpgrade(args []string, currentVersion string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	usage := usageWithFlags(fs, "amq upgrade", "Downloads and installs the latest amq release from GitHub")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := &http.Client{Timeout: 30 * time.Second}

	latestTag, err := update.FetchLatestTag(ctx, client)
	if err != nil {
		return err
	}
	latest := update.NormalizeVersion(latestTag)
	if latest == "" {
		return fmt.Errorf("invalid latest version: %q", latestTag)
	}

	if cmp, ok := update.CompareVersions(currentVersion, latest); ok && cmp >= 0 {
		return writeStdoutLine(fmt.Sprintf("amq is already up to date (%s)", latest))
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

	path, resolved, err := update.ExecutablePath()
	if err != nil {
		return err
	}
	destPath := resolved
	if destPath == "" {
		destPath = path
	}
	if destPath == "" {
		return fmt.Errorf("unable to resolve executable path")
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
		return writeStdoutLine("Upgrade scheduled; it will complete after this process exits.")
	}
	return writeStdoutLine("Upgrade complete.")
}
