package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	RepoOwner            = "avivsinai"
	RepoName             = "agent-message-queue"
	RepoSlug             = RepoOwner + "/" + RepoName
	BinaryName           = "amq"
	ChecksumsFilename    = "checksums.txt"
	DefaultCheckInterval = 24 * time.Hour
	EnvNoUpdateCheck     = "AMQ_NO_UPDATE_CHECK"
)

var (
	ErrUnsupportedOS   = errors.New("unsupported operating system")
	ErrUnsupportedArch = errors.New("unsupported architecture")
)

type Cache struct {
	CheckedAt     time.Time `json:"checked_at"`
	LatestVersion string    `json:"latest_version"`
}

type Notifier struct {
	CurrentVersion string
	NoCheck        bool
	Stderr         io.Writer
	Now            func() time.Time
	Client         *http.Client
	CachePath      string
	CheckInterval  time.Duration
}

func (n Notifier) Start(ctx context.Context) {
	if n.NoCheck {
		return
	}
	current := normalizeVersion(n.CurrentVersion)
	if current == "" {
		return
	}
	if _, ok := parseSemver(current); !ok {
		return
	}

	cachePath, err := n.cachePath()
	if err != nil {
		return
	}
	cache, err := LoadCache(cachePath)
	hinted := false
	if err == nil && cache != nil {
		if IsUpdateAvailable(current, cache.LatestVersion) {
			_ = writeUpdateHint(n.stderr(), current, cache.LatestVersion)
			hinted = true
		}
	}

	if cache == nil || n.now().Sub(cache.CheckedAt) >= n.interval() {
		go func(alreadyHinted bool) {
			refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			latest, err := FetchLatestTag(refreshCtx, n.client())
			if err != nil {
				return
			}
			latest = normalizeVersion(latest)
			if latest == "" {
				return
			}
			if !alreadyHinted && IsUpdateAvailable(current, latest) {
				_ = writeUpdateHint(n.stderr(), current, latest)
			}
			_ = SaveCache(cachePath, &Cache{
				CheckedAt:     n.now().UTC(),
				LatestVersion: latest,
			})
		}(hinted)
	}
}

func IsUpdateAvailable(current, latest string) bool {
	cmp, ok := CompareVersions(current, latest)
	if !ok {
		return false
	}
	return cmp < 0
}

func CompareVersions(current, latest string) (int, bool) {
	current = normalizeVersion(current)
	latest = normalizeVersion(latest)
	if current == "" || latest == "" {
		return 0, false
	}
	c, ok := parseSemver(current)
	if !ok {
		return 0, false
	}
	l, ok := parseSemver(latest)
	if !ok {
		return 0, false
	}
	if c.Major != l.Major {
		return compareInts(c.Major, l.Major), true
	}
	if c.Minor != l.Minor {
		return compareInts(c.Minor, l.Minor), true
	}
	return compareInts(c.Patch, l.Patch), true
}

func compareInts(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

type semver struct {
	Major int
	Minor int
	Patch int
}

func parseSemver(version string) (semver, bool) {
	version = strings.TrimSpace(version)
	version = strings.TrimPrefix(version, "v")
	if version == "" {
		return semver{}, false
	}
	if cut := strings.IndexAny(version, "+-"); cut >= 0 {
		version = version[:cut]
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	major, ok := parsePositiveInt(parts[0])
	if !ok {
		return semver{}, false
	}
	minor, ok := parsePositiveInt(parts[1])
	if !ok {
		return semver{}, false
	}
	patch, ok := parsePositiveInt(parts[2])
	if !ok {
		return semver{}, false
	}
	return semver{Major: major, Minor: minor, Patch: patch}, true
}

func parsePositiveInt(raw string) (int, bool) {
	if raw == "" {
		return 0, false
	}
	value := 0
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, false
		}
		value = value*10 + int(r-'0')
	}
	return value, true
}

func normalizeVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return ""
	}
	if strings.HasPrefix(version, "v") {
		return version
	}
	if version[0] >= '0' && version[0] <= '9' {
		return "v" + version
	}
	return version
}

func NormalizeVersion(version string) string {
	return normalizeVersion(version)
}

func AssetName(tag, goos, goarch string) (string, error) {
	version := strings.TrimSpace(strings.TrimPrefix(tag, "v"))
	if version == "" {
		return "", errors.New("invalid release tag")
	}
	osName, err := normalizeOS(goos)
	if err != nil {
		return "", err
	}
	archName, err := normalizeArch(goarch)
	if err != nil {
		return "", err
	}
	ext := "tar.gz"
	if osName == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s.%s", BinaryName, version, osName, archName, ext), nil
}

func normalizeOS(goos string) (string, error) {
	switch goos {
	case "darwin", "linux", "windows":
		return goos, nil
	default:
		return "", ErrUnsupportedOS
	}
}

func normalizeArch(goarch string) (string, error) {
	switch goarch {
	case "amd64", "arm64":
		return goarch, nil
	default:
		return "", ErrUnsupportedArch
	}
}

func ExecutablePath() (string, string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	path = filepath.Clean(path)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	resolved := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		resolved = real
	}
	return path, resolved, nil
}

