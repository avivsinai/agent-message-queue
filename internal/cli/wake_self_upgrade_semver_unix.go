//go:build darwin || linux

package cli

import "strings"

type wakeSelfUpgradeSemver struct {
	core       [3]string
	prerelease []string
}

func wakeSelfUpgradeVersionStrictlyNewer(incumbent, candidate string) bool {
	current, ok := parseWakeSelfUpgradeSemver(incumbent)
	if !ok {
		return false
	}
	next, ok := parseWakeSelfUpgradeSemver(candidate)
	if !ok {
		return false
	}
	return compareWakeSelfUpgradeSemver(current, next) < 0
}

func parseWakeSelfUpgradeSemver(raw string) (wakeSelfUpgradeSemver, bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "v")
	if raw == "" {
		return wakeSelfUpgradeSemver{}, false
	}
	if build := strings.IndexByte(raw, '+'); build >= 0 {
		if !validWakeSelfUpgradeIdentifiers(raw[build+1:], false) {
			return wakeSelfUpgradeSemver{}, false
		}
		raw = raw[:build]
	}
	var prerelease []string
	if dash := strings.IndexByte(raw, '-'); dash >= 0 {
		value := raw[dash+1:]
		if !validWakeSelfUpgradeIdentifiers(value, true) {
			return wakeSelfUpgradeSemver{}, false
		}
		prerelease = strings.Split(value, ".")
		raw = raw[:dash]
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return wakeSelfUpgradeSemver{}, false
	}
	var parsed wakeSelfUpgradeSemver
	for index, part := range parts {
		if !wakeSelfUpgradeNumericIdentifier(part) || (len(part) > 1 && part[0] == '0') {
			return wakeSelfUpgradeSemver{}, false
		}
		parsed.core[index] = part
	}
	parsed.prerelease = prerelease
	return parsed, true
}

func validWakeSelfUpgradeIdentifiers(raw string, prerelease bool) bool {
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
		if prerelease && wakeSelfUpgradeNumericIdentifier(identifier) &&
			len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func wakeSelfUpgradeNumericIdentifier(raw string) bool {
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

func compareWakeSelfUpgradeSemver(first, second wakeSelfUpgradeSemver) int {
	for index := range first.core {
		if compared := compareWakeSelfUpgradeNumeric(first.core[index], second.core[index]); compared != 0 {
			return compared
		}
	}
	if len(first.prerelease) == 0 || len(second.prerelease) == 0 {
		switch {
		case len(first.prerelease) == len(second.prerelease):
			return 0
		case len(first.prerelease) == 0:
			return 1
		default:
			return -1
		}
	}
	limit := min(len(first.prerelease), len(second.prerelease))
	for index := 0; index < limit; index++ {
		left, right := first.prerelease[index], second.prerelease[index]
		leftNumeric := wakeSelfUpgradeNumericIdentifier(left)
		rightNumeric := wakeSelfUpgradeNumericIdentifier(right)
		switch {
		case leftNumeric && rightNumeric:
			if compared := compareWakeSelfUpgradeNumeric(left, right); compared != 0 {
				return compared
			}
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		case left < right:
			return -1
		case left > right:
			return 1
		}
	}
	switch {
	case len(first.prerelease) < len(second.prerelease):
		return -1
	case len(first.prerelease) > len(second.prerelease):
		return 1
	default:
		return 0
	}
}

func compareWakeSelfUpgradeNumeric(first, second string) int {
	switch {
	case len(first) < len(second):
		return -1
	case len(first) > len(second):
		return 1
	case first < second:
		return -1
	case first > second:
		return 1
	default:
		return 0
	}
}
