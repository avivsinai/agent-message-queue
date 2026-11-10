// Package semver parses and compares the numeric core of semantic versions.
package semver

import "strings"

// Version is a parsed semantic version. Core holds the major, minor, and patch
// digits as written, without leading zeros.
type Version struct {
	Core       [3]string
	Prerelease []string
}

// Parse accepts an optional leading "v", a three-part numeric core without
// leading zeros, an optional prerelease, and optional build metadata.
func Parse(raw string) (Version, bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "v")
	if raw == "" {
		return Version{}, false
	}
	if build := strings.IndexByte(raw, '+'); build >= 0 {
		if !validIdentifiers(raw[build+1:], false) {
			return Version{}, false
		}
		raw = raw[:build]
	}
	var prerelease []string
	if dash := strings.IndexByte(raw, '-'); dash >= 0 {
		value := raw[dash+1:]
		if !validIdentifiers(value, true) {
			return Version{}, false
		}
		prerelease = strings.Split(value, ".")
		raw = raw[:dash]
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var parsed Version
	for index, part := range parts {
		if !numericIdentifier(part) || (len(part) > 1 && part[0] == '0') {
			return Version{}, false
		}
		parsed.Core[index] = part
	}
	parsed.Prerelease = prerelease
	return parsed, true
}

func validIdentifiers(raw string, prerelease bool) bool {
	if raw == "" {
		return false
	}
	for _, identifier := range strings.Split(raw, ".") {
		if identifier == "" {
			return false
		}
		for _, value := range identifier {
			if (value < '0' || value > '9') &&
				(value < 'A' || value > 'Z') &&
				(value < 'a' || value > 'z') && value != '-' {
				return false
			}
		}
		if prerelease && numericIdentifier(identifier) &&
			len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func numericIdentifier(raw string) bool {
	if raw == "" {
		return false
	}
	for _, value := range raw {
		if value < '0' || value > '9' {
			return false
		}
	}
	return true
}

// CompareCore orders two versions by major, minor, and patch only. It returns
// -1, 0, or 1. Prerelease and build metadata do not take part.
func CompareCore(first, second Version) int {
	for index := range first.Core {
		left := first.Core[index]
		right := second.Core[index]
		if len(left) != len(right) {
			if len(left) < len(right) {
				return -1
			}
			return 1
		}
		if left < right {
			return -1
		}
		if left > right {
			return 1
		}
	}
	return 0
}
