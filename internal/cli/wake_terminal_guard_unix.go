//go:build darwin || linux

package cli

import "strings"

func authorizeTerminalWritePlatform(cfg *wakeConfig) bool {
	current := inspectWakeLock(cfg.root, cfg.me)
	if !current.Exists || current.Lock.Generation == "" {
		return false
	}
	if cfg.terminalGeneration == "" {
		cfg.terminalGeneration = current.Lock.Generation
		cfg.terminalTTY = current.Lock.TTY
	}
	if current.Lock.Generation != cfg.terminalGeneration ||
		current.Lock.TTY != cfg.terminalTTY {
		return false
	}
	switch wakeLockTerminalAttachment(current) {
	case wakeTerminalGone:
		// The retained terminal capability may outlive controlling-terminal
		// attachment. Preserve the legacy unknown-name fail-open policy; a
		// concrete path still fails because it can no longer match current.
		return !strings.HasPrefix(cfg.terminalTTY, "/dev/")
	case wakeTerminalAttached, wakeTerminalUndeterminable:
		// A concrete path must still name this process's current terminal.
		// Legacy "unknown" locks retain the prior fail-open behavior because
		// writes use the wake's already-bound controlling-terminal capability.
		return !strings.HasPrefix(cfg.terminalTTY, "/dev/") ||
			getWakeCurrentTTY() == cfg.terminalTTY
	default:
		return false
	}
}
