//go:build darwin || linux

package cli

import "strings"

func authorizeTerminalWritePlatform(cfg *wakeConfig) bool {
	allowed, _ := authorizeTerminalWritePlatformState(cfg)
	return allowed
}

func authorizeTerminalWritePlatformState(cfg *wakeConfig) (bool, error) {
	current := inspectWakeLock(cfg.root, cfg.me)
	if !current.Exists {
		if cfg.terminalGeneration != "" {
			return false, newWakeTerminalAuthorityLoss("wake generation disappeared")
		}
		return false, nil
	}
	if current.Lock.Generation == "" {
		// An empty or unreadable generation is not owner-identity evidence.
		// Park input without silencing attention while ownership is inconclusive.
		return false, nil
	}
	if cfg.terminalGeneration == "" {
		cfg.terminalGeneration = current.Lock.Generation
		cfg.terminalTTY = current.Lock.TTY
	}
	if current.Lock.Generation != cfg.terminalGeneration {
		// A readable different generation is positive replacement evidence even
		// when a newer lock schema is otherwise unverified. Mixed-version
		// upgrades must supersede the old narrator instead of preserving it.
		return false, newWakeTerminalAuthorityLoss("wake generation changed")
	}
	if current.Lock.TTY != cfg.terminalTTY {
		return false, newWakeTerminalAuthorityLoss("wake terminal changed")
	}
	switch wakeLockTerminalAttachment(current) {
	case wakeTerminalGone:
		// The retained terminal capability may outlive controlling-terminal
		// attachment. Preserve the legacy unknown-name fail-open policy; a
		// concrete path still fails because it can no longer match current.
		return !strings.HasPrefix(cfg.terminalTTY, "/dev/"), nil
	case wakeTerminalAttached, wakeTerminalUndeterminable:
		// A concrete path must still name this process's current terminal.
		// Legacy "unknown" locks retain the prior fail-open behavior because
		// writes use the wake's already-bound controlling-terminal capability.
		return !strings.HasPrefix(cfg.terminalTTY, "/dev/") ||
			getWakeCurrentTTY() == cfg.terminalTTY, nil
	default:
		return false, nil
	}
}
