package selfupgrade

import "strings"

type semanticVersion struct {
	core       [3]string
	prerelease []string
}

func VersionStrictlyNewer(incumbent, candidate string) bool {
	current, ok := parseSemanticVersion(incumbent)
	if !ok {
		return false
	}
	next, ok := parseSemanticVersion(candidate)
	if !ok {
		return false
	}
	if len(current.prerelease) > 0 || len(next.prerelease) > 0 {
		// Git-describe suffixes are build snapshots, not ordered SemVer
		// prereleases. Require a strictly newer core whenever either exists.
		return compareSemanticCore(current, next) < 0
	}
	return compareSemanticVersion(current, next) < 0
}

func parseSemanticVersion(raw string) (semanticVersion, bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "v")
	if raw == "" {
		return semanticVersion{}, false
	}
	if build := strings.IndexByte(raw, '+'); build >= 0 {
		if !validIdentifiers(raw[build+1:], false) {
			return semanticVersion{}, false
		}
		raw = raw[:build]
	}
	var prerelease []string
	if dash := strings.IndexByte(raw, '-'); dash >= 0 {
		value := raw[dash+1:]
		if !validIdentifiers(value, true) {
			return semanticVersion{}, false
		}
		prerelease = strings.Split(value, ".")
		raw = raw[:dash]
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return semanticVersion{}, false
	}
	var parsed semanticVersion
	for index, part := range parts {
		if !numericIdentifier(part) || (len(part) > 1 && part[0] == '0') {
			return semanticVersion{}, false
		}
		parsed.core[index] = part
	}
	parsed.prerelease = prerelease
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

func compareSemanticVersion(first, second semanticVersion) int {
	if core := compareSemanticCore(first, second); core != 0 {
		return core
	}
	if len(first.prerelease) == 0 && len(second.prerelease) == 0 {
		return 0
	}
	if len(first.prerelease) == 0 {
		return 1
	}
	if len(second.prerelease) == 0 {
		return -1
	}
	for index := 0; index < len(first.prerelease) && index < len(second.prerelease); index++ {
		left := first.prerelease[index]
		right := second.prerelease[index]
		if left == right {
			continue
		}
		leftNumeric := numericIdentifier(left)
		rightNumeric := numericIdentifier(right)
		switch {
		case leftNumeric && rightNumeric:
			if len(left) != len(right) {
				if len(left) < len(right) {
					return -1
				}
				return 1
			}
			if left < right {
				return -1
			}
			return 1
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		case left < right:
			return -1
		default:
			return 1
		}
	}
	if len(first.prerelease) < len(second.prerelease) {
		return -1
	}
	if len(first.prerelease) > len(second.prerelease) {
		return 1
	}
	return 0
}

func compareSemanticCore(first, second semanticVersion) int {
	for index := range first.core {
		left := first.core[index]
		right := second.core[index]
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
