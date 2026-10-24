package usage

import (
	"strconv"
	"strings"
)

// The record formats the readers were checked against. Each range starts at
// the oldest version seen in the real records the readers were run on (for
// Codex, rollout sums also matched the harness's own per-thread totals) and
// ends before the next major version, which may change the format. A record outside its range, or without a version,
// is not counted: its spend is unknown, never a number read from a format
// nobody checked. The Codex guard covers rollouts only; the readers do not
// open state_5.sqlite.
var (
	claudeVersions = versionRange{min: [3]int{2, 1, 267}, below: [3]int{3, 0, 0}}
	codexVersions  = versionRange{min: [3]int{0, 114, 0}, below: [3]int{1, 0, 0}}
)

// piFormat is the pi session header version the reader understands.
const piFormat = 3

type versionRange struct{ min, below [3]int }

// has reports whether v (major.minor.patch, any pre-release or build suffix
// ignored) lies in the range.
func (r versionRange) has(v string) bool {
	n, ok := parseVersion(v)
	return ok && !less(n, r.min) && less(n, r.below)
}

func parseVersion(v string) ([3]int, bool) {
	var n [3]int
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return n, false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return n, false
		}
		n[i] = x
	}
	return n, true
}

func less(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// unsupported is why a record's spend is unknown.
func unsupported(harness, version string) string {
	if version == "" {
		version = "not recorded"
	}
	return harness + " version " + version + " not verified"
}
