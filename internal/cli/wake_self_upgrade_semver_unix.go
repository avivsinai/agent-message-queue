//go:build darwin || linux

package cli

import "github.com/avivsinai/agent-message-queue/internal/selfupgrade"

func wakeSelfUpgradeVersionStrictlyNewer(incumbent, candidate string) bool {
	return selfupgrade.VersionStrictlyNewer(incumbent, candidate)
}
