package selfupgrade

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"strings"
)

var errEmbeddedVersionUnavailable = errors.New("embedded version is unavailable")

// ReadEmbeddedVersion reads the version from Go build metadata without running
// the candidate. An image whose build-info region cannot be read has no
// trustworthy version and therefore returns an error so callers defer.
func ReadEmbeddedVersion(path string) (string, error) {
	image, err := openImageMetadataFile(path)
	if err != nil {
		return "", fmt.Errorf("open candidate build info: %w", err)
	}
	defer func() { _ = image.Close() }()
	return readEmbeddedVersionFromOpenFile(image, path)
}

// ReadEmbeddedVersionFromOpenFile reads build metadata from an already opened
// image. Callers that also need image evidence should use this fd-bound form so
// the version and digest describe the same opened file.
func ReadEmbeddedVersionFromOpenFile(image *os.File) (string, error) {
	return readEmbeddedVersionFromOpenFile(image, "candidate build info")
}

func readEmbeddedVersionFromOpenFile(image *os.File, path string) (string, error) {
	if image == nil {
		return "", errors.New("read candidate build info: image file is missing")
	}
	stat, err := image.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if !stat.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	info, err := buildinfo.Read(image)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
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
	var version string
	var found bool
	for _, setting := range info.Settings {
		if setting.Key != "-ldflags" {
			continue
		}
		parsed, settingFound, ambiguous := parseEmbeddedVersionFromLDFlags(setting.Value)
		if ambiguous {
			return "", false
		}
		if settingFound {
			version = parsed
			found = true
		}
	}
	return version, found && version != ""
}

func embeddedVersionFromLDFlags(raw string) (string, bool) {
	version, found, ambiguous := parseEmbeddedVersionFromLDFlags(raw)
	return version, found && version != "" && !ambiguous
}

func parseEmbeddedVersionFromLDFlags(raw string) (string, bool, bool) {
	fields, ok := splitLinkerFlags(raw)
	if !ok {
		return "", false, true
	}
	var version string
	var found bool
	for index := 0; index < len(fields); index++ {
		field := fields[index]
		switch {
		case field == "-X":
			if index+1 >= len(fields) {
				return "", false, true
			}
			assignment := fields[index+1]
			index++
			target, value, valid := parseLinkerAssignment(assignment)
			if !valid {
				return "", false, true
			}
			if target == "main.version" {
				if !validEmbeddedVersionText(value) {
					return "", false, true
				}
				version = value
				found = true
			}
		case strings.HasPrefix(field, "-X="):
			target, value, valid := parseLinkerAssignment(strings.TrimPrefix(field, "-X="))
			if !valid {
				return "", false, true
			}
			if target == "main.version" {
				if !validEmbeddedVersionText(value) {
					return "", false, true
				}
				version = value
				found = true
			}
		case strings.HasPrefix(field, "-X"):
			target, value, valid := parseLinkerAssignment(strings.TrimPrefix(field, "-X"))
			if !valid {
				return "", false, true
			}
			if target == "main.version" {
				if !validEmbeddedVersionText(value) {
					return "", false, true
				}
				version = value
				found = true
			}
		default:
			// A non-flag token containing '=' is a per-package setting, not a
			// linker option. It can change which -X assignment is effective.
			if strings.ContainsRune(field, '=') && !strings.HasPrefix(field, "-") {
				return "", false, true
			}
		}
	}
	if !found {
		return "", false, false
	}
	return version, true, false
}

func parseLinkerAssignment(raw string) (string, string, bool) {
	equal := strings.IndexByte(raw, '=')
	if equal <= 0 {
		return "", "", false
	}
	target := raw[:equal]
	if !validLinkerSymbol(target) {
		return "", "", false
	}
	return target, raw[equal+1:], true
}

func validLinkerSymbol(raw string) bool {
	separator := strings.LastIndexByte(raw, '.')
	return separator > 0 && separator < len(raw)-1 &&
		!strings.ContainsAny(raw, "\x00\r\n\t '\"\\")
}

func validEmbeddedVersionText(raw string) bool {
	return raw == strings.TrimSpace(raw) && !strings.ContainsAny(raw, "\x00\r\n\t '\"\\")
}

func splitLinkerFlags(raw string) ([]string, bool) {
	// Keep this equivalent to cmd/internal/quoted.Split: quotes are accepted
	// only at token start, and their contents are not unescaped.
	var fields []string
	for len(raw) > 0 {
		for len(raw) > 0 && isLinkerFlagSpace(raw[0]) {
			raw = raw[1:]
		}
		if len(raw) == 0 {
			break
		}
		if raw[0] == '\'' || raw[0] == '"' {
			quote := raw[0]
			raw = raw[1:]
			end := strings.IndexByte(raw, quote)
			if end < 0 {
				return nil, false
			}
			fields = append(fields, raw[:end])
			raw = raw[end+1:]
			continue
		}
		end := 0
		for end < len(raw) && !isLinkerFlagSpace(raw[end]) {
			end++
		}
		fields = append(fields, raw[:end])
		raw = raw[end:]
	}
	return fields, true
}

func isLinkerFlagSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r'
}
