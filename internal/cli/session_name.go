package cli

import "strings"

const (
	sessionKindCanonical = "canonical"
	sessionKindLegacy    = "legacy_name"
)

// isCanonicalSessionName reports whether name is a porcelain session spelling.
// Resume will reuse this; it is the same grammar as validateSessionName.
func isCanonicalSessionName(name string) bool {
	return validateSessionName(name) == nil
}

// isSafeLegacySessionName reports whether name is a safe single-component
// legacy spelling (uppercase and/or dotted) that session list may report.
// Separators, `.`, and `..` are never legacy. Resume will reuse this helper
// without granting those spellings creation authority.
func isSafeLegacySessionName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	if isCanonicalSessionName(name) {
		return false
	}
	hasUpper, hasDot := false, false
	for _, r := range name {
		switch {
		case r == '.':
			hasDot = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		}
	}
	return hasUpper || hasDot
}
