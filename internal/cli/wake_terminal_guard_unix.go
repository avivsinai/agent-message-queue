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
	return !strings.HasPrefix(cfg.terminalTTY, "/dev/") ||
		getWakeCurrentTTY() == cfg.terminalTTY
}
