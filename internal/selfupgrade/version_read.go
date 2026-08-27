package selfupgrade

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"strings"
)

var errEmbeddedVersionUnavailable = errors.New("embedded version is unavailable")

// ReadEmbeddedVersion reads the version from Go build metadata without running
// the candidate. A truncated or non-Go image has no trustworthy version and
// therefore returns an error so callers defer the upgrade.
func ReadEmbeddedVersion(path string) (string, error) {
	image, err := openImageMetadataFile(path)
	if err != nil {
		return "", fmt.Errorf("open candidate build info: %w", err)
	}
	defer func() { _ = image.Close() }()
	stat, err := image.Stat()
	if err != nil {
		return "", fmt.Errorf("stat candidate build info: %w", err)
	}
	if !stat.Mode().IsRegular() {
		return "", fmt.Errorf("candidate build info is not a regular file")
	}
	info, err := buildinfo.Read(image)
	if err != nil {
		return "", fmt.Errorf("read candidate build info: %w", err)
	}
	version, ok := embeddedVersionFromBuildInfo(info)
	if !ok {
		return "", fmt.Errorf("%w for %s", errEmbeddedVersionUnavailable, path)
	}
	if _, ok := parseSemanticVersion(version); !ok {
		return "", fmt.Errorf("%w for %s", errEmbeddedVersionUnavailable, path)
	}
	return version, nil
}

func embeddedVersionFromBuildInfo(info *buildinfo.BuildInfo) (string, bool) {
	if info == nil {
		return "", false
	}
	for _, setting := range info.Settings {
		if setting.Key != "-ldflags" {
			continue
		}
		if version, ok := embeddedVersionFromLDFlags(setting.Value); ok {
			return version, true
		}
	}
	return "", false
}

func embeddedVersionFromLDFlags(raw string) (string, bool) {
	fields := strings.Fields(raw)
	for index, field := range fields {
		var value string
		switch {
		case field == "-X" && index+1 < len(fields):
			value = fields[index+1]
		case strings.HasPrefix(field, "-Xmain.version="):
			value = strings.TrimPrefix(field, "-X")
		case strings.HasPrefix(field, "-X=main.version="):
			value = strings.TrimPrefix(field, "-X=")
		default:
			continue
		}
		if strings.HasPrefix(value, "main.version=") {
			version := strings.TrimPrefix(value, "main.version=")
			if version != "" && !strings.ContainsAny(version, "\x00\r\n\t ") {
				return version, true
			}
		}
	}
	return "", false
}
