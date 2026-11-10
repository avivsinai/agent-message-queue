package selfupgrade

import "github.com/avivsinai/agent-message-queue/internal/semver"

func VersionStrictlyNewer(incumbent, candidate string) bool {
	current, ok := semver.Parse(incumbent)
	if !ok {
		return false
	}
	next, ok := semver.Parse(candidate)
	if !ok {
		return false
	}
	// Git-describe suffixes are build snapshots, not ordered SemVer
	// prereleases, so only a strictly newer core is newer.
	return semver.CompareCore(current, next) < 0
}