func ExtractBinaryFromTarGz(archivePath, destDir string) (string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	gz, err := gzip.NewReader(file)
	if err != nil {
		return "", err
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if hdr == nil {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.Base(hdr.Name)
		if name != BinaryName && name != BinaryName+".exe" {
			continue
		}
		outPath := filepath.Join(destDir, name)
		out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0700)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			return "", err
		}
		if err := out.Chmod(0755); err != nil {
			_ = out.Close()
			return "", err
		}
		if err := out.Sync(); err != nil {
			_ = out.Close()
			return "", err
		}
		if err := out.Close(); err != nil {
			return "", err
		}
		return outPath, nil
	}
	return "", errors.New("binary not found in archive")
}

func ExtractBinaryFromZip(archivePath, destDir string) (string, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = reader.Close() }()

	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		name := filepath.Base(file.Name)
		if name != BinaryName && name != BinaryName+".exe" {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return "", err
		}
		outPath := filepath.Join(destDir, name)
		out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0700)
		if err != nil {
			_ = rc.Close()
			return "", err
		}
		if _, err := io.Copy(out, rc); err != nil {
			_ = rc.Close()
			_ = out.Close()
			return "", err
		}
		if err := rc.Close(); err != nil {
			_ = out.Close()
			return "", err
		}
		if err := out.Chmod(0755); err != nil {
			_ = out.Close()
			return "", err
		}
		if err := out.Sync(); err != nil {
			_ = out.Close()
			return "", err
		}
		if err := out.Close(); err != nil {
			return "", err
		}
		return outPath, nil
	}
	return "", errors.New("binary not found in archive")
}

func ReplaceBinary(srcPath, destPath string) (bool, error) {
	dir := filepath.Dir(destPath)
	src, err := os.Open(srcPath)
	if err != nil {
		return false, err
	}
	defer func() { _ = src.Close() }()

	tmp, err := os.CreateTemp(dir, ".amq-update-*")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0755); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		if runtime.GOOS == "windows" {
			if err := scheduleWindowsReplace(tmpPath, destPath); err != nil {
				return false, err
			}
			cleanup = false
			_ = fsyncDir(dir)
			return true, nil
		}
		return false, err
	}
	_ = fsyncDir(dir)
	return false, nil
}

func scheduleWindowsReplace(tmpPath, destPath string) error {
	command := fmt.Sprintf("ping 127.0.0.1 -n 2 >NUL & move /Y %q %q", tmpPath, destPath)
	cmd := exec.Command("cmd.exe", "/C", command)
	return cmd.Start()
}

func VerifySHA256(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(actual, strings.TrimSpace(expected)) {
		return fmt.Errorf("checksum mismatch: expected %s got %s", expected, actual)
	}
	return nil
}

func ParseChecksums(data []byte) (map[string]string, error) {
	checksums := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sum := fields[0]
		name := fields[len(fields)-1]
		checksums[name] = sum
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return checksums, nil
}

func FetchLatestTag(ctx context.Context, client *http.Client) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", RepoSlug)
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := fetchJSON(ctx, client, url, &release); err != nil {
		return "", err
	}
	if strings.TrimSpace(release.TagName) == "" {
		return "", errors.New("latest release missing tag_name")
	}
	return release.TagName, nil
}

func DownloadReleaseAsset(ctx context.Context, client *http.Client, tag, assetName, destPath string) error {
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", RepoSlug, tag, assetName)
	resp, err := doRequest(ctx, client, url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return fmt.Errorf("download failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, resp.Body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func FetchChecksums(ctx context.Context, client *http.Client, tag string) (map[string]string, error) {
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", RepoSlug, tag, ChecksumsFilename)
	resp, err := doRequest(ctx, client, url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("download checksums failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return nil, err
	}
	return ParseChecksums(data)
}

func LoadCache(path string) (*Cache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cache Cache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, err
	}
	return &cache, nil
}

func SaveCache(path string, cache *Cache) error {
	if cache == nil {
		return errors.New("cache is nil")
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "update-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	_ = fsyncDir(dir)
	return nil
}

func DefaultCachePath() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		cacheDir = filepath.Join(home, ".cache")
	}
	return filepath.Join(cacheDir, "amq", "update.json"), nil
}

func writeUpdateHint(w io.Writer, current, latest string) error {
	_, err := fmt.Fprintf(w, "amq: update available (%s -> %s). Run 'amq upgrade' to install.\n", current, latest)
	return err
}

func (n Notifier) cachePath() (string, error) {
	if strings.TrimSpace(n.CachePath) != "" {
		return n.CachePath, nil
	}
	return DefaultCachePath()
}

func (n Notifier) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now().UTC()
}

func (n Notifier) stderr() io.Writer {
	if n.Stderr != nil {
		return n.Stderr
	}
	return os.Stderr
}

func (n Notifier) client() *http.Client {
	if n.Client != nil {
		return n.Client
	}
	return &http.Client{Timeout: 5 * time.Second}
}

func (n Notifier) interval() time.Duration {
	if n.CheckInterval > 0 {
		return n.CheckInterval
	}
	return DefaultCheckInterval
}

func fsyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func doRequest(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", BinaryName+"/"+runtime.Version())
	return client.Do(req)
}

func fetchJSON(ctx context.Context, client *http.Client, url string, target any) error {
	resp, err := doRequest(ctx, client, url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return fmt.Errorf("github api error: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

func IsNoUpdateCheckEnv() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(EnvNoUpdateCheck)))
	switch value {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
